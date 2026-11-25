package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// #2614: consumers of a fail-fast-cancelled run with a failed step.

// withJobs gives the engine's client the jobs-read capability over its existing mock.
func withJobs(t *testing.T, eng *Engine, jobs map[int64][]gh.WorkflowJob) *jobsClient {
	t.Helper()
	if existing, ok := eng.client.(*jobsClient); ok {
		existing.mu.Lock()
		existing.jobs = jobs
		existing.mu.Unlock()
		return existing
	}
	mc, ok := eng.client.(*mockGitHubClient)
	if !ok {
		t.Fatalf("engine client is %T, want *mockGitHubClient", eng.client)
	}
	jc := &jobsClient{mockGitHubClient: mc, jobs: jobs}
	eng.client = jc
	return jc
}

// A trial whose only run is cancelled with a failed step: one re-run (kept, FR-008),
// then red — never the infra verdict.
func TestEvalTrialCI_FailFastCancelled_RerunThenRed(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", checkRuns: []gh.CheckRun{actionsCheck(11, 7, "test", "cancelled")},
		runs: []gh.WorkflowRun{healthyWorkflowRun(7)}}
	eng := infraTestEngine(t, it)
	it.useClock(eng)
	withJobs(t, eng, map[int64][]gh.WorkflowJob{7: failedStepJobs(11)})
	ctx := context.Background()
	var st trialCIState

	if _, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st); decided {
		t.Fatal("the first promoted failure must be re-run once, not decided")
	}
	if !st.rr.done || len(it.rerunIDs) != 1 {
		t.Fatalf("rerun done=%v ids=%v, want one re-run", st.rr.done, it.rerunIDs)
	}
	// The re-run was fail-fast-cancelled again (new check-run ID, same shape).
	it.mu.Lock()
	it.checkRuns = []gh.CheckRun{actionsCheck(12, 7, "test", "cancelled")}
	it.mu.Unlock()
	withJobs(t, eng, map[int64][]gh.WorkflowJob{7: failedStepJobs(12)})
	res, diag, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIRed || diag == nil || len(diag.FailedChecks) != 1 {
		t.Fatalf("decided=%v res=%v diag=%+v, want red with the failed check", decided, res, diag)
	}
	if got := describeCheckRuns(diag.FailedChecks); !strings.Contains(got, "Run tests") {
		t.Errorf("diagnostic %q should name the failed step", got)
	}
}

// Same shape with the promotion neutralised is today's infra verdict (non-vacuity).
func TestEvalTrialCI_FailFastCancelled_NeutralisedIsInfra(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", checkRuns: []gh.CheckRun{actionsCheck(11, 7, "test", "cancelled")},
		runs: []gh.WorkflowRun{healthyWorkflowRun(7)}}
	eng := infraTestEngine(t, it)
	it.useClock(eng)
	withJobs(t, eng, map[int64][]gh.WorkflowJob{7: failedStepJobs(11)})
	defer eng.SetFailFastPromotionDisabledForTest()()
	ctx := context.Background()
	var st trialCIState
	eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	it.mu.Lock()
	it.checkRuns = []gh.CheckRun{actionsCheck(12, 7, "test", "cancelled")}
	it.mu.Unlock()
	res, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIInfra {
		t.Fatalf("decided=%v res=%v, want infra when promotion is off", decided, res)
	}
}

// No failed step: unchanged infra.
func TestEvalTrialCI_CancelledNoFailedStep_StaysInfra(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", checkRuns: []gh.CheckRun{actionsCheck(11, 7, "test", "cancelled")},
		runs: []gh.WorkflowRun{healthyWorkflowRun(7)}}
	eng := infraTestEngine(t, it)
	it.useClock(eng)
	noFail := []gh.WorkflowJob{{ID: 11, Name: "test", Status: "completed", Steps: []gh.WorkflowStep{{Name: "Run tests", Conclusion: "cancelled"}}}}
	withJobs(t, eng, map[int64][]gh.WorkflowJob{7: noFail})
	ctx := context.Background()
	var st trialCIState
	eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	it.mu.Lock()
	it.checkRuns = []gh.CheckRun{actionsCheck(12, 7, "test", "cancelled")}
	it.mu.Unlock()
	res, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIInfra {
		t.Fatalf("decided=%v res=%v, want infra", decided, res)
	}
}

// A failed jobs read keeps today's infra verdict.
func TestEvalTrialCI_CancelledJobsReadFails_StaysInfra(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", checkRuns: []gh.CheckRun{actionsCheck(11, 7, "test", "cancelled")},
		runs: []gh.WorkflowRun{healthyWorkflowRun(7)}}
	eng := infraTestEngine(t, it)
	it.useClock(eng)
	jc := withJobs(t, eng, nil)
	jc.err = fmt.Errorf("x: %w", gh.ErrForbidden)
	ctx := context.Background()
	var st trialCIState
	eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	it.mu.Lock()
	it.checkRuns = []gh.CheckRun{actionsCheck(12, 7, "test", "cancelled")}
	it.mu.Unlock()
	res, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIInfra {
		t.Fatalf("decided=%v res=%v, want infra", decided, res)
	}
}

// classifyLandingCI (admission, fast path, landing settle) reads it red, not "not passed".
func TestClassifyLandingCI_FailFastCancelled_IsRed(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	jc := withJobs(t, eng, map[int64][]gh.WorkflowJob{900: failedStepJobs(2)})
	_ = jc
	runs := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{cancelledCheck(2)})
	if res, _ := eng.classifyLandingCI("owner", "repo", "blocked", "sha1", runs); res != TrainCIRed {
		t.Fatalf("result = %v, want TrainCIRed", res)
	}
	plain := []gh.CheckRun{cancelledCheck(2)}
	if res, _ := eng.classifyLandingCI("owner", "repo", "blocked", "sha1", plain); res != TrainCINotPassed {
		t.Fatalf("unpromoted result = %v, want TrainCINotPassed", res)
	}
}

// The stage gate: settlePRMergeState reads the head through the promoted path, so a
// fail-fast-cancelled run with a failed step is a CI failure with regression framing.
func TestSettle_FailFastCancelled_IsCIFailureWithRegressionFraming(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			return boolPtr(true), "blocked", nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}, cancelledCheck(2)}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	withJobs(t, eng, map[int64][]gh.WorkflowJob{900: failedStepJobs(2)})
	tr := true
	stage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	r := eng.settlePRMergeStateWith(settleItem(1), stage, true)
	if r.Status != PRMergeBlocked {
		t.Fatalf("status = %v, want PRMergeBlocked", r.Status)
	}
	blocked, ciFailure, _ := eng.classifyCIFromCheckRuns("owner", "repo", gh.ProjectItem{Number: 1}, r.CheckRuns)
	if !blocked || !ciFailure {
		t.Fatalf("(blocked, ciFailure) = (%v, %v)", blocked, ciFailure)
	}
	body := eng.buildCIFixComment(gh.ProjectItem{Number: 1}, stage, "/tmp", r).Body
	for _, want := range []string{"NEW REGRESSION", "Run tests", "fail-fast", "**test**"} {
		if !strings.Contains(body, want) {
			t.Errorf("CI-fix comment missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"not a test failure", "fresh run is needed"} {
		if strings.Contains(body, bad) {
			t.Errorf("CI-fix comment must not carry cancellation framing %q:\n%s", bad, body)
		}
	}
}

// The ci-status.md failing-checks section names the failed step.
func TestRenderFailingChecks_NamesFailedStep(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	withJobs(t, eng, map[int64][]gh.WorkflowJob{900: failedStepJobs(2)})
	runs := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{cancelledCheck(2)})
	var extras ciStatusExtras
	eng.collectFailingChecks(gh.ProjectItem{Number: 1}, "owner", "repo", runs, &extras)
	var b strings.Builder
	renderFailingChecks(&b, &extras)
	out := b.String()
	if !strings.Contains(out, "failed step") || !strings.Contains(out, "Run tests") || strings.Contains(out, "not a test failure") {
		t.Errorf("failing-checks section:\n%s", out)
	}
}

// The catch-up wait's member CI: cancelled with a failed step reads red after the re-run.
func TestMemberCI_FailFastCancelledPromotedIsFailedClass(t *testing.T) {
	eng := testEngineForMerge(t, &mockGitHubClient{})
	withJobs(t, eng, map[int64][]gh.WorkflowJob{900: failedStepJobs(2)})
	runs := eng.promoteFailFast("owner", "repo", "sha1", []gh.CheckRun{cancelledCheck(2)})
	status, _, failed := gh.ClassifyCheckRuns(runs)
	if status != gh.CheckRunsFailed || len(failed) != 1 {
		t.Fatalf("status = %v failed=%d, want Failed so waitMemberCI takes its red branch", status, len(failed))
	}
}
