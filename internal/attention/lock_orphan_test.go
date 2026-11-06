package attention

import (
	"strings"
	"testing"
	"time"
)

// R4 (#2550): an engine lock label with no worker, past the grace period, is
// stalled — not the gate-hold or retry wait that used to hide it.

func orphanInput(label string, since time.Time, grace time.Duration) Input {
	in := baseInput()
	in.StatusEnteredAt = t0.Add(-2 * time.Hour)
	in.Labels = []string{"stage:Implement:complete", label}
	in.LockOrphan = &LockOrphanFact{Label: label, Since: since, Grace: grace}
	// The competing explanations the incident showed instead of a stall.
	in.Gate = &GateFact{Gate: "feedback", Reason: "holding advance — 2 unprocessed comments", IDs: []string{"c1", "c2"}, Since: t0.Add(-time.Hour)}
	return in
}

func TestLockOrphanPastGraceIsStalled(t *testing.T) {
	grace := 3 * time.Minute
	for _, label := range []string{"fabrik:editing", "fabrik:locked:alice"} {
		res := Classify(orphanInput(label, t0.Add(-grace-time.Second), grace))
		if res.State != Stalled || res.Code != CodeLockOrphaned || res.Rank != RankStalled {
			t.Fatalf("%s: got %s/%s rank=%d: %s", label, res.State, res.Code, res.Rank, res.Summary)
		}
		if !strings.Contains(res.Summary, label) {
			t.Errorf("summary %q should name the label", res.Summary)
		}
	}
}

func TestLockOrphanGraceBoundary(t *testing.T) {
	grace := 3 * time.Minute
	cases := []struct {
		name  string
		since time.Time
		want  State
	}{
		{"just inside the grace", t0.Add(-grace + time.Second), Waiting},
		{"exactly at the grace", t0.Add(-grace), Stalled},
		{"just past the grace", t0.Add(-grace - time.Second), Stalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Classify(orphanInput("fabrik:editing", tc.since, grace))
			if res.State != tc.want {
				t.Fatalf("got %s/%s, want %s", res.State, res.Code, tc.want)
			}
			if tc.want == Waiting && res.Code == CodeLockOrphaned {
				t.Error("inside grace the orphan code must not appear")
			}
		})
	}
}

func TestLockOrphanNotStalledWithAWorker(t *testing.T) {
	in := orphanInput("fabrik:editing", t0.Add(-time.Hour), 3*time.Minute)
	in.HasWorker = true
	in.WorkerStartedAt = t0.Add(-time.Minute)
	if res := Classify(in); res.Code == CodeLockOrphaned || res.State == Stalled {
		t.Fatalf("a worker holds the item: got %s/%s", res.State, res.Code)
	}
	in.HasWorker = false
	in.Conflict = &ConflictFact{Since: t0.Add(-time.Minute)}
	if res := Classify(in); res.Code == CodeLockOrphaned {
		t.Fatalf("a conflict resolution holds the item: got %s/%s", res.State, res.Code)
	}
}

func TestLockOrphanOverridesRetryAndGateRows(t *testing.T) {
	in := orphanInput("fabrik:editing", t0.Add(-time.Hour), 3*time.Minute)
	in.Retry = &RetryFact{Stage: "Implement", Cause: "wall-time-kill", Due: t0.Add(time.Minute)}
	res := Classify(in)
	if res.State != Stalled || res.Code != CodeLockOrphaned {
		t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
	}
}

// Default inputs are unchanged: no fact, no verdict.
func TestLockOrphanAbsentFactChangesNothing(t *testing.T) {
	in := orphanInput("fabrik:editing", t0.Add(-time.Hour), 3*time.Minute)
	in.LockOrphan = nil
	res := Classify(in)
	if res.Code == CodeLockOrphaned {
		t.Fatalf("no fact but got %s/%s", res.State, res.Code)
	}
	if res.State != Waiting || res.Code != CodeGateHold {
		t.Errorf("without the fact the gate-hold wait must stand, got %s/%s", res.State, res.Code)
	}
	// A zero Since (unknown) is never a verdict.
	in.LockOrphan = &LockOrphanFact{Label: "fabrik:editing", Grace: time.Minute}
	if res := Classify(in); res.Code == CodeLockOrphaned {
		t.Errorf("unknown Since must not stall: %s/%s", res.State, res.Code)
	}
}
