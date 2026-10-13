package engine

import (
	"context"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// evalTrialCI is one evaluation: the retrigger / re-run budget spent on an earlier call
// must still be spent on a later one when the same trialCIState is passed in (#2051 —
// this is what lets the per-poll evaluator persist it between polls and across restarts).
func TestEvalTrialCI_RetriggerBudgetCarriesAcrossCalls(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", runs: []gh.WorkflowRun{deadWorkflowRun(1)}}
	eng := infraTestEngine(t, it)
	clk := it.useClock(eng)
	ctx := context.Background()
	var st trialCIState

	res, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if decided {
		t.Fatalf("first evaluation after a retrigger must not decide, got %v", res)
	}
	if it.closes != 1 || it.reopens != 1 || st.sw.retriggers != 1 {
		t.Fatalf("close/reopen/retriggers = %d/%d/%d, want 1/1/1", it.closes, it.reopens, st.sw.retriggers)
	}

	// Same state, only the old dead run visible, inside the new-run dwell: nothing more happens.
	res, _, decided = eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if decided || it.closes != 1 {
		t.Fatalf("second evaluation inside the dwell: decided=%v closes=%d, want undecided and still 1 close", decided, it.closes)
	}

	// A FRESH state would retrigger again — proving the carried state is what held it back.
	var fresh trialCIState
	if _, _, d := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &fresh); d || it.closes != 2 {
		t.Fatalf("fresh state must spend its own retrigger: decided=%v closes=%d", d, it.closes)
	}

	clk.Advance(time.Second)
	res, diag, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIInfra || diag == nil {
		t.Fatalf("after the dwell with no new run: decided=%v res=%v diag=%v, want infra", decided, res, diag)
	}
}

func TestEvalTrialCI_FailedJobRerunStateCarriesAcrossCalls(t *testing.T) {
	it := &infraTrial{mergeable: "blocked", checkRuns: []gh.CheckRun{actionsCheck(11, 7, "build", "failure")},
		runs: []gh.WorkflowRun{healthyWorkflowRun(7)}}
	eng := infraTestEngine(t, it)
	it.useClock(eng)
	ctx := context.Background()
	var st trialCIState

	if _, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st); decided {
		t.Fatal("the first failure must re-run the failed jobs, not decide")
	}
	if !st.rr.done || len(it.rerunIDs) != 1 {
		t.Fatalf("rerun done=%v ids=%v, want one accepted re-run recorded in the state", st.rr.done, it.rerunIDs)
	}
	// Stale failure (same check-run ID) on a later call, inside the dwell: still not a verdict,
	// and no second re-run.
	if _, _, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st); decided || len(it.rerunIDs) != 1 {
		t.Fatalf("stale failure must not be judged nor re-run twice: decided=%v ids=%v", decided, it.rerunIDs)
	}
	// The re-run produced a NEW failing check run: that is red.
	it.mu.Lock()
	it.checkRuns = []gh.CheckRun{actionsCheck(12, 7, "build", "failure")}
	it.mu.Unlock()
	res, diag, decided := eng.evalTrialCI(ctx, "owner", "repo", 42, "sha123", &st)
	if !decided || res != TrainCIRed || diag == nil || len(diag.FailedChecks) != 1 {
		t.Fatalf("a failure produced by the re-run must be red: decided=%v res=%v diag=%+v", decided, res, diag)
	}
}
