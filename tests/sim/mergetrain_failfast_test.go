package sim

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// #2614: a fail-fast-cancelled trial run with a failed step is red, so the
// train bisects to the member that broke the test instead of retrying the same
// batch. Sim twin of engine/ci_failfast_consumers_test.go's trial cases.
//
// The shape is the observed one: the failing job's step concludes `failure`, a
// final step cancels the run, and GitHub reports the check run `cancelled`.

func failFastJobs(failedStep bool) []gh.WorkflowJob {
	conclusion := "cancelled"
	if failedStep {
		conclusion = "failure"
	}
	return []gh.WorkflowJob{{
		Name: "test", Status: "completed", Conclusion: "cancelled",
		Steps: []gh.WorkflowStep{
			{Name: "Set up job", Number: 1, Status: "completed", Conclusion: "success"},
			{Name: "Run tests", Number: 2, Status: "completed", Conclusion: conclusion},
			{Name: "Cancel run on failure", Number: 3, Status: "completed", Conclusion: "success"},
		},
	}}
}

// failFastScript scripts every trial PR: a trial containing a poisoned member is
// a fail-fast-cancelled run (steps with failedStep per the argument, re-run
// cancelled again); every other trial is green.
func failFastScript(t *testing.T, env *Env, poison map[int]bool, failedStep bool, trials *int, mu *sync.Mutex) {
	t.Helper()
	startTrialScript(t, env, func(pr int, sha string, trial int) {
		mu.Lock()
		*trials++
		mu.Unlock()
		s := env.Sim.Sim()
		poisoned := false
		for _, m := range parseClosesNumbers(prBody(t, env, pr)) {
			if poison[m] {
				poisoned = true
			}
		}
		if !poisoned {
			s.SeedCheckRun(env.OwnerRepo, sha, greenCheckRun("test"))
			return
		}
		id := int64(9300 + trial)
		s.SeedWorkflowRun(env.OwnerRepo, sha, gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "cancelled", JobCount: 1})
		s.SeedWorkflowRunJobs(env.OwnerRepo, id, failFastJobs(failedStep)...)
		s.SeedRerunStep(env.OwnerRepo, id, simgh.RerunStep{SHA: sha, Conclusion: "cancelled",
			Jobs:      failFastJobs(failedStep),
			CheckRuns: []gh.CheckRun{{Name: "test", Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(id, 1)}}})
		s.SeedCheckRun(env.OwnerRepo, sha, gh.CheckRun{Name: "test", Status: "completed", Conclusion: "cancelled", DetailsURL: actionsJobURL(id, 1)})
	})
}

func failFastBatch(t *testing.T, env *Env) (nums []int) {
	t.Helper()
	files := []map[string]string{{"a.txt": "a\n"}, {"b.txt": "b\n"}, {"c.txt": "c\n"}, {"d.txt": "d\n"}}
	for i := range files {
		n, _ := QueueMember(t, env, "failfast", files[i])
		nums = append(nums, n)
	}
	return nums
}

// The acceptance scenario: a batch of four whose trial run is fail-fast-cancelled
// after one member breaks a test bisects to that member.
func TestMergeTrainFailFast_BatchBisectsToTheBrokenMember(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)
	nums := failFastBatch(t, env)
	broken := nums[1]
	var mu sync.Mutex
	trials := 0
	failFastScript(t, env, map[int]bool{broken: true}, true, &trials, &mu)

	RunPoll(t, env)

	WaitForProjectStatus(t, env, broken, "Queued", 30)
	if !hasCommentContaining(t, env, broken, "ejected") {
		t.Fatalf("expected #%d to be ejected as the isolated poisoner", broken)
	}
	for i, n := range nums {
		if i != 1 {
			WaitForProjectStatus(t, env, n, "Done", 20)
		}
	}
	if n := len(env.Sim.Log().ByMethod("RerunFailedJobs")); n < 1 {
		t.Errorf("RerunFailedJobs calls = %d, want the one-time re-run per red trial (FR-008)", n)
	}
	if got := draftPRCount(env); got < 3 {
		t.Errorf("trial PRs = %d, want a bisection (several trials), not one retried batch", got)
	}
}

// Non-vacuity: the same scenario with the promotion neutralised is today's
// behaviour — the trial is infrastructure, the batch is not bisected, nobody is
// ejected.
func TestMergeTrainFailFast_NeutralisedRetriesTheBatchUncharged(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)
	defer env.Engine.SetFailFastPromotionDisabledForTest()()
	nums := failFastBatch(t, env)
	var mu sync.Mutex
	trials := 0
	failFastScript(t, env, map[int]bool{nums[1]: true}, true, &trials, &mu)

	RunPoll(t, env)
	RunPolls(t, env, 3)

	for _, n := range nums {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d status = %q, want it left in Queued", n, st)
		}
		if hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("#%d was ejected with the promotion neutralised", n)
		}
	}
	if got := draftPRCount(env); got != 1 {
		t.Errorf("trial PRs = %d, want 1 — the neutralised run reads infra and is not bisected", got)
	}
}

// A cancelled run with NO failed step (cancel-in-progress, a human) stays
// infrastructure: not bisected, nobody ejected (R4).
func TestMergeTrainFailFast_CancelledWithoutFailedStepStaysInfra(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)
	nums := failFastBatch(t, env)
	var mu sync.Mutex
	trials := 0
	failFastScript(t, env, map[int]bool{nums[1]: true}, false, &trials, &mu)

	RunPoll(t, env)
	RunPolls(t, env, 3)

	for _, n := range nums {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d status = %q, want it left in Queued", n, st)
		}
		if hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("#%d was ejected over a run with no failed step", n)
		}
	}
	if got := draftPRCount(env); got != 1 {
		t.Errorf("trial PRs = %d, want 1", got)
	}
}

// A failed jobs read keeps today's classification (R5).
func TestMergeTrainFailFast_JobsReadFailureKeepsInfra(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	env.Engine.SetCIInfraTimingForTest(5*time.Second, 5*time.Second, time.Minute, -1)
	env.Sim.Faults().FailAlways("FetchWorkflowRunJobs", ghfault.Wrap(gh.ErrForbidden, "Resource not accessible by integration"))
	nums := failFastBatch(t, env)
	var mu sync.Mutex
	trials := 0
	failFastScript(t, env, map[int]bool{nums[1]: true}, true, &trials, &mu)

	RunPoll(t, env)
	RunPolls(t, env, 3)

	for _, n := range nums {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d status = %q, want it left in Queued", n, st)
		}
		if hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("#%d was ejected although the jobs could not be read", n)
		}
	}
	if got := draftPRCount(env); got != 1 {
		t.Errorf("trial PRs = %d, want 1", got)
	}
}
