//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return ts
}

func TestParseCheckRunTimings(t *testing.T) {
	body := `{"total_count":3,"check_runs":[
	  {"name":"late-check-fast","status":"completed","conclusion":"success","started_at":"2026-09-25T21:10:00Z","completed_at":"2026-09-25T21:10:20Z"},
	  {"name":"late-check-slow","status":"in_progress","conclusion":null,"started_at":"2026-09-25T21:10:30Z","completed_at":null},
	  {"name":"queued-one","status":"queued","conclusion":null,"started_at":null,"completed_at":null}
	]}`
	runs, err := parseCheckRunTimings([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(runs))
	}
	if runs[0].Conclusion != "success" || !runs[0].CompletedAt.Equal(mustTime(t, "2026-09-25T21:10:20Z")) {
		t.Errorf("run 0 = %+v", runs[0])
	}
	if runs[1].Conclusion != "" || !runs[1].CompletedAt.IsZero() || runs[1].StartedAt.IsZero() {
		t.Errorf("in-progress run should have null completed_at/conclusion, got %+v", runs[1])
	}
	if !runs[2].StartedAt.IsZero() {
		t.Errorf("queued run should have zero started_at, got %+v", runs[2])
	}
}

func TestParseCheckRunTimingsErrors(t *testing.T) {
	if _, err := parseCheckRunTimings([]byte(`not json`)); err == nil {
		t.Error("want error for malformed JSON")
	}
	if _, err := parseCheckRunTimings([]byte(`{"check_runs":[{"name":"x","started_at":"yesterday"}]}`)); err == nil {
		t.Error("want error for malformed timestamp")
	}
}

func TestEarliestLabeledAt(t *testing.T) {
	// Two concatenated pages (as --paginate emits), a non-label event, a
	// different label, and a re-application of the wanted label later.
	body := `[{"event":"labeled","created_at":"2026-09-25T21:30:00Z","label":{"name":"fabrik:awaiting-ci"}},
	          {"event":"commented","created_at":"2026-09-25T21:00:00Z"},
	          {"event":"labeled","created_at":"2026-09-25T21:05:00Z","label":{"name":"other"}}]
	         [{"event":"unlabeled","created_at":"2026-09-25T21:40:00Z","label":{"name":"fabrik:awaiting-ci"}},
	          {"event":"labeled","created_at":"2026-09-25T21:20:00Z","label":{"name":"fabrik:awaiting-ci"}}]`
	at, found, err := earliestLabeledAt([]byte(body), "fabrik:awaiting-ci")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if want := mustTime(t, "2026-09-25T21:20:00Z"); !at.Equal(want) {
		t.Errorf("earliest = %s, want %s", at, want)
	}
}

func TestEarliestLabeledAtNotFoundAndErrors(t *testing.T) {
	_, found, err := earliestLabeledAt([]byte(`[{"event":"labeled","created_at":"2026-09-25T21:05:00Z","label":{"name":"other"}}]`), "fabrik:awaiting-ci")
	if err != nil || found {
		t.Errorf("found=%v err=%v, want not found, nil", found, err)
	}
	if _, found, err := earliestLabeledAt(nil, "x"); err != nil || found {
		t.Errorf("empty body: found=%v err=%v", found, err)
	}
	if _, _, err := earliestLabeledAt([]byte(`{oops`), "x"); err == nil {
		t.Error("want error for malformed JSON")
	}
	if _, _, err := earliestLabeledAt([]byte(`[{"event":"labeled","created_at":"nope","label":{"name":"x"}}]`), "x"); err == nil {
		t.Error("want error for malformed created_at")
	}
}

func TestLatestRunNamedPicksMostRecentStart(t *testing.T) {
	runs := []CheckRunTiming{
		{Name: "a", StartedAt: mustTime(t, "2026-09-25T21:00:00Z"), Conclusion: "failure"},
		{Name: "a", StartedAt: mustTime(t, "2026-09-25T21:10:00Z"), Conclusion: "success"},
		{Name: "b", StartedAt: mustTime(t, "2026-09-25T21:20:00Z")},
	}
	got, ok := latestRunNamed(runs, "a")
	if !ok || got.Conclusion != "success" {
		t.Errorf("got %+v ok=%v, want the later run", got, ok)
	}
	if _, ok := latestRunNamed(runs, "missing"); ok {
		t.Error("missing name should not be found")
	}
}

// lateCheckFixture is a healthy timeline: the gate goes active at 21:10:00, the
// fast job completes at 21:10:20, the late job starts at 21:10:40 and completes
// four minutes later, and the gate clears at 21:15:00.
func lateCheckFixture(t *testing.T) (runs []CheckRunTiming, awaitingCI, validateComplete time.Time) {
	t.Helper()
	runs = []CheckRunTiming{
		{Name: lateCheckFastName, Status: "completed", Conclusion: "success",
			StartedAt: mustTime(t, "2026-09-25T21:09:50Z"), CompletedAt: mustTime(t, "2026-09-25T21:10:20Z")},
		{Name: lateCheckSlowName, Status: "completed", Conclusion: "success",
			StartedAt: mustTime(t, "2026-09-25T21:10:40Z"), CompletedAt: mustTime(t, "2026-09-25T21:14:45Z")},
	}
	return runs, mustTime(t, "2026-09-25T21:10:00Z"), mustTime(t, "2026-09-25T21:15:00Z")
}

func TestCheckLateCheckOrdering(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		runs, ci, done := lateCheckFixture(t)
		if err := checkLateCheckOrdering(runs, ci, done); err != nil {
			t.Fatalf("healthy timeline rejected: %v", err)
		}
	})

	t.Run("equality tolerated", func(t *testing.T) {
		runs, ci, _ := lateCheckFixture(t)
		// Label applied in the same second the late job completed; late job
		// started the same second the fast job completed; fast completed the
		// same second the gate went active.
		runs[1].StartedAt = runs[0].CompletedAt
		ci = runs[0].CompletedAt
		if err := checkLateCheckOrdering(runs, ci, runs[1].CompletedAt); err != nil {
			t.Fatalf("equal timestamps must be tolerated: %v", err)
		}
	})

	t.Run("A2 premature clear", func(t *testing.T) {
		// The pre-#1822 shape: the gate cleared as soon as the fast job was
		// green, well before the late job finished.
		runs, ci, _ := lateCheckFixture(t)
		err := checkLateCheckOrdering(runs, ci, mustTime(t, "2026-09-25T21:10:30Z"))
		if err == nil || !strings.Contains(err.Error(), "A2") {
			t.Fatalf("want A2 failure, got %v", err)
		}
	})

	t.Run("A1 no window", func(t *testing.T) {
		runs, ci, done := lateCheckFixture(t)
		runs[1].StartedAt = mustTime(t, "2026-09-25T21:10:00Z") // before fast completed
		err := checkLateCheckOrdering(runs, ci, done)
		if err == nil || !strings.Contains(err.Error(), "A1") {
			t.Fatalf("want A1 failure, got %v", err)
		}
	})

	t.Run("A3 vacuity", func(t *testing.T) {
		runs, _, done := lateCheckFixture(t)
		// Gate only went active after the fast job had already finished.
		err := checkLateCheckOrdering(runs, mustTime(t, "2026-09-25T21:12:00Z"), done)
		if err == nil || !strings.Contains(err.Error(), "INCONCLUSIVE") {
			t.Fatalf("want inconclusive failure, got %v", err)
		}
	})

	t.Run("missing fast run", func(t *testing.T) {
		runs, ci, done := lateCheckFixture(t)
		err := checkLateCheckOrdering(runs[1:], ci, done)
		if err == nil || !strings.Contains(err.Error(), lateCheckFastName) {
			t.Fatalf("want missing-fast failure, got %v", err)
		}
	})

	t.Run("missing late run", func(t *testing.T) {
		runs, ci, done := lateCheckFixture(t)
		err := checkLateCheckOrdering(runs[:1], ci, done)
		if err == nil || !strings.Contains(err.Error(), lateCheckSlowName) {
			t.Fatalf("want missing-late failure, got %v", err)
		}
	})

	t.Run("late run still outstanding", func(t *testing.T) {
		runs, ci, done := lateCheckFixture(t)
		runs[1].Status, runs[1].Conclusion, runs[1].CompletedAt = "in_progress", "", time.Time{}
		err := checkLateCheckOrdering(runs, ci, done)
		if err == nil || !strings.Contains(err.Error(), "not completed") {
			t.Fatalf("want not-completed failure, got %v", err)
		}
	})

	t.Run("late run failed", func(t *testing.T) {
		runs, ci, done := lateCheckFixture(t)
		runs[1].Conclusion = "failure"
		err := checkLateCheckOrdering(runs, ci, done)
		if err == nil || !strings.Contains(err.Error(), "want success") {
			t.Fatalf("want conclusion failure, got %v", err)
		}
	})
}
