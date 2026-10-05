package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestIsStartupFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  gh.WorkflowRun
		want bool
	}{
		{"startup_failure conclusion", gh.WorkflowRun{Status: "completed", Conclusion: "startup_failure"}, true},
		{"completed zero jobs", gh.WorkflowRun{Status: "completed", Conclusion: "failure", JobCount: 0}, true},
		{"completed zero jobs success", gh.WorkflowRun{Status: "completed", Conclusion: "success", JobCount: 0}, true},
		// #2033: a concurrency-group supersession cancels the run before any job.
		{"zero-job cancelled is not infrastructure", gh.WorkflowRun{Status: "completed", Conclusion: "cancelled", JobCount: 0}, false},
		{"completed with jobs", gh.WorkflowRun{Status: "completed", Conclusion: "failure", JobCount: 3}, false},
		{"queued", gh.WorkflowRun{Status: "queued"}, false},
		{"in progress zero jobs yet", gh.WorkflowRun{Status: "in_progress", JobCount: 0}, false},
	} {
		if got := isStartupFailure(tc.run); got != tc.want {
			t.Errorf("%s: isStartupFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

var infraT0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func deadRun(id int64) gh.WorkflowRun {
	return gh.WorkflowRun{ID: id, Name: "CI", Status: "completed", Conclusion: "startup_failure"}
}

func TestStartupWatch_NoRunsIsNone(t *testing.T) {
	var w startupWatch
	if a, _ := w.observe(nil, infraT0, time.Minute); a != startupNone {
		t.Fatalf("action = %v, want none", a)
	}
	// An in-progress run with no check runs yet is today's "keep polling".
	if a, _ := w.observe([]gh.WorkflowRun{{ID: 1, Status: "in_progress"}}, infraT0, time.Minute); a != startupNone {
		t.Fatalf("action = %v, want none", a)
	}
}

// R3a: the startup-failed run stays on the head SHA after the reopen and must
// not consume a second retrigger.
func TestStartupWatch_OldRunDoesNotBurnSecondRetrigger(t *testing.T) {
	var w startupWatch
	runs := []gh.WorkflowRun{deadRun(1)}
	if a, r := w.observe(runs, infraT0, time.Minute); a != startupRetrigger || r.ID != 1 {
		t.Fatalf("first observe = %v %+v, want retrigger of run 1", a, r)
	}
	w.retriggered(runs, infraT0)
	for i := 1; i <= 5; i++ {
		a, _ := w.observe(runs, infraT0.Add(time.Duration(i)*time.Second), time.Minute)
		if a != startupWait {
			t.Fatalf("poll %d with only the old run: action = %v, want wait", i, a)
		}
	}
	if w.retriggers != 1 {
		t.Fatalf("retriggers = %d, want 1", w.retriggers)
	}
}

func TestStartupWatch_BoundThenAbandon(t *testing.T) {
	var w startupWatch
	runs := []gh.WorkflowRun{deadRun(1)}
	for i := 0; i < maxCIRetriggers; i++ {
		a, r := w.observe(runs, infraT0, time.Minute)
		if a != startupRetrigger {
			t.Fatalf("round %d: action = %v, want retrigger", i, a)
		}
		w.retriggered(runs, infraT0)
		runs = append(runs, deadRun(r.ID+100)) // the retrigger produced another dead run
	}
	a, r := w.observe(runs, infraT0, time.Minute)
	if a != startupAbandon {
		t.Fatalf("after the bound: action = %v, want abandon", a)
	}
	if r.ID != 201 {
		t.Errorf("abandon names run %d, want the newest dead run 201", r.ID)
	}
}

// A reopen that starts nothing (types: omits reopened) is abandoned after the
// dwell rather than waiting for the backstop.
func TestStartupWatch_NoNewRunWithinDwellAbandons(t *testing.T) {
	var w startupWatch
	runs := []gh.WorkflowRun{deadRun(1)}
	w.observe(runs, infraT0, time.Minute)
	w.retriggered(runs, infraT0)
	if a, _ := w.observe(runs, infraT0.Add(30*time.Second), time.Minute); a != startupWait {
		t.Fatalf("inside dwell: %v, want wait", a)
	}
	a, r := w.observe(runs, infraT0.Add(61*time.Second), time.Minute)
	if a != startupAbandon || r.ID != 1 {
		t.Fatalf("past dwell: %v %+v, want abandon naming run 1", a, r)
	}
}

func TestStartupWatch_NewHealthyRunClearsTheWait(t *testing.T) {
	var w startupWatch
	runs := []gh.WorkflowRun{deadRun(1)}
	w.observe(runs, infraT0, time.Minute)
	w.retriggered(runs, infraT0)
	runs = append(runs, gh.WorkflowRun{ID: 2, Status: "in_progress"})
	if a, _ := w.observe(runs, infraT0.Add(10*time.Minute), time.Minute); a != startupNone {
		t.Fatalf("a started run must end the wait even past the dwell: %v", a)
	}
}

func TestRetriggerPR(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	e.SetCIInfraTimingForTest(0, 0, 0, -1)

	t.Run("closes then reopens", func(t *testing.T) {
		var calls []string
		mc := &mockGitHubClient{
			closeIssueFn:  func(o, r string, n int) error { calls = append(calls, fmt.Sprintf("close#%d", n)); return nil },
			reopenIssueFn: func(o, r string, n int) error { calls = append(calls, fmt.Sprintf("reopen#%d", n)); return nil },
		}
		e.client = mc
		if err := e.retriggerPR("o/r", "o", "r", 7); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls) != "[close#7 reopen#7]" {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("failed close does not reopen", func(t *testing.T) {
		reopened := false
		e.client = &mockGitHubClient{
			closeIssueFn:  func(o, r string, n int) error { return errors.New("boom") },
			reopenIssueFn: func(o, r string, n int) error { reopened = true; return nil },
		}
		if err := e.retriggerPR("o/r", "o", "r", 7); err == nil || reopened {
			t.Fatalf("err = %v, reopened = %v; want an error and no reopen", err, reopened)
		}
	})

	t.Run("reopen is retried then reported", func(t *testing.T) {
		attempts := 0
		e.client = &mockGitHubClient{
			closeIssueFn:  func(o, r string, n int) error { return nil },
			reopenIssueFn: func(o, r string, n int) error { attempts++; return errors.New("flaky") },
		}
		if err := e.retriggerPR("o/r", "o", "r", 7); err == nil {
			t.Fatal("want an error when the PR cannot be reopened")
		}
		if attempts != reopenAttempts {
			t.Fatalf("attempts = %d, want %d", attempts, reopenAttempts)
		}
	})

	t.Run("reopen succeeds on retry", func(t *testing.T) {
		attempts := 0
		e.client = &mockGitHubClient{
			closeIssueFn: func(o, r string, n int) error { return nil },
			reopenIssueFn: func(o, r string, n int) error {
				attempts++
				if attempts < 2 {
					return errors.New("flaky")
				}
				return nil
			},
		}
		if err := e.retriggerPR("o/r", "o", "r", 7); err != nil {
			t.Fatalf("err = %v, want success on the second attempt", err)
		}
	})
}

func TestFetchWorkflowRunsSoft_PermissionRefusalLogsOnce(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	calls := 0
	e.client = &mockGitHubClient{fetchWorkflowRunsFn: func(o, r, sha string) ([]gh.WorkflowRun, error) {
		calls++
		return nil, fmt.Errorf("x: %w", gh.ErrForbidden)
	}}
	for i := 0; i < 3; i++ {
		if _, ok, refused := e.fetchWorkflowRunsSoft("o/r", "o", "r", "sha"); ok || !refused {
			t.Fatalf("call %d: ok=%v refused=%v, want a refusal", i, ok, refused)
		}
	}
	if !e.actionsDegradeLogged.Load() {
		t.Error("degrade notice never recorded")
	}
	if calls != 3 {
		t.Errorf("client calls = %d, want 3 (the read is retried on every call)", calls)
	}
}

func TestRerunFailedWorkflowRuns(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	failed := []gh.CheckRun{
		{ID: 1, Name: "a", DetailsURL: "https://github.com/o/r/actions/runs/10/job/1"},
		{ID: 2, Name: "b", DetailsURL: "https://github.com/o/r/actions/runs/10/job/2"},
		{ID: 3, Name: "c", DetailsURL: "https://github.com/o/r/actions/runs/11/job/3"},
	}
	var reran []int64
	e.client = &mockGitHubClient{rerunFailedJobsFn: func(o, r string, id int64) error { reran = append(reran, id); return nil }}
	if !e.rerunFailedWorkflowRuns("o/r", "o", "r", failed) {
		t.Fatal("want success")
	}
	if fmt.Sprint(reran) != "[10 11]" {
		t.Fatalf("re-ran %v, want each distinct run once", reran)
	}

	// A third-party failing check has no run to re-run: nothing is re-run.
	reran = nil
	mixed := append([]gh.CheckRun{{ID: 9, Name: "ci/circle", DetailsURL: "https://circleci.com/x"}}, failed...)
	if e.rerunFailedWorkflowRuns("o/r", "o", "r", mixed) || len(reran) != 0 {
		t.Fatalf("third-party failure must stay red without re-running; reran %v", reran)
	}

	// An erroring re-run falls back to red.
	e.client = &mockGitHubClient{rerunFailedJobsFn: func(o, r string, id int64) error { return fmt.Errorf("x: %w", gh.ErrForbidden) }}
	if e.rerunFailedWorkflowRuns("o/r", "o", "r", failed) {
		t.Fatal("a refused re-run must report false")
	}
}

func TestHasNewCheckRun(t *testing.T) {
	seen := map[int64]bool{1: true, 2: true}
	if hasNewCheckRun([]gh.CheckRun{{ID: 1}, {ID: 2}}, seen) {
		t.Error("only stale ids: want false")
	}
	if !hasNewCheckRun([]gh.CheckRun{{ID: 1}, {ID: 5}}, seen) {
		t.Error("a new id: want true")
	}
}

// #2052 review: the stage gate's per-PR retrigger state must not grow without
// bound — an entry not consulted for startupStateTTL is dropped on the next
// consultation, while a fresh one and the PR being consulted survive.
func TestStartupStateForLocked_PrunesStaleEntries(t *testing.T) {
	e := &Engine{}
	e.startupWatchMu.Lock()
	defer e.startupWatchMu.Unlock()
	e.startupStateForLocked("o/r", 1, "sha1")
	e.startupStateForLocked("o/r", 2, "sha2")
	e.startupWatches[startupWatchKey("o/r", 1)].touched = time.Now().Add(-2 * startupStateTTL)

	e.startupStateForLocked("o/r", 3, "sha3")

	if _, ok := e.startupWatches[startupWatchKey("o/r", 1)]; ok {
		t.Errorf("stale entry for PR 1 was not pruned")
	}
	for _, n := range []int{2, 3} {
		if _, ok := e.startupWatches[startupWatchKey("o/r", n)]; !ok {
			t.Errorf("live entry for PR %d was pruned", n)
		}
	}
}
