package sim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
)

// #2051 (ADR 2051): the merge train's trial state is persisted and evaluated per poll, so
// a daemon restart resumes the SAME trial or bisection position instead of rebuilding it.
// These scenarios run on the production (asynchronous) driver — mergeTrainEnv enables it —
// and use RestartEnv, which carries the persisted run records across the restart while
// discarding every byte of in-memory engine state.

// openTrialPRs lists the open merge-train trial PRs (head branch prefix) in env's repo.
func openTrialPRs(t *testing.T, env *Env) (numbers []int, heads []string) {
	t.Helper()
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	for _, pr := range prs {
		if pr.State == "open" && strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
			numbers = append(numbers, pr.Number)
			heads = append(heads, pr.HeadRefName)
		}
	}
	return numbers, heads
}

func mergedTrialPRs(t *testing.T, env *Env) []int {
	t.Helper()
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	var out []int
	for _, pr := range prs {
		if pr.Merged && strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
			out = append(out, pr.Number)
		}
	}
	return out
}

// pollUntilRun runs polls until cond holds for the engine's open runs and returns them.
func pollUntilRun(t *testing.T, env *Env, cond func([]engine.TrainRunPhase) bool, maxPolls int) []engine.TrainRunPhase {
	t.Helper()
	for i := 0; i < maxPolls; i++ {
		RunPoll(t, env)
		if ph := env.Engine.TrainRunPhases(); cond(ph) {
			return ph
		}
	}
	t.Fatalf("condition not met after %d polls: %+v", maxPolls, env.Engine.TrainRunPhases())
	return nil
}

// Acceptance 1: a restart mid-trial resumes the same trial PR and lands it.
func TestMergeTrainResume_RestartMidTrialResumesSameTrialAndLands(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	numA, _ := QueueMember(t, env, "resume-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "resume-b", map[string]string{"b.txt": "b\n"})

	// No verdict is seeded: the trial opens and sits waiting on CI.
	phases := pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool { return len(p) == 1 && p[0].TrialPR != 0 }, 10)
	if phases[0].Phase != "trial-ci" {
		t.Fatalf("phase = %q, want trial-ci", phases[0].Phase)
	}
	trialPRs, heads := openTrialPRs(t, env)
	if len(trialPRs) != 1 {
		t.Fatalf("want exactly one open trial PR before the restart, got %v", trialPRs)
	}
	if env.Engine.HasInFlightWorker() {
		t.Fatal("no worker goroutine may be held while the trial waits on CI")
	}
	files, _ := filepath.Glob(filepath.Join(env.TrainStateDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one persisted run record, got %v", files)
	}
	draftsBefore := draftPRCount(env)

	// The restart: everything in memory is gone. CI then goes green on the SAME trial.
	env2 := restartMergeTrainEnv(t, env)
	startTrialVerdictSeeder(t, env2, allGreenVerdict)

	WaitForProjectStatus(t, env2, numA, "Done", 30)
	WaitForProjectStatus(t, env2, numB, "Done", 10)

	if got := draftPRCount(env2); got != draftsBefore {
		t.Errorf("a restart mid-trial created %d new trial PR(s); it must resume the existing one", got-draftsBefore)
	}
	merged := mergedTrialPRs(t, env2)
	if len(merged) != 1 || merged[0] != trialPRs[0] {
		t.Errorf("merged trial PRs = %v, want exactly the pre-restart trial #%d (branch %s)", merged, trialPRs[0], heads[0])
	}
	if ph := env2.Engine.TrainRunPhases(); len(ph) != 0 {
		t.Errorf("run still open after landing: %+v", ph)
	}
	if left, _ := filepath.Glob(filepath.Join(env2.TrainStateDir, "*.json")); len(left) != 0 {
		t.Errorf("run record outlived its trial: %v", left)
	}
}

// Acceptance 2: a restart mid-bisection resumes at the same step — the trials still to run
// and the final landed/ejected members equal an uninterrupted run's.
func TestMergeTrainResume_RestartMidBisectionResumesAtSameStep(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, restart bool) (trials int, ejected int, survivorsDone bool) {
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})
		nums := make([]int, 3)
		files := []map[string]string{{"a.txt": "a\n"}, {"b.txt": "b\n"}, {"c.txt": "c\n"}}
		for i := range nums {
			nums[i], _ = QueueMember(t, env, "resume-bisect", files[i])
		}
		poisoned := nums[1] // the middle member: the 4-trial shape of mergetrain_bisect_bound_test.go
		stop := startTrialVerdictSeeder(t, env, poisonVerdict(poisoned))

		if restart {
			// Drive until a bisection half-trial is open and waiting, then restart there.
			pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool {
				return len(p) == 1 && p[0].Phase == "bisect" && p[0].Step == "bisect-trial"
			}, 40)
			stop()
			env = restartMergeTrainEnv(t, env)
			startTrialVerdictSeeder(t, env, poisonVerdict(poisoned))
		}

		WaitForProjectStatus(t, env, poisoned, "Queued", 40)
		for _, n := range []int{nums[0], nums[2]} {
			WaitForProjectStatus(t, env, n, "Done", 40)
		}
		if !hasCommentContaining(t, env, poisoned, "ejected") {
			t.Fatalf("#%d was not ejected as the isolated poisoner", poisoned)
		}
		return draftPRCount(env), 1, true
	}

	wantTrials, _, _ := run(t, false)
	gotTrials, _, ok := run(t, true)
	if !ok {
		t.Fatal("resumed run did not complete")
	}
	if gotTrials != wantTrials {
		t.Errorf("resumed run opened %d trial PRs in total, an uninterrupted run %d — the restart repeated or skipped a step", gotTrials, wantTrials)
	}
}

// Acceptance 3: a trial with no CI progress is surfaced by the per-poll evaluation within one
// poll of its deadline.
func TestMergeTrainResume_StuckTrialSurfacedWithinOnePollOfDeadline(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	numA, _ := QueueMember(t, env, "stuck-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "stuck-b", map[string]string{"b.txt": "b\n"})

	// No verdict is ever seeded: CI makes no progress.
	pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool { return len(p) == 1 && p[0].TrialPR != 0 }, 10)
	trialPRs, _ := openTrialPRs(t, env)
	if len(trialPRs) != 1 {
		t.Fatalf("want one open trial, got %v", trialPRs)
	}

	// Still inside its deadline: left alone.
	env.Clock.Advance(5 * time.Second)
	RunPoll(t, env)
	if len(env.Engine.TrainRunPhases()) != 1 {
		t.Fatal("a trial inside its deadline must be left alone")
	}

	// Past the deadline (cfg.CIBackstopTimeout is 10s): the very next poll surfaces it.
	env.Clock.Advance(10 * time.Second)
	RunPoll(t, env)
	if ph := env.Engine.TrainRunPhases(); len(ph) != 0 {
		t.Fatalf("one poll after the deadline the stuck trial must be surfaced and handled: %+v", ph)
	}
	if n := len(mergedTrialPRs(t, env)); n != 0 {
		t.Fatalf("a timed-out trial lands nothing, merged %d", n)
	}
	for _, n := range []int{numA, numB} {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d Status = %q, want it left in Queued", n, st)
		}
	}
	if branches, err := env.WM.ListTrainBranchesOnOrigin(); err != nil {
		t.Errorf("listing trial branches: %v", err)
	} else if len(branches) != 0 {
		t.Errorf("the stuck trial's branch must be cleaned up, still on origin: %v", branches)
	}
}

// FR-009: anything wrong with the persisted record falls back to today's behaviour — the
// partition forms fresh and lands — never wedges.
func TestMergeTrainResume_CorruptRecordFallsBackToFreshTrain(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	numA, _ := QueueMember(t, env, "corrupt-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "corrupt-b", map[string]string{"b.txt": "b\n"})

	pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool { return len(p) == 1 && p[0].TrialPR != 0 }, 10)
	files, _ := filepath.Glob(filepath.Join(env.TrainStateDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one persisted record, got %v", files)
	}
	if err := os.WriteFile(files[0], []byte(`{"version":1,"train_key":`), 0o600); err != nil {
		t.Fatal(err)
	}

	env2 := restartMergeTrainEnv(t, env)
	startTrialVerdictSeeder(t, env2, allGreenVerdict)
	WaitForProjectStatus(t, env2, numA, "Done", 60)
	WaitForProjectStatus(t, env2, numB, "Done", 10)
	if _, err := os.Stat(files[0] + ".corrupt"); err != nil {
		t.Errorf("the unreadable record must be quarantined: %v", err)
	}
}

// FR-008/FR-009: a member paused while the daemon was down must not be landed from stale
// state; the record is discarded and the partition re-forms without it.
func TestMergeTrainResume_MemberPausedWhileDownIsNotLandedFromStaleState(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	numA, _ := QueueMember(t, env, "paused-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "paused-b", map[string]string{"b.txt": "b\n"})

	pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool { return len(p) == 1 && p[0].TrialPR != 0 }, 10)

	env2 := restartMergeTrainEnv(t, env)
	if err := env2.Sim.AddLabelToIssue(env2.Owner, env2.Repo, numB, "fabrik:paused"); err != nil {
		t.Fatalf("pausing #%d: %v", numB, err)
	}
	startTrialVerdictSeeder(t, env2, allGreenVerdict)

	WaitForProjectStatus(t, env2, numA, "Done", 60)
	if st := projectItem(t, env2, numB).Status; st != "Queued" {
		t.Errorf("paused member #%d Status = %q, want it left in Queued", numB, st)
	}
}
