package sim

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// This file is the sim twin of #2052: the merge train telling CI infrastructure
// failures (a workflow run that never started; a single flaky test) from real
// ones. There is no live twin — a startup_failure cannot be induced on demand
// against the e2e bed (see tests/sim/simgh/FIDELITY.md, "Workflow runs", and
// adrs/2052-ci-infrastructure-failures-in-merge-train.md).

// startTrialScript runs script once for every new trial PR the engine opens
// (head under the merge-train branch prefix), in its own goroutine, mirroring
// startTrialVerdictSeeder's polling. script is where a scenario scripts that
// trial's workflow runs, reopen outcomes and check runs. Returns after
// registering its own t.Cleanup.
func startTrialScript(t *testing.T, env *Env, script func(prNumber int, sha string, trial int)) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seen := map[int]bool{}
		trial := 0
		for {
			select {
			case <-done:
				return
			default:
			}
			if prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo); err == nil {
				for _, pr := range prs {
					if seen[pr.Number] || pr.State != "open" || !strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
						continue
					}
					seen[pr.Number] = true
					trial++
					script(pr.Number, pr.HeadSHA, trial)
				}
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(done) }); wg.Wait() })
}

func actionsJobURL(runID int64, job int) string {
	return fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d/job/%d", runID, job)
}

// mutationsOn returns the methods of the mutations that touched PR/issue number.
func mutationsOn(env *Env, number int, methods ...string) []string {
	want := map[string]bool{}
	for _, m := range methods {
		want[m] = true
	}
	var out []string
	for _, e := range env.Sim.Log().Mutations() {
		if want[e.Method] && e.Args.Number == number && e.Err == nil {
			out = append(out, e.Method)
		}
	}
	return out
}

// A trial whose CI run never started is retriggered by close/reopen and, once
// CI runs, lands normally — never held to the backstop.
func TestMergeTrainCIInfra_StartupFailureRetriggeredThenLands(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)

	numA, _ := QueueMember(t, env, "infra-retrigger-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "infra-retrigger-b", map[string]string{"b.txt": "b\n"})
	var trialPR int
	var mu sync.Mutex
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		mu.Lock()
		trialPR = pr
		mu.Unlock()
		s := env.Sim.Sim()
		s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: 5001, Name: "CI", Status: "completed", Conclusion: "startup_failure"})
		// The reopen fires a fresh, healthy run; the dead one stays on the SHA.
		s.SeedReopenSteps(env.OwnerRepo, pr, simgh.ReopenStep{
			SHA:          sha,
			WorkflowRuns: []gh.WorkflowRun{{ID: 5002, Name: "CI", Status: "completed", Conclusion: "success", JobCount: 1}},
			CheckRuns:    []gh.CheckRun{greenCheckRun("build")},
		})
	})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	mu.Lock()
	pr := trialPR
	mu.Unlock()
	if got := mutationsOn(env, pr, "CloseIssue", "ReopenIssue"); strings.Join(got, ",") != "CloseIssue,ReopenIssue" {
		t.Errorf("trial PR #%d close/reopen mutations = %v, want exactly one close then one reopen", pr, got)
	}
	if n := len(env.Sim.Log().ByMethod("CreateDraftPR")); n != 1 {
		t.Errorf("draft CI PRs created = %d, want 1 — a retrigger must reuse the trial, not rebuild it", n)
	}
}

// A single flaky failure is re-run once and the trial lands without bisecting.
func TestMergeTrainCIInfra_FlakeRerunThenLandsWithoutBisecting(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)

	numA, _ := QueueMember(t, env, "infra-flake-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "infra-flake-b", map[string]string{"b.txt": "b\n"})
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		s := env.Sim.Sim()
		// A finished, failed Actions run with one failing job — a flake.
		s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: 6001, Name: "CI", Status: "completed", Conclusion: "failure", JobCount: 1})
		s.SeedRerunStep(env.OwnerRepo, 6001, simgh.RerunStep{
			SHA:       sha,
			CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success", DetailsURL: actionsJobURL(6001, 1)}},
		})
		s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "failure", DetailsURL: actionsJobURL(6001, 1)})
	})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n != 1 {
		t.Errorf("RerunFailedJobs calls = %d, want exactly 1", n)
	}
	if n := len(env.Sim.Log().ByMethod("CreateDraftPR")); n != 1 {
		t.Errorf("trial PRs = %d, want 1 — a flake must not start a bisection", n)
	}
	for _, n := range []int{numA, numB} {
		if hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("#%d was ejected over a flake", n)
		}
	}
}

// A trial that is red twice is red: the re-run happens once, then the existing
// bisect/eject path runs as before.
func TestMergeTrainCIInfra_RedTwiceStillIsolatesThePoisoner(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)

	numA, _ := QueueMember(t, env, "infra-red2-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "infra-red2-b", map[string]string{"b.txt": "b\n"})
	runID := int64(7000)
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		s := env.Sim.Sim()
		id := runID + int64(trial)
		members := parseClosesNumbers(prBody(t, env, pr))
		bad := false
		for _, m := range members {
			if m == numB {
				bad = true
			}
		}
		s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "failure", JobCount: 1})
		if bad { // fails again after the re-run
			s.SeedRerunStep(env.OwnerRepo, id, simgh.RerunStep{SHA: sha,
				CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure", DetailsURL: actionsJobURL(id, 1)}}})
			s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "failure", DetailsURL: actionsJobURL(id, 1)})
			return
		}
		s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "success", DetailsURL: actionsJobURL(id, 1)})
	})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 30)
	if projectItem(t, env, numB).IsClosed {
		t.Fatalf("#%d is poisoned and must not land", numB)
	}
}

// A trial whose CI never starts, however often it is retriggered, is abandoned
// quickly with its members left Queued and nothing charged; the partition is
// held off for the cooldown rather than rebuilt every poll.
func TestMergeTrainCIInfra_PersistentStartupFailureAbandonedUncharged(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(40*time.Millisecond, 5*time.Second, time.Minute, -1)

	numA, _ := QueueMember(t, env, "infra-dead-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "infra-dead-b", map[string]string{"b.txt": "b\n"})
	var trialPR int
	var mu sync.Mutex
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		mu.Lock()
		trialPR = pr
		mu.Unlock()
		// A dead run, and a reopen that starts nothing (no ReopenStep queued).
		env.Sim.Sim().SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: 8001, Name: "CI", Status: "completed", Conclusion: "startup_failure"})
	})

	start := time.Now()
	RunPoll(t, env)
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("abandoning took %v — it must take minutes-scale dwells, not the backstop", d)
	}

	for _, n := range []int{numA, numB} {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d status = %q, want it left in Queued", n, st)
		}
		labels := IssueLabels(t, env, n)
		if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") {
			t.Errorf("#%d was paused: %v", n, labels)
		}
		if hasCommentContaining(t, env, n, "ejected") || hasCommentContaining(t, env, n, "dissolved") {
			t.Errorf("#%d was charged or dissolved over an infrastructure failure", n)
		}
	}
	mu.Lock()
	pr := trialPR
	mu.Unlock()
	if got := mutationsOn(env, pr, "CloseIssue", "ReopenIssue"); len(got) < 2 || got[0] != "CloseIssue" || got[1] != "ReopenIssue" {
		t.Errorf("trial PR #%d mutations = %v, want a retrigger (close, reopen) before abandoning", pr, got)
	}
	if n := len(env.Sim.Log().ByMethod("MergePR")); n != 0 {
		t.Errorf("MergePR calls = %d, want 0", n)
	}

	// The partition is on cooldown: further polls form no new trial.
	trials := len(env.Sim.Log().ByMethod("CreateDraftPR"))
	RunPolls(t, env, 3)
	if got := len(env.Sim.Log().ByMethod("CreateDraftPR")); got != trials {
		t.Errorf("draft PRs %d -> %d: a fresh trial was formed inside the cooldown", trials, got)
	}
}

// A refused workflow-run read (the Actions permission was revoked at runtime)
// leaves the pre-#2052 behaviour: the trial polls on, the dead run is invisible.
func TestMergeTrainCIInfra_PermissionRefusedDegradesQuietly(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)
	env.Sim.Faults().FailAlways("FetchWorkflowRuns", ghfault.Wrap(gh.ErrForbidden, "Resource not accessible by integration"))

	numA, _ := QueueMember(t, env, "infra-perm-a", map[string]string{"a.txt": "a\n"})
	numB, _ := QueueMember(t, env, "infra-perm-b", map[string]string{"b.txt": "b\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
}

// prBody returns the body of PR number (empty if unknown).
func prBody(t *testing.T, env *Env, number int) string {
	t.Helper()
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	for _, pr := range prs {
		if pr.Number == number {
			return pr.Body
		}
	}
	return ""
}
