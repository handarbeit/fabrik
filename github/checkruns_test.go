package github

import "testing"

func TestClassifyCheckRuns_Empty_ReturnsReady(t *testing.T) {
	status, pending, failed := ClassifyCheckRuns(nil)
	if status != CheckRunsReady {
		t.Errorf("expected CheckRunsReady, got %v", status)
	}
	if len(pending) != 0 || len(failed) != 0 {
		t.Errorf("expected no pending/failed runs, got pending=%v failed=%v", pending, failed)
	}
}

func TestClassifyCheckRuns_AllSuccess_ReturnsReady(t *testing.T) {
	status, _, _ := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "success"},
	})
	if status != CheckRunsReady {
		t.Errorf("expected CheckRunsReady, got %v", status)
	}
}

func TestClassifyCheckRuns_Pending_ReturnsPending(t *testing.T) {
	status, pending, _ := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "test", Status: "in_progress"},
	})
	if status != CheckRunsPending {
		t.Errorf("expected CheckRunsPending, got %v", status)
	}
	if len(pending) != 1 || pending[0].Name != "test" {
		t.Errorf("expected pending=[test], got %v", pending)
	}
}

func TestClassifyCheckRuns_Failed_ReturnsFailed(t *testing.T) {
	status, _, failed := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "success"},
	})
	if status != CheckRunsFailed {
		t.Errorf("expected CheckRunsFailed, got %v", status)
	}
	if len(failed) != 1 || failed[0].Name != "build" {
		t.Errorf("expected failed=[build], got %v", failed)
	}
}

func TestClassifyCheckRuns_TimedOutAndActionRequired_CountAsFailed(t *testing.T) {
	status, _, failed := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "a", Status: "completed", Conclusion: "timed_out"},
		{ID: 2, Name: "b", Status: "completed", Conclusion: "action_required"},
	})
	if status != CheckRunsFailed {
		t.Errorf("expected CheckRunsFailed, got %v", status)
	}
	if len(failed) != 2 {
		t.Errorf("expected 2 failed runs, got %d", len(failed))
	}
}

// PendingBeatsFailed_SiblingCheck: a failed check coexisting with a pending
// check of a *different* name must classify as Pending — this is the #958
// leg-1 precedence fix.
func TestClassifyCheckRuns_PendingBeatsFailed_SiblingCheck(t *testing.T) {
	status, _, _ := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "test", Status: "in_progress"},
	})
	if status != CheckRunsPending {
		t.Errorf("expected CheckRunsPending when a pending check coexists with a failed sibling, got %v", status)
	}
}

// PendingBeatsFailed_SameNameRerun: a stale failed run superseded by a fresh
// (higher-ID) pending rerun of the *same* check name must classify as
// Pending, not Failed — the stale entry must be discarded, not counted.
func TestClassifyCheckRuns_PendingBeatsFailed_SameNameRerun(t *testing.T) {
	status, pending, failed := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "build", Status: "in_progress"},
	})
	if status != CheckRunsPending {
		t.Errorf("expected CheckRunsPending, got %v", status)
	}
	if len(failed) != 0 {
		t.Errorf("expected no failed runs (stale entry discarded), got %v", failed)
	}
	if len(pending) != 1 {
		t.Errorf("expected 1 pending run, got %v", pending)
	}
}

// LatestByID_HigherIDWins: when a check name has multiple completed runs
// (rerun after rerun), the highest-ID (most recent) run's conclusion wins.
func TestClassifyCheckRuns_LatestByID_HigherIDWins(t *testing.T) {
	status, _, failed := ClassifyCheckRuns([]CheckRun{
		{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "build", Status: "completed", Conclusion: "success"},
	})
	if status != CheckRunsReady {
		t.Errorf("expected CheckRunsReady (latest rerun succeeded), got %v", status)
	}
	if len(failed) != 0 {
		t.Errorf("expected no failed runs, got %v", failed)
	}

	// Order in the input slice must not matter.
	status2, _, failed2 := ClassifyCheckRuns([]CheckRun{
		{ID: 2, Name: "build", Status: "completed", Conclusion: "success"},
		{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
	})
	if status2 != CheckRunsReady {
		t.Errorf("expected CheckRunsReady regardless of input order, got %v", status2)
	}
	if len(failed2) != 0 {
		t.Errorf("expected no failed runs regardless of input order, got %v", failed2)
	}
}

// TestClassifyCheckRuns_FailedOrder_MatchesFirstAppearance guards against
// non-deterministic map-iteration order in latestCheckRunsByName: with
// several distinct failed check names, the returned failed slice must follow
// each name's first appearance in the input, not an arbitrary map order.
func TestClassifyCheckRuns_FailedOrder_MatchesFirstAppearance(t *testing.T) {
	input := []CheckRun{
		{ID: 1, Name: "zeta", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "alpha", Status: "completed", Conclusion: "failure"},
		{ID: 3, Name: "mid", Status: "completed", Conclusion: "success"},
		{ID: 4, Name: "beta", Status: "completed", Conclusion: "failure"},
	}
	want := []string{"zeta", "alpha", "beta"}

	for i := 0; i < 20; i++ {
		_, _, failed := ClassifyCheckRuns(input)
		if len(failed) != len(want) {
			t.Fatalf("run %d: expected %d failed runs, got %v", i, len(want), failed)
		}
		for j, cr := range failed {
			if cr.Name != want[j] {
				t.Fatalf("run %d: failed order = %v, want names in input order %v", i, failed, want)
			}
		}
	}
}
