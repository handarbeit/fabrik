package sim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// #2297 (ADR 2297): marking a trial PR ready fires pull_request: ready_for_review, and for a
// short window GitHub reports mergeable_state "blocked". The landing waits that window out on
// the SAME PR instead of treating the refused merge as a failed landing that deletes the green
// trial and rebuilds it. simgh's ReadyForReviewStep scripts the window (see FIDELITY.md).

const landingWindowReads = 3 // blocked reads after mark-ready: more than the one the old poll never looked at

// assertLandedOnFirstTrial checks the invariants every settle scenario shares: exactly one
// trial PR was ever opened, it is the one that merged, and the landing posted no failure
// comment on any member.
func assertLandedOnFirstTrial(t *testing.T, env *Env, members []int) {
	t.Helper()
	if got := draftPRCount(env); got != 1 {
		t.Errorf("trial PRs opened = %d, want exactly 1 — the landing must not rebuild a green trial", got)
	}
	if merged := mergedTrialPRs(t, env); len(merged) != 1 {
		t.Errorf("merged trial PRs = %v, want exactly one", merged)
	}
	okMerges := 0
	for _, e := range env.Sim.Log().ByMethod("MergePR") {
		if e.Err == nil {
			okMerges++
		}
	}
	if okMerges != 1 {
		t.Errorf("successful MergePR calls = %d, want 1", okMerges)
	}
	for _, n := range members {
		if hasCommentContaining(t, env, n, "merge failure") {
			t.Errorf("#%d received a merge-failure comment for a landing that only had to wait", n)
		}
	}
}

// A single-member train goes green, the landing marks the trial PR ready, GitHub reads
// blocked for a few reads, and the same PR lands.
func TestMergeTrainLandingSettle_SingleMemberLandsOnFirstTrial(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	num, _ := QueueMember(t, env, "settle-single", map[string]string{"a.txt": "a\n"})
	startTrialSeeder(t, env, allGreenVerdict, &simgh.ReadyForReviewStep{BlockedReads: landingWindowReads})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, num, "Done", 20)
	assertLandedOnFirstTrial(t, env, []int{num})
	assertBlockedWindowObserved(t, env)
}

// assertBlockedWindowObserved proves the scripted window was actually read: the landing read
// the PR more often than a window-free landing would have after the mark-ready.
func assertBlockedWindowObserved(t *testing.T, env *Env) {
	t.Helper()
	entries := env.Sim.Log().Entries()
	readySeq := -1
	for _, e := range entries {
		if e.Method == "MarkPRReady" && e.Err == nil {
			readySeq = e.Seq
		}
	}
	if readySeq < 0 {
		t.Fatal("the trial PR was never marked ready")
	}
	reads := 0
	for _, e := range entries {
		if e.Seq > readySeq && e.Method == "FetchPRDetails" {
			reads++
		}
	}
	if reads <= landingWindowReads {
		t.Errorf("FetchPRDetails reads after mark-ready = %d, want > %d — the blocked window was not exercised", reads, landingWindowReads)
	}
}

// The same window on a two-member batch, landed through landMergeTrainBatch.
func TestMergeTrainLandingSettle_TwoMemberBatchLandsOnFirstTrial(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	numA, _ := QueueMember(t, env, "settle-batch-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "settle-batch-b", map[string]string{"b.txt": "b\n"})
	startTrialSeeder(t, env, allGreenVerdict, &simgh.ReadyForReviewStep{BlockedReads: landingWindowReads})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	assertLandedOnFirstTrial(t, env, []int{numA, numB})
	assertBlockedWindowObserved(t, env)
}

// When the short bound runs out, the green trial is kept (PR open, branch present), one
// comment is posted, and the next pass reuses that PR without opening a new trial.
func TestMergeTrainLandingSettle_BoundExpiryKeepsTrialAndNextPassReusesIt(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	num, _ := QueueMember(t, env, "settle-bound", map[string]string{"a.txt": "a\n"})
	env.Engine.SetLandingBlockedBoundForTest(100 * time.Millisecond)
	// A sim read costs a git merge (~100ms+), so one 100ms attempt makes two reads: the first
	// attempt runs out of bound inside the window (3 blocked reads), and the next pass's reads
	// see the window close.
	startTrialSeeder(t, env, allGreenVerdict, &simgh.ReadyForReviewStep{BlockedReads: 3})

	RunPoll(t, env)

	trialPRs, _ := openTrialPRs(t, env)
	if len(trialPRs) != 1 {
		t.Fatalf("open trial PRs after the bound ran out = %v, want the one kept green trial", trialPRs)
	}
	if status := projectItem(t, env, num).Status; status != "Queued" {
		t.Fatalf("#%d status = %q after a landing that did not merge, want Queued", num, status)
	}
	if !hasCommentContaining(t, env, num, "merge failure") {
		t.Error("the expired bound posted no merge-failure comment")
	}
	kept := trialPRs[0]

	for i := 0; i < 15; i++ {
		if projectItem(t, env, num).Status == "Done" {
			break
		}
		RunPoll(t, env)
	}
	WaitForProjectStatus(t, env, num, "Done", 5)

	if got := draftPRCount(env); got != 1 {
		t.Errorf("trial PRs opened = %d, want 1 — the next pass must reuse the kept PR", got)
	}
	if merged := mergedTrialPRs(t, env); len(merged) != 1 || merged[0] != kept {
		t.Errorf("merged trial PRs = %v, want the kept #%d", merged, kept)
	}
	comments := 0
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, "merge failure") {
			comments++
		}
	}
	if comments != 1 {
		t.Errorf("merge-failure comments = %d, want exactly 1 (not reposted for the same PR)", comments)
	}
}

// A restart in the middle of the landing wait (the persisted run is in the landing step) keeps
// the PR: the new process resumes it and merges it exactly once.
func TestMergeTrainLandingSettle_RestartMidWaitLandsKeptPRExactlyOnce(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{AsyncTrain: true})
	num, _ := QueueMember(t, env, "settle-restart", map[string]string{"a.txt": "a\n"})
	env.Engine.SetLandingBlockedBoundForTest(10 * time.Second)
	// Each sim read costs ~100ms+, so the old process consumes only a few of these before it
	// is stopped; the rest are left for the restarted one.
	stop := startTrialSeeder(t, env, allGreenVerdict, &simgh.ReadyForReviewStep{BlockedReads: 12})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	landing := false
	for i := 0; i < 200 && !landing; i++ {
		env.Clock.Advance(env.PollInterval)
		if err := env.Engine.PollOnce(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
		for _, p := range env.Engine.TrainRunPhases() {
			if p.Phase == "landing" {
				landing = true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !landing {
		t.Fatal("the run never reached the landing step")
	}

	// A kill leaves the landing-step record on disk; keep a copy, then stop the old process.
	saved := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(env.TrainStateDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one persisted run record mid-landing, got %v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saved, filepath.Base(files[0])), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cancel()
	for deadline := time.Now().Add(30 * time.Second); env.Engine.HasInFlightWorker(); {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled landing never returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	trialPRs, _ := openTrialPRs(t, env)
	if len(trialPRs) != 1 {
		t.Fatalf("cancelling the wait must keep the green trial PR, open trial PRs = %v", trialPRs)
	}
	env.TrainStateDir = saved

	env2 := restartMergeTrainEnv(t, env)
	env2.Engine.SetLandingBlockedBoundForTest(10 * time.Second)

	for i := 0; i < 40 && projectItem(t, env2, num).Status != "Done"; i++ {
		RunPoll(t, env2)
	}
	WaitForProjectStatus(t, env2, num, "Done", 5)

	if merged := mergedTrialPRs(t, env2); len(merged) != 1 || merged[0] != trialPRs[0] {
		t.Errorf("merged trial PRs = %v, want exactly the kept #%d", merged, trialPRs[0])
	}
	okMerges := 0
	for _, e := range env2.Sim.Log().ByMethod("MergePR") {
		if e.Err == nil {
			okMerges++
		}
	}
	if okMerges != 1 {
		t.Errorf("successful MergePR calls after the restart = %d, want exactly 1", okMerges)
	}
	if got := draftPRCount(env2); got != 1 { // the mutation log carries across the restart: the original trial only
		t.Errorf("trial PRs opened in total = %d, want 1 — the restart must resume the kept PR, not open another", got)
	}
	if ph := env2.Engine.TrainRunPhases(); len(ph) != 0 {
		t.Errorf("run still open after landing: %+v", ph)
	}
}
