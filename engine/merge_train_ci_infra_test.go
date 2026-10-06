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
	onRerun func(t *infraTrial)
	// Poll-scripting hooks, called under mu with the number of reads made so far
	// of that endpoint after the re-run was accepted (0 before it). They replace
	// goroutines and sleeps: the state changes on a read count, never on wall time.
	onMergeableRead func(t *infraTrial, sinceRerun int)
	onCheckRunsRead func(t *infraTrial, sinceRerun int)
	onRunsRead      func(t *infraTrial, sinceRerun int)

	checkRunReads, runReads            int
	checkReadsAtRerun, runReadsAtRerun int
	rerunMergeableReads                int // mergeable reads since the re-run was accepted
	rerunErr                           error
	runsError                          error
}

func (it *infraTrial) client() *mockGitHubClient {
	sinceRerun := func(n int) int {
		if len(it.rerunIDs) == 0 {
			return 0
		}
		return n
	}
	return &mockGitHubClient{
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
			it.mu.Lock()
			defer it.mu.Unlock()
			if len(it.rerunIDs) > 0 {
				it.rerunMergeableReads++
			}
			if it.onMergeableRead != nil {
				it.onMergeableRead(it, it.rerunMergeableReads)
			}
			return nil, it.mergeable, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.checkRunReads++
			if it.onCheckRunsRead != nil {
				it.onCheckRunsRead(it, sinceRerun(it.checkRunReads-it.checkReadsAtRerun))
			}
			return append([]gh.CheckRun(nil), it.checkRuns...), nil
		},
		fetchWorkflowRunsFn: func(owner, repo, sha string) ([]gh.WorkflowRun, error) {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.runListReads++
			it.runReads++
			if it.onRunsRead != nil {
				it.onRunsRead(it, sinceRerun(it.runReads-it.runReadsAtRerun))
			}
			if it.runsError != nil {
				return nil, it.runsError
			}
			return append([]gh.WorkflowRun(nil), it.runs...), nil
		},
		rerunFailedJobsFn: func(owner, repo string, id int64) error {
			it.mu.Lock()
			defer it.mu.Unlock()
			it.rerunIDs = append(it.rerunIDs, id)
			it.checkReadsAtRerun, it.runReadsAtRerun = it.checkRunReads, it.runReads
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

// advClock is an engine clock the test advances explicitly, so a dwell or a
// timeout elapses on a scripted read count, not on wall time. Fixing the
// scheduling this way is what keeps these tests deterministic on a loaded host.
type advClock struct {
	mu sync.Mutex
	t  time.Time
}

func newAdvClock() *advClock { return &advClock{t: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)} }

func (c *advClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *advClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// infraDwell is the re-run settle dwell infraTestEngine configures.
const infraDwell = 150 * time.Millisecond

// useClock gives pollTrainCI's retrigger and re-run dwells an explicit clock for
// a test that scripts them. The default is the real clock, which the retrigger
// tests rely on.
func (it *infraTrial) useClock(eng *Engine) *advClock {
	c := newAdvClock()
	eng.SetCIInfraClockForTest(c)
	return c
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
	// The run stays queued, with no check run, for the first three reads of the
	// workflow runs, then CI shows up.
	it.onRunsRead = func(it *infraTrial, _ int) {
		if it.runReads > 3 {
			it.runs = []gh.WorkflowRun{healthyWorkflowRun(1)}
			it.checkRuns = []gh.CheckRun{actionsCheck(11, 1, "build", "success")}
		}
	}
	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green", result)
	}
	if it.runReads <= 3 {
		t.Fatalf("went green after %d run read(s), while a workflow run was still queued with no check run", it.runReads)
	}
}

// Only a queued or running workflow run holds the zero-check green shortcut. A
// run held in `waiting` (environment approval) or `pending` is not CI on its
// way; before #2052 such a trial went green, and it must not sit to the backstop.
func TestPollTrainCI_WaitingOrPendingRunDoesNotHoldZeroCheckGreen(t *testing.T) {
	for _, status := range []string{"waiting", "pending", "requested"} {
		it := &infraTrial{mergeable: "clean", runs: []gh.WorkflowRun{{ID: 1, Name: "Deploy", Status: status}}}
		eng := infraTestEngine(t, it)

		start := time.Now()
		result, _ := pollInfraTrial(t, eng)
		if result != TrainCIGreen {
			t.Fatalf("status %q: result = %v, want today's green", status, result)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("status %q: held for %v by a run that is not CI on its way", status, time.Since(start))
		}
		if it.closes != 0 {
			t.Fatalf("status %q: a non-failed run must not retrigger (closes = %d)", status, it.closes)
		}
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
	// The stale failure stays the latest for the first few reads after the
	// re-run was accepted (the settle dwell never elapses: the clock is frozen),
	// then the re-run's check runs replace it.
	it.onCheckRunsRead = func(it *infraTrial, sinceRerun int) {
		if sinceRerun >= 4 {
			it.checkRuns = []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(13, 10, "test", "success")}
		}
	}
	it.useClock(eng)

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
	clock := it.useClock(eng)
	// The re-run never produces anything and no run is in flight; the settle
	// dwell elapses on the third poll after the re-run was accepted.
	it.onMergeableRead = func(it *infraTrial, sinceRerun int) {
		if sinceRerun == 3 {
			clock.Advance(infraDwell + time.Millisecond)
		}
	}

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIRed {
		t.Fatalf("result = %v, want red", result)
	}
	if it.rerunMergeableReads < 3 {
		t.Fatalf("red after %d poll(s) since the re-run, before the settle dwell elapsed", it.rerunMergeableReads)
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
	eng := infraTestEngine(t, it)
	clock := it.useClock(eng)
	it.onMergeableRead = func(it *infraTrial, sinceRerun int) {
		if sinceRerun == 2 {
			clock.Advance(infraDwell + time.Millisecond)
		}
	}
	// Past the dwell the run stays queued for three reads of the workflow runs,
	// then completes — on the read itself, i.e. between the two reads of one poll
	// iteration, which is the window a naive check-runs-then-runs order misses.
	pastDwellReads := 0
	it.onRunsRead = func(it *infraTrial, sinceRerun int) {
		pastDwellReads++
		if pastDwellReads > 3 {
			it.runs = []gh.WorkflowRun{healthyWorkflowRun(10)}
			it.checkRuns = []gh.CheckRun{actionsCheck(11, 10, "build", "success"), actionsCheck(13, 10, "test", "success")}
		}
	}

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIGreen {
		t.Fatalf("result = %v, want green — a queued re-run, or one finishing between the two reads, was counted as a second failure", result)
	}
	if pastDwellReads <= 3 {
		t.Fatalf("settled after %d in-flight read(s); the queued re-run was not waited for", pastDwellReads)
	}
}

// Only the re-run workflow runs count as "still in flight". An unrelated
// workflow still running, or a run held in `waiting` for an approval, must not
// postpone the stale failure's verdict past the settle dwell.
func TestPollTrainCI_UnrelatedInFlightRun_DoesNotDelayRed(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  gh.WorkflowRun
	}{
		{"unrelated workflow running", gh.WorkflowRun{ID: 99, Name: "Other", Status: "in_progress"}},
		{"re-run run held for approval", gh.WorkflowRun{ID: 10, Name: "CI", Status: "waiting"}},
	} {
		it := failedFirstTrial()
		it.runs = []gh.WorkflowRun{tc.run}
		eng := infraTestEngine(t, it)
		clock := it.useClock(eng)
		it.onMergeableRead = func(it *infraTrial, sinceRerun int) {
			if sinceRerun == 2 {
				clock.Advance(infraDwell + time.Millisecond)
			}
		}

		result, _ := pollInfraTrial(t, eng)
		if result != TrainCIRed {
			t.Fatalf("%s: result = %v, want red after the settle dwell", tc.name, result)
		}
		if it.rerunMergeableReads < 2 {
			t.Fatalf("%s: red after %d poll(s) since the re-run, before the settle dwell elapsed", tc.name, it.rerunMergeableReads)
		}
	}
}

// A re-run that stays queued is waited for only up to rerunMaxWait past the
// settle dwell — its own bound, not the 4h backstop.
func TestPollTrainCI_RerunStuckQueued_RedAfterMaxWait(t *testing.T) {
	it := failedFirstTrial()
	it.onRerun = func(it *infraTrial) {
		it.runs = []gh.WorkflowRun{{ID: 10, Name: "CI", Status: "queued"}}
	}
	eng := infraTestEngine(t, it)
	const maxWait = 300 * time.Millisecond
	eng.SetRerunMaxWaitForTest(maxWait)
	clock := it.useClock(eng)
	it.onMergeableRead = func(it *infraTrial, sinceRerun int) {
		switch sinceRerun {
		case 2: // past the settle dwell: the queued re-run is waited for
			clock.Advance(infraDwell + time.Millisecond)
		case 6: // past dwell + max wait: the stale failure is the verdict
			clock.Advance(maxWait)
		}
	}

	result, _ := pollInfraTrial(t, eng)
	if result != TrainCIRed {
		t.Fatalf("result = %v, want red once the max wait elapsed", result)
	}
	if it.rerunMergeableReads < 6 {
		t.Fatalf("red after %d poll(s) since the re-run, before dwell + max wait elapsed", it.rerunMergeableReads)
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
