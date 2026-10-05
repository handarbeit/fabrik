package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// infraTrial is a scripted CI provider for one trial PR: a mutable view of what
// the workflow-run, check-run and re-run endpoints say, plus a record of what
// the engine did to it.
type infraTrial struct {
	mu        sync.Mutex
	runs      []gh.WorkflowRun
	checkRuns []gh.CheckRun
	mergeable string

	closes, reopens, runListReads int
	rerunIDs                      []int64

	// onReopen, if set, mutates the scripted state when the PR is reopened (the
	// provider "fires a new pull_request run").
	onReopen func(t *infraTrial)
	// onRerun, if set, mutates the state when a re-run is accepted.
	onRerun   func(t *infraTrial)
	rerunErr  error
	runsError error
}

func (it *infraTrial) client() *mockGitHubClient {
	return &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
			it.mu.Lock()
			defer it.mu.Unlock()
			return nil, it.mergeable, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			it.mu.Lock()
			defer it.mu.Unlock()
			return append([]gh.CheckRun(nil), it.checkRuns...), nil
		},
		fetchWorkflowRunsFn: func(owner, repo, sha string) ([]gh.WorkflowRun, error) {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.runListReads++
			if it.runsError != nil {
				return nil, it.runsError
			}
			return append([]gh.WorkflowRun(nil), it.runs...), nil
		},
		rerunFailedJobsFn: func(owner, repo string, id int64) error {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.rerunIDs = append(it.rerunIDs, id)
			if it.rerunErr != nil {
				return it.rerunErr
			}
			if it.onRerun != nil {
				it.onRerun(it)
			}
			return nil
		},
		closeIssueFn: func(owner, repo string, n int) error {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.closes++
			return nil
		},
		reopenIssueFn: func(owner, repo string, n int) error {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.reopens++
			if it.onReopen != nil {
				it.onReopen(it)
			}
			return nil
		},
	}
}

func deadWorkflowRun(id int64) gh.WorkflowRun {
	return gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "startup_failure",
		HTMLURL: fmt.Sprintf("https://github.com/owner/repo/actions/runs/%d", id)}
}

func healthyWorkflowRun(id int64) gh.WorkflowRun {
	return gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "success", JobCount: 2}
}

func actionsCheck(id, runID int64, name, conclusion string) gh.CheckRun {
	return gh.CheckRun{ID: id, Name: name, Status: "completed", Conclusion: conclusion,
		DetailsURL: fmt.Sprintf("https://github.com/owner/repo/actions/runs/%d/job/%d", runID, id)}
}

// infraTestEngine is trainTestEngine with a fast poll, a generous backstop (so a
// result is never the backstop's) and shrunk CI-infrastructure dwells.
func infraTestEngine(t *testing.T, it *infraTrial) *Engine {
	t.Helper()
	eng := trainTestEngine(t, it.client(), &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.cfg.CIBackstopTimeout = time.Minute
	eng.SetTrainCIPollIntervalForTest(time.Millisecond)
	eng.SetCIInfraTimingForTest(150*time.Millisecond, 150*time.Millisecond, time.Minute, -1)
	return eng
}

func pollInfraTrial(t *testing.T, eng *Engine) (TrainCIResult, *trainCIDiagnostic) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return eng.pollTrainCI(ctx, "owner", "repo", 42, "sha123")
}

// ── R1/R3: startup failure is retriggered, not held to the backstop ─────────

func TestPollTrainCI_StartupFailure_RetriggeredByCloseReopenThenGreen(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", runs: []gh.WorkflowRun{deadWorkflowRun(1)}}
	it.onReopen = func(it *infraTrial) {
		// The reopen fires a fresh run; the dead one stays on the head SHA.
		it.runs = append(it.runs, healthyWorkflowRun(2))
		it.checkRuns = []gh.CheckRun{actionsCheck(11, 2, "build", "success")}
	}
	eng := infraTestEngine(t, it)

	start := time.Now()
	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green after the retrigger started CI", result)
	}
	if it.closes != 1 || it.reopens != 1 {
		t.Fatalf("close/reopen = %d/%d, want exactly 1/1", it.closes, it.reopens)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v — held to a backstop instead of retriggering", time.Since(start))
	}
}

// R3a: the dead run stays on the head SHA after the reopen and must not burn the
// retrigger budget in one loop.
func TestPollTrainCI_StartupFailure_OldRunDoesNotConsumeSecondRetrigger(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", runs: []gh.WorkflowRun{deadWorkflowRun(1)}}
	// The reopen starts nothing: only the old dead run is ever visible.
	eng := infraTestEngine(t, it)

	result, diag := pollInfraTrial(t, eng)
	if result != TrainCIInfra {
		t.Fatalf("result = %v, want TrainCIInfra once no new run appeared within the dwell", result)
	}
	if it.closes != 1 || it.reopens != 1 {
		t.Fatalf("close/reopen = %d/%d, want exactly 1/1 — the old run was counted again", it.closes, it.reopens)
	}
	if diag == nil || diag.Note == "" {
		t.Fatalf("want a diagnostic naming the run, got %+v", diag)
	}
}

// R3/R4: once the bound is spent the trial is abandoned, naming the run.
func TestPollTrainCI_StartupFailure_AbandonedAfterBound(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", runs: []gh.WorkflowRun{deadWorkflowRun(1)}}
	next := int64(2)
	it.onReopen = func(it *infraTrial) { // every reopen produces another dead run
		it.runs = append(it.runs, deadWorkflowRun(next))
		next++
	}
	eng := infraTestEngine(t, it)

	result, diag := pollInfraTrial(t, eng)
	if result != TrainCIInfra {
		t.Fatalf("result = %v, want TrainCIInfra", result)
	}
	if it.closes != maxCIRetriggers || it.reopens != maxCIRetriggers {
		t.Fatalf("close/reopen = %d/%d, want %d/%d", it.closes, it.reopens, maxCIRetriggers, maxCIRetriggers)
	}
	if diag == nil || !strings.Contains(diag.Note, "actions/runs/3") {
		t.Fatalf("diagnostic must name the last dead run (3), got %+v", diag)
	}
}

// Without required checks mergeable_state is `clean` with zero check runs, which
// used to land a trial whose CI never ran.
func TestPollTrainCI_StartupFailure_NeverFalseGreenOnCleanState(t *testing.T) {
	it := &infraTrial{mergeable: "clean", runs: []gh.WorkflowRun{deadWorkflowRun(1)}}
	eng := infraTestEngine(t, it)

	result, _ := pollInfraTrial(t, eng)
	if result == TrainCIGreen {
		t.Fatal("a trial whose only workflow run is a startup_failure must never be green")
	}
	if result != TrainCIInfra {
		t.Fatalf("result = %v, want TrainCIInfra", result)
	}
}

func TestPollTrainCI_CancelledZeroJobRunIsNotInfrastructure(t *testing.T) {
	it := &infraTrial{mergeable: "clean", runs: []gh.WorkflowRun{{ID: 1, Status: "completed", Conclusion: "cancelled"}}}
	eng := infraTestEngine(t, it)

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want today's green (a superseded-run cancellation is not a startup failure)", result)
	}
	if it.closes != 0 {
		t.Fatalf("PR closed %d time(s) over a deliberate cancellation", it.closes)
	}
}

// A run that exists but has not finished, with no check run yet, holds the
// zero-check green shortcut.
func TestPollTrainCI_UnfinishedRunHoldsZeroCheckGreen(t *testing.T) {
	it := &infraTrial{mergeable: "clean", runs: []gh.WorkflowRun{{ID: 1, Name: "CI", Status: "queued"}}}
	eng := infraTestEngine(t, it)
	go func() {
		time.Sleep(60 * time.Millisecond)
		it.mu.Lock()
		it.runs = []gh.WorkflowRun{healthyWorkflowRun(1)}
		it.checkRuns = []gh.CheckRun{actionsCheck(11, 1, "build", "success")}
		it.mu.Unlock()
	}()
	start := time.Now()
	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green", result)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("went green while a workflow run was still queued with no check run")
	}
}

// R7: a refused workflow-run read behaves exactly as before — here the clean
// zero-check shortcut still goes green.
func TestPollTrainCI_WorkflowRunReadRefused_BehavesAsBefore(t *testing.T) {
	it := &infraTrial{mergeable: "clean", runsError: fmt.Errorf("x: %w", gh.ErrForbidden)}
	eng := infraTestEngine(t, it)

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want today's green", result)
	}
	if it.closes != 0 {
		t.Fatalf("a refused read must not retrigger (closes = %d)", it.closes)
	}
	if !eng.actionsDegradeLogged.Load() {
		t.Error("the degrade notice was not recorded")
	}
}

// ── R5: one re-run of the failed jobs before a trial is red ──────────────────

func failedFirstTrial() *infraTrial {
	return &infraTrial{
		mergeable: "blocked",
		checkRuns: []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(12, 10, "test", "failure")},
	}
}

func TestPollTrainCI_FlakeRedOnceThenGreen_NoRed(t *testing.T) {
	it := failedFirstTrial()
	it.onRerun = func(it *infraTrial) { // the re-run supersedes the failed check with a higher id
		it.checkRuns = []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(13, 10, "test", "success")}
	}
	eng := infraTestEngine(t, it)

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green after the re-run passed", result)
	}
	if len(it.rerunIDs) != 1 || it.rerunIDs[0] != 10 {
		t.Fatalf("re-runs = %v, want exactly one, of run 10", it.rerunIDs)
	}
}

func TestPollTrainCI_RedTwice_IsRedAfterExactlyOneRerun(t *testing.T) {
	it := failedFirstTrial()
	it.onRerun = func(it *infraTrial) {
		it.checkRuns = []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(13, 10, "test", "failure")}
	}
	eng := infraTestEngine(t, it)

	result, diag := pollInfraTrial(t, eng)
	if result != TrainCIRed {
		t.Fatalf("result = %v, want red on the second failure", result)
	}
	if len(it.rerunIDs) != 1 {
		t.Fatalf("re-runs = %v, want exactly one per trial", it.rerunIDs)
	}
	if diag == nil || len(diag.FailedChecks) != 1 || diag.FailedChecks[0].ID != 13 {
		t.Fatalf("diagnostic must carry the re-run's failure, got %+v", diag)
	}
}

func TestPollTrainCI_RerunError_FallsBackToRed(t *testing.T) {
	it := failedFirstTrial()
	it.rerunErr = fmt.Errorf("x: %w", gh.ErrForbidden)
	eng := infraTestEngine(t, it)

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIRed {
		t.Fatalf("result = %v, want red when rerun-failed-jobs is refused", result)
	}
}

func TestPollTrainCI_ThirdPartyFailure_FallsBackToRed(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", checkRuns: []gh.CheckRun{
		{ID: 5, Name: "ci/circleci", Status: "completed", Conclusion: "failure", DetailsURL: "https://circleci.com/gh/o/r/9"},
	}}
	eng := infraTestEngine(t, it)

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIRed || len(it.rerunIDs) != 0 {
		t.Fatalf("result = %v, re-runs = %v; want red with no re-run for a check with no workflow run", result, it.rerunIDs)
	}
}

// Right after the re-run is accepted the stale failure is still visible for a
// poll or two; that must not be counted as the second failure.
func TestPollTrainCI_RerunStaleFailureIsNotTheSecondFailure(t *testing.T) {
	it := failedFirstTrial()
	eng := infraTestEngine(t, it)
	go func() {
		for {
			it.mu.Lock()
			reran := len(it.rerunIDs) > 0
			it.mu.Unlock()
			if reran {
				time.Sleep(40 * time.Millisecond) // the stale failure stays visible for a while
				it.mu.Lock()
				it.checkRuns = []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(13, 10, "test", "success")}
				it.mu.Unlock()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green — the stale failure was counted as a second one", result)
	}
}

// If the re-run never materialises, the stale failure becomes the verdict once
// the settle dwell elapses — bounded, never a hang.
func TestPollTrainCI_RerunNeverMaterialises_RedAfterSettleDwell(t *testing.T) {
	it := failedFirstTrial()
	eng := infraTestEngine(t, it)

	start := time.Now()
	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIRed {
		t.Fatalf("result = %v, want red", result)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatal("red was returned before the settle dwell elapsed")
	}
}

// A re-run still queued behind runner capacity when the settle dwell elapses is
// not a second failure: the workflow run on the SHA is not completed, so the
// trial keeps waiting for it.
func TestPollTrainCI_RerunQueuedPastSettleDwell_IsNotTheSecondFailure(t *testing.T) {
	it := failedFirstTrial()
	it.onRerun = func(it *infraTrial) {
		it.runs = []gh.WorkflowRun{{ID: 10, Name: "CI", Status: "queued"}}
	}
	eng := infraTestEngine(t, it) // settle dwell 150ms
	go func() {
		for {
			it.mu.Lock()
			reran := len(it.rerunIDs) > 0
			it.mu.Unlock()
			if reran {
				time.Sleep(400 * time.Millisecond) // well past the dwell, still queued
				it.mu.Lock()
				it.runs = []gh.WorkflowRun{healthyWorkflowRun(10)}
				it.checkRuns = []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(13, 10, "test", "success")}
				it.mu.Unlock()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green — a queued re-run was counted as a second failure", result)
	}
}

// Each pollTrainCI call is one trial with its own single re-run — the property
// a bisection sub-trial relies on.
func TestPollTrainCI_EachTrialGetsItsOwnRerun(t *testing.T) {
	it := failedFirstTrial()
	it.onRerun = func(it *infraTrial) {
		it.checkRuns = []gh.CheckRun{actionsCheck(13, 10, "test", "failure")}
	}
	eng := infraTestEngine(t, it)
	for i := 0; i < 2; i++ {
		it.mu.Lock()
		it.checkRuns = []gh.CheckRun{actionsCheck(int64(20+i*10), 10, "test", "failure")}
		it.mu.Unlock()
		it.onRerun = func(it *infraTrial) {
			it.checkRuns = []gh.CheckRun{actionsCheck(it.checkRuns[0].ID+1, 10, "test", "failure")}
		}
		if result, _ := pollInfraTrial(t, eng); result != TrainCIRed {
			t.Fatalf("trial %d: result = %v, want red", i, result)
		}
	}
	if len(it.rerunIDs) != 2 {
		t.Fatalf("re-runs = %v, want one per trial (2)", it.rerunIDs)
	}
}
