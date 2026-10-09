package engine

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

type flakeTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *flakeTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *flakeTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// flakeFixture is a stage-gate engine whose PR head has a failing Actions check
// run (workflow run 900), with the check runs replaceable between polls.
type flakeFixture struct {
	eng    *Engine
	client *mockGitHubClient
	clock  *flakeTestClock

	mu        sync.Mutex
	checks    []gh.CheckRun
	head      string
	reruns    []int64
	rerunErr  error
	inFlight  bool
	advanced  map[string]bool
	rerunCall atomic.Int32
}

func newFlakeFixture(t *testing.T) *flakeFixture {
	t.Helper()
	f := &flakeFixture{
		head:     "cafebabe1234",
		advanced: map[string]bool{},
		checks: []gh.CheckRun{
			{ID: 10, Name: "test", Status: "completed", Conclusion: "failure",
				DetailsURL: "https://github.com/owner/repo/actions/runs/900/job/1"},
		},
		clock: &flakeTestClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
	}
	client := ciFailureSettleClient()
	client.fetchLinkedPRFn = func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return &gh.PRDetails{Number: 43, HeadSHA: f.head, State: "open"}, nil
	}
	client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]gh.CheckRun(nil), f.checks...), nil
	}
	client.rerunFailedJobsFn = func(owner, repo string, runID int64) error {
		f.rerunCall.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rerunErr != nil {
			return f.rerunErr
		}
		f.reruns = append(f.reruns, runID)
		return nil
	}
	client.fetchWorkflowRunsFn = func(owner, repo, sha string) ([]gh.WorkflowRun, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.inFlight {
			return []gh.WorkflowRun{{ID: 900, Status: "in_progress", JobCount: 1}}, nil
		}
		return []gh.WorkflowRun{{ID: 900, Status: "completed", Conclusion: "failure", JobCount: 1}}, nil
	}
	f.client = client

	waitTrue := true
	stgs := []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	}
	f.eng = testEngineWithStages(t, client, stgs)
	f.eng.cfg.MaxCiFixCycles = 5
	f.eng.SetClock(f.clock)
	return f
}

// poll runs the gate once and reports whether a CI-fix worker was dispatched.
func (f *flakeFixture) poll(t *testing.T) (dispatched bool) {
	t.Helper()
	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, f.advanced)
	delete(f.advanced, "owner/repo#20")
	if !f.eng.handleMergeAndCIGates(pctx) {
		t.Fatal("handleMergeAndCIGates did not claim the failing item")
	}
	f.eng.wg.Wait()
	return f.advanced["owner/repo#20"]
}

func (f *flakeFixture) rerunCount() int { return int(f.rerunCall.Load()) }

func (f *flakeFixture) setChecks(c ...gh.CheckRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = c
}

func (f *flakeFixture) setHead(h string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = h
}

func failedCheck(id int64) gh.CheckRun {
	return gh.CheckRun{ID: id, Name: "test", Status: "completed", Conclusion: "failure",
		DetailsURL: "https://github.com/owner/repo/actions/runs/900/job/1"}
}

// R1: the first confirmed failure re-runs the failed jobs and dispatches no
// CI-fix worker.
func TestFlakeRerun_R1_FirstFailureRerunsWithoutDispatch(t *testing.T) {
	f := newFlakeFixture(t)
	if f.poll(t) {
		t.Fatal("first failure dispatched a CI-fix worker; want a re-run")
	}
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
	snap, _ := f.eng.store.Get("owner/repo", 20)
	if snap.CIFixCycles("Implement") != 0 {
		t.Errorf("CIFixCycles = %d, want 0 (a re-run is not charged)", snap.CIFixCycles("Implement"))
	}
}

// Neutralised twin of the R1 test: with the re-run switched off the same setup
// dispatches a worker and re-runs nothing, so the R1 test discriminates.
func TestFlakeRerun_R1_Neutralised(t *testing.T) {
	f := newFlakeFixture(t)
	f.eng.SetCIFlakeRerunDisabledForTest(true)
	if !f.poll(t) {
		t.Fatal("neutralised: want a CI-fix dispatch")
	}
	if got := f.rerunCount(); got != 0 {
		t.Fatalf("neutralised: RerunFailedJobs calls = %d, want 0", got)
	}
}

// Stale failing check runs from before the re-run (same IDs) are not counted as
// a new failure within the settle dwell.
func TestFlakeRerun_StaleFailureWithinDwellIsNotCounted(t *testing.T) {
	f := newFlakeFixture(t)
	f.poll(t) // R1 re-run
	f.clock.Advance(30 * time.Second)
	if f.poll(t) {
		t.Fatal("stale failure inside the dwell dispatched a worker")
	}
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

// A re-run still queued/running keeps the gate waiting past the dwell, up to the
// max wait; after that the stale failure is the verdict and a worker dispatches.
func TestFlakeRerun_InFlightExtendsWaitUntilMaxWait(t *testing.T) {
	f := newFlakeFixture(t)
	f.poll(t)
	f.mu.Lock()
	f.inFlight = true
	f.mu.Unlock()

	f.clock.Advance(defaultTrainRerunSettleDwell + time.Minute)
	if f.poll(t) {
		t.Fatal("in-flight re-run past the dwell dispatched a worker")
	}
	f.clock.Advance(defaultTrainRerunMaxWait)
	if !f.poll(t) {
		t.Fatal("re-run still in flight past max wait: want the stale failure to become the verdict")
	}
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

// With the re-run finished (not in flight) and only the stale failure visible,
// the dwell elapsing makes the failure the verdict.
func TestFlakeRerun_StaleFailurePastDwellDispatches(t *testing.T) {
	f := newFlakeFixture(t)
	f.poll(t)
	f.clock.Advance(defaultTrainRerunSettleDwell + time.Second)
	if !f.poll(t) {
		t.Fatal("stale failure past the dwell and no re-run in flight: want a CI-fix dispatch")
	}
}

// A failure after the R1 re-run (a new check-run ID) dispatches the worker at once.
func TestFlakeRerun_FailureAfterR1Dispatches(t *testing.T) {
	f := newFlakeFixture(t)
	f.poll(t)
	f.setChecks(failedCheck(11))
	if !f.poll(t) {
		t.Fatal("new failure after the R1 re-run: want a CI-fix dispatch")
	}
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

// R2: a recorded no-op triggers exactly one re-run, then today's skip applies.
func TestFlakeRerun_R2_NoOpTriggersExactlyOneRerun(t *testing.T) {
	f := newFlakeFixture(t)
	f.poll(t)                    // R1
	f.setChecks(failedCheck(11)) // re-run failed again
	if !f.poll(t) {              // dispatch the worker
		t.Fatal("want a dispatch after the R1 re-run failed")
	}
	f.eng.store.Apply(itemstate.CIFixNoOpRecorded{Repo: "owner/repo", Number: 20, SHA: f.head})

	f.poll(t) // R2 re-run
	if got := f.rerunCount(); got != 2 {
		t.Fatalf("RerunFailedJobs calls = %d, want 2 (R1 + R2)", got)
	}
	f.setChecks(failedCheck(12)) // R2's re-run failed too
	if f.poll(t) {
		t.Fatal("after R2 the existing no-op skip applies: want no dispatch")
	}
	f.clock.Advance(time.Hour)
	f.poll(t)
	if got := f.rerunCount(); got != 2 {
		t.Fatalf("RerunFailedJobs calls = %d, want exactly 2", got)
	}
}

// Neutralised twin of the R2 test: a recorded no-op re-runs nothing and skips.
func TestFlakeRerun_R2_Neutralised(t *testing.T) {
	f := newFlakeFixture(t)
	f.eng.SetCIFlakeRerunDisabledForTest(true)
	f.eng.store.Apply(itemstate.CIFixNoOpRecorded{Repo: "owner/repo", Number: 20, SHA: f.head})
	if f.poll(t) {
		t.Fatal("neutralised: a recorded no-op skips dispatch")
	}
	if got := f.rerunCount(); got != 0 {
		t.Fatalf("neutralised: RerunFailedJobs calls = %d, want 0", got)
	}
}

// R2 on its own (a no-op recorded for a head with no R1 re-run yet) re-runs once.
func TestFlakeRerun_R2_OnlyOncePerHead(t *testing.T) {
	f := newFlakeFixture(t)
	f.eng.store.Apply(itemstate.CIFixNoOpRecorded{Repo: "owner/repo", Number: 20, SHA: f.head})
	f.poll(t)
	f.clock.Advance(time.Hour)
	f.poll(t)
	f.poll(t)
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

// R4: ErrForbidden falls back to dispatch, and the re-run is not retried.
func TestFlakeRerun_ForbiddenFallsBackToDispatch(t *testing.T) {
	f := newFlakeFixture(t)
	f.mu.Lock()
	f.rerunErr = gh.ErrForbidden
	f.mu.Unlock()
	if !f.poll(t) {
		t.Fatal("ErrForbidden: want a CI-fix dispatch")
	}
	f.eng.store.Apply(itemstate.CIFixNoOpRecorded{Repo: "owner/repo", Number: 20, SHA: f.head})
	f.poll(t)
	f.poll(t)
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1 (degraded head is never retried)", got)
	}
}

// R4: a transient API error also degrades without pausing the item.
func TestFlakeRerun_ErrorDegradesWithoutPause(t *testing.T) {
	f := newFlakeFixture(t)
	f.mu.Lock()
	f.rerunErr = errors.New("boom")
	f.mu.Unlock()
	if !f.poll(t) {
		t.Fatal("re-run error: want a CI-fix dispatch")
	}
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	for _, c := range f.client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("a failed re-run must never pause the item")
		}
	}
}

// R4: a failing check run with no Actions run behind it (external CI) falls back.
func TestFlakeRerun_NonActionsCheckRunFallsBack(t *testing.T) {
	f := newFlakeFixture(t)
	f.setChecks(gh.CheckRun{ID: 10, Name: "ci/circle", Status: "completed", Conclusion: "failure",
		DetailsURL: "https://circleci.com/gh/o/r/1"})
	if !f.poll(t) {
		t.Fatal("non-Actions check run: want a CI-fix dispatch")
	}
	if got := f.rerunCount(); got != 0 {
		t.Fatalf("RerunFailedJobs calls = %d, want 0", got)
	}
}

// R3: a new head resets the budget.
func TestFlakeRerun_NewHeadResetsBudget(t *testing.T) {
	f := newFlakeFixture(t)
	f.poll(t)
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	f.setHead("deadbeef5678")
	f.setChecks(failedCheck(20))
	if f.poll(t) {
		t.Fatal("new head's first failure: want a re-run, not a dispatch")
	}
	if got := f.rerunCount(); got != 2 {
		t.Fatalf("calls = %d, want 2 (budget reset for the new head)", got)
	}
}

// A worker already in flight is never second-guessed by a re-run.
func TestFlakeRerun_WorkerInFlightProceeds(t *testing.T) {
	f := newFlakeFixture(t)
	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, f.advanced)
	f.eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 20, StageName: "Implement", StartedAt: time.Now()})
	settle := f.eng.settlePRMergeStateForCIGate(pctx.item, pctx.stage)
	if f.eng.ciFlakeRerun(pctx, settle) {
		t.Fatal("worker in flight: ciFlakeRerun must not claim")
	}
	if got := f.rerunCount(); got != 0 {
		t.Fatalf("calls = %d, want 0", got)
	}
}

// A failure with no failed check run (required-context only) proceeds.
func TestFlakeRerun_NoFailedCheckRunProceeds(t *testing.T) {
	f := newFlakeFixture(t)
	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, f.advanced)
	if f.eng.ciFlakeRerun(pctx, PRSettleResult{PR: &gh.PRDetails{Number: 43, HeadSHA: "abc"}}) {
		t.Fatal("no failed check runs: must not claim")
	}
}

// R3: the poll loop and the settle scan racing on the same head re-run it once.
func TestFlakeRerun_ConcurrentCallsRerunOnce(t *testing.T) {
	f := newFlakeFixture(t)
	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, f.advanced)
	settle := f.eng.settlePRMergeStateForCIGate(pctx.item, pctx.stage)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.eng.ciFlakeRerun(pctx, settle)
		}()
	}
	wg.Wait()
	if got := f.rerunCount(); got != 1 {
		t.Fatalf("RerunFailedJobs calls = %d, want 1", got)
	}
}

func TestFlakeStateForLocked_ResetsOnHeadAndPrunes(t *testing.T) {
	e := &Engine{}
	now := time.Now()
	e.flakeRerunMu.Lock()
	defer e.flakeRerunMu.Unlock()
	st := e.flakeStateForLocked("o/r", 1, "a", now)
	st.r1Done = true
	if !e.flakeStateForLocked("o/r", 1, "a", now).r1Done {
		t.Error("same head: state must persist")
	}
	if e.flakeStateForLocked("o/r", 1, "b", now).r1Done {
		t.Error("new head: state must reset")
	}
	e.flakeStateForLocked("o/r", 2, "x", now)
	e.flakeReruns[startupWatchKey("o/r", 2)].touched = now.Add(-2 * startupStateTTL)
	e.flakeStateForLocked("o/r", 3, "y", now)
	if _, ok := e.flakeReruns[startupWatchKey("o/r", 2)]; ok {
		t.Error("stale entry not pruned")
	}
}
