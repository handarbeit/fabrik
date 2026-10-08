package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// gateInfra scripts one item's PR for the stage CI gate: a mutable PR head and
// workflow-run list, plus a record of close/reopen.
type gateInfra struct {
	mu                sync.Mutex
	head              string
	state             string // "open" | "closed"
	mergeable         string
	runs              []gh.WorkflowRun
	closes, reopens   int
	runReads          int
	onReopen          func(g *gateInfra)
	reopenErr         error
	runsErr           error
	merged            bool
	checkRunsOverride []gh.CheckRun
}

func newGateInfra(mergeable string) *gateInfra {
	return &gateInfra{head: "sha1", state: "open", mergeable: mergeable, runs: []gh.WorkflowRun{deadWorkflowRun(1)}}
}

func (g *gateInfra) client() *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return &gh.PRDetails{Number: 5, State: g.state, HeadSHA: g.head}, nil
		},
		fetchPRMergedFn: func(owner, repo string, n int) (bool, error) { return false, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return boolPtr(true), g.mergeable, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.checkRunsOverride, nil
		},
		fetchWorkflowRunsFn: func(owner, repo, sha string) ([]gh.WorkflowRun, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.runReads++
			if g.runsErr != nil {
				return nil, g.runsErr
			}
			return append([]gh.WorkflowRun(nil), g.runs...), nil
		},
		closeIssueFn: func(owner, repo string, n int) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.closes++
			g.state = "closed"
			return nil
		},
		reopenIssueFn: func(owner, repo string, n int) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.reopens++
			if g.reopenErr != nil {
				return g.reopenErr
			}
			g.state = "open"
			if g.onReopen != nil {
				g.onReopen(g)
			}
			return nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
}

func gateStage(waitForCI bool) *stages.Stage {
	return &stages.Stage{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitForCI}
}

func gateEngine(t *testing.T, g *gateInfra) *Engine {
	t.Helper()
	eng := testEngineForMerge(t, g.client())
	eng.SetCIInfraTimingForTest(40*time.Millisecond, 0, 0, -1)
	return eng
}

// "clean" with no required checks plus a startup_failure used to settle Ready
// with CI never run.
func TestSettleForCIGate_CleanStartupFailure_RetriggersNotReady(t *testing.T) {
	g := newGateInfra("clean")
	eng := gateEngine(t, g)

	r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if r.Status == PRMergeReady {
		t.Fatal("a PR whose only workflow run is a startup_failure must never settle Ready")
	}
	if r.Status != PRMergeUnsettled || r.StartupFailure != nil || r.MergeableState != "" {
		t.Fatalf("result = %+v, want Unsettled, no StartupFailure yet, MergeableState omitted", r)
	}
	if g.closes != 1 || g.reopens != 1 {
		t.Fatalf("close/reopen = %d/%d, want 1/1", g.closes, g.reopens)
	}
}

// R3a for the gate: the dead run stays on the head SHA and must not consume a
// second retrigger, and an unstarted reopen is abandoned after the dwell.
func TestSettleForCIGate_OldRunDoesNotConsumeSecondRetrigger_ThenAbandons(t *testing.T) {
	g := newGateInfra("blocked")
	eng := gateEngine(t, g)

	for i := 0; i < 4; i++ {
		r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
		if r.StartupFailure != nil {
			t.Fatalf("poll %d: abandoned before the dwell elapsed", i)
		}
	}
	if g.closes != 1 {
		t.Fatalf("closes = %d, want exactly 1 — the old dead run was counted again", g.closes)
	}
	time.Sleep(60 * time.Millisecond)
	r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if r.StartupFailure == nil {
		t.Fatalf("result = %+v, want StartupFailure after the dwell with no new run", r)
	}
	if r.StartupFailure.Run.ID != 1 || r.StartupFailure.Retriggers != 1 {
		t.Errorf("StartupFailure = %+v", r.StartupFailure)
	}
}

func TestSettleForCIGate_AbandonsAfterBoundWhenEveryRetriggerDiesAgain(t *testing.T) {
	g := newGateInfra("blocked")
	next := int64(2)
	g.onReopen = func(g *gateInfra) { g.runs = append(g.runs, deadWorkflowRun(next)); next++ }
	eng := gateEngine(t, g)

	var r PRSettleResult
	for i := 0; i < 6 && r.StartupFailure == nil; i++ {
		r = eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	}
	if r.StartupFailure == nil {
		t.Fatal("never abandoned")
	}
	if g.closes != maxCIRetriggers {
		t.Fatalf("closes = %d, want %d", g.closes, maxCIRetriggers)
	}
	if r.StartupFailure.Run.ID != 3 {
		t.Errorf("abandon names run %d, want the newest dead run 3", r.StartupFailure.Run.ID)
	}
}

// A push gives CI a fresh chance: the retrigger budget resets with the head.
func TestSettleForCIGate_NewHeadResetsTheWatch(t *testing.T) {
	g := newGateInfra("blocked")
	eng := gateEngine(t, g)
	eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if g.closes != 1 {
		t.Fatalf("setup: closes = %d", g.closes)
	}
	g.mu.Lock()
	g.head = "sha2"
	g.runs = []gh.WorkflowRun{deadWorkflowRun(9)}
	g.mu.Unlock()
	eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if g.closes != 2 {
		t.Fatalf("closes = %d, want a fresh retrigger on the new head", g.closes)
	}
}

// Only a wait_for_ci stage is touched; the plain settle never retriggers.
func TestSettleForCIGate_NonWaitForCIStageAndPlainSettleAreUntouched(t *testing.T) {
	g := newGateInfra("clean")
	eng := gateEngine(t, g)

	if r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(false)); r.Status != PRMergeReady {
		t.Errorf("non-wait_for_ci stage: %+v, want today's Ready", r)
	}
	if r := eng.settlePRMergeState(settleItem(1), gateStage(true)); r.Status != PRMergeReady {
		t.Errorf("plain settle: %+v, want today's Ready", r)
	}
	if g.closes != 0 || g.runReads != 0 {
		t.Errorf("closes=%d runReads=%d — nothing must be read or retriggered", g.closes, g.runReads)
	}
}

func TestSettleForCIGate_RefusedWorkflowRunReadBehavesAsBefore(t *testing.T) {
	g := newGateInfra("clean")
	g.runsErr = fmt.Errorf("x: %w", gh.ErrForbidden)
	eng := gateEngine(t, g)
	if r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true)); r.Status != PRMergeReady {
		t.Fatalf("result = %+v, want today's Ready", r)
	}
	if g.closes != 0 {
		t.Fatal("a refused read must not retrigger")
	}
}

func TestSettleForCIGate_CancelledZeroJobRunIsNotInfrastructure(t *testing.T) {
	g := newGateInfra("clean")
	g.runs = []gh.WorkflowRun{{ID: 1, Status: "completed", Conclusion: "cancelled"}}
	eng := gateEngine(t, g)
	if r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true)); r.Status != PRMergeReady || g.closes != 0 {
		t.Fatalf("result = %+v closes=%d, want today's Ready and no retrigger", r, g.closes)
	}
}

// A PR with real check runs is untouched even on a clean state: the workflow
// runs are only read on the zero-check-run path.
func TestSettleForCIGate_ChecksPresentNeverReadsWorkflowRuns(t *testing.T) {
	g := newGateInfra("clean")
	g.checkRunsOverride = []gh.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}}
	eng := gateEngine(t, g)
	if r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true)); r.Status != PRMergeReady {
		t.Fatalf("result = %+v", r)
	}
	if g.runReads != 0 {
		t.Fatalf("workflow runs read %d time(s) with check runs present", g.runReads)
	}
}

// A reopen that fails leaves the PR closed: report it so the pause says so.
func TestSettleForCIGate_FailedReopenIsReported(t *testing.T) {
	g := newGateInfra("blocked")
	g.reopenErr = fmt.Errorf("boom")
	eng := gateEngine(t, g)
	r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if r.StartupFailure == nil || r.StartupFailure.ReopenErr == nil {
		t.Fatalf("result = %+v, want a StartupFailure carrying the reopen error", r)
	}
	if g.reopens != reopenAttempts {
		t.Errorf("reopen attempts = %d, want %d", g.reopens, reopenAttempts)
	}
}

// The closed window: a PR this engine just closed must not settle as "closed
// without merging" (which would pause the item), only until the grace lapses.
func TestSettleForCIGate_ClosedInsideRetriggerWindowIsTransient(t *testing.T) {
	g := newGateInfra("blocked")
	g.state = "open"
	eng := gateEngine(t, g)
	eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true)) // retrigger → recorded
	g.mu.Lock()
	g.state = "closed" // a lagging read still shows the PR closed
	g.mu.Unlock()

	r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if r.Status != PRMergeUnsettled {
		t.Fatalf("closed inside the window: %+v, want transient Unsettled", r)
	}
	eng.startupWatchMu.Lock()
	for _, st := range eng.startupWatches {
		st.lastRetrigger = time.Now().Add(-2 * ciRetriggerGrace)
	}
	eng.startupWatchMu.Unlock()
	r = eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	if r.Status != PRMergeTerminal {
		t.Fatalf("closed after the window: %+v, want today's Terminal", r)
	}
}

// The handler pauses through the CI pause machinery, naming the startup failure.
func TestHandleMergeAndCIGates_StartupFailure_PausesNamingTheRun(t *testing.T) {
	g := newGateInfra("blocked")
	var mu sync.Mutex
	var comments []string
	mc := g.client()
	mc.addCommentFn = func(_, _ string, _ int, body string) (int, error) {
		mu.Lock()
		comments = append(comments, body)
		mu.Unlock()
		return 1, nil
	}
	waitTrue := true
	eng := testEngineWithStages(t, mc, []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	})
	eng.SetCIInfraTimingForTest(time.Millisecond, 0, 0, -1)

	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, map[string]bool{})
	var claimed bool
	for i := 0; i < 5 && !hasPausedLabel(mc); i++ {
		claimed = eng.handleMergeAndCIGates(pctx)
		time.Sleep(3 * time.Millisecond)
	}
	if !claimed || !hasPausedLabel(mc) {
		t.Fatalf("expected the item to be claimed and paused (claimed=%v)", claimed)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, c := range comments {
		if strings.Contains(c, "CI never started") && strings.Contains(c, "actions/runs/1") {
			found = true
		}
	}
	if !found {
		t.Errorf("pause comment must name the startup-failed run; comments: %v", comments)
	}
}

// After the gate pauses for a startup failure, the retrigger state is reset: a
// workflow fix lands on the base branch and does not move the PR head SHA, so
// without the reset the spent budget and old baseline would make the resumed
// item abandon at once, with no retrigger, and re-pause.
func TestHandleMergeAndCIGates_StartupFailure_ResumeStartsWithFreshRetriggerBudget(t *testing.T) {
	g := newGateInfra("blocked")
	mc := g.client()
	waitTrue := true
	eng := testEngineWithStages(t, mc, []*stages.Stage{
		{Name: "Implement", Order: 1, Prompt: "implement", WaitForCI: &waitTrue},
		{Name: "Review", Order: 2, Prompt: "review"},
	})
	eng.SetCIInfraTimingForTest(time.Millisecond, 0, 0, -1)

	pctx := makeMergeGatePctx(&gh.ProjectBoard{ProjectID: "PVT_1"}, map[string]bool{})
	for i := 0; i < 5 && !hasPausedLabel(mc); i++ {
		eng.handleMergeAndCIGates(pctx)
		time.Sleep(3 * time.Millisecond)
	}
	if !hasPausedLabel(mc) {
		t.Fatal("expected the item to be paused for the startup failure")
	}
	g.mu.Lock()
	closesAtPause := g.closes
	g.mu.Unlock()

	// The human resumes; the head SHA is unchanged and the dead run is still on it.
	time.Sleep(5 * time.Millisecond)
	r := eng.settlePRMergeStateForCIGate(settleItem(1), gateStage(true))
	g.mu.Lock()
	closes := g.closes
	g.mu.Unlock()
	if r.StartupFailure != nil {
		t.Fatalf("resumed item was abandoned at once (%+v) instead of retriggering afresh", r.StartupFailure)
	}
	if closes != closesAtPause+1 {
		t.Fatalf("closes after resume = %d, want %d — no fresh retrigger", closes, closesAtPause+1)
	}
}

func hasPausedLabel(mc *mockGitHubClient) bool {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	for _, c := range mc.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			return true
		}
	}
	return false
}
