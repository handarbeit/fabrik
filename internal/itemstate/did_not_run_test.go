package itemstate

import "testing"

// Tests for the #1812 did-not-run reinvoke refund mutations. All follow the
// ReviewCycleDecremented (#1045) shape: floored at zero, and a decrement on an
// already-zero counter must emit no Change (invariant I6).

func TestApplyDidNotRunDecrements(t *testing.T) {
	type tc struct {
		name string
		inc  Mutation
		dec  Mutation
		get  func(StageState) int
	}
	cases := []tc{
		{
			"ReviewBlocked",
			ReviewBlockedCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"},
			ReviewBlockedCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"},
			func(ss StageState) int { return ss.ReviewBlockedCycles["Review"] },
		},
		{
			"CIFix",
			CIFixCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"},
			CIFixCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"},
			func(ss StageState) int { return ss.CIFixCycles["Review"] },
		},
		{
			"Rebase",
			RebaseCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"},
			RebaseCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"},
			func(ss StageState) int { return ss.RebaseCycles["Review"] },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStoreWithItem(t, testRepo, 1)

			// Fresh item: decrement is a no-op with no Change.
			_, changes, err := s.Apply(c.dec)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(changes) != 0 {
				t.Errorf("decrement on zero counter: expected no Change (I6), got %v", changes)
			}

			applyExpect(t, s, c.inc, StageStateChanged)
			applyExpect(t, s, c.inc, StageStateChanged)
			applyExpect(t, s, c.dec, StageStateChanged)
			if got := c.get(getItem(t, s, testRepo, 1).StageState); got != 1 {
				t.Errorf("counter = %d, want 1", got)
			}

			// Double refund for one increment is harmless: floored at zero.
			applyExpect(t, s, c.dec, StageStateChanged)
			_, changes, err = s.Apply(c.dec)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(changes) != 0 {
				t.Errorf("double decrement at zero: expected no Change (I6), got %v", changes)
			}
			if got := c.get(getItem(t, s, testRepo, 1).StageState); got != 0 {
				t.Errorf("counter = %d, want 0 (floored)", got)
			}
		})
	}
}

func TestApplyDidNotRunReinvokeRecordedAndCleared(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, DidNotRunReinvokeRecorded{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	applyExpect(t, s, DidNotRunReinvokeRecorded{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	applyExpect(t, s, DidNotRunReinvokeRecorded{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)

	snap, err := s.Get(testRepo, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := snap.DidNotRunReinvokes("Review"); got != 2 {
		t.Errorf("DidNotRunReinvokes(Review) = %d, want 2", got)
	}

	// EngineCyclesCleared resets only the named stage.
	applyExpect(t, s, EngineCyclesCleared{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	snap, err = s.Get(testRepo, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := snap.DidNotRunReinvokes("Review"); got != 0 {
		t.Errorf("DidNotRunReinvokes(Review) after clear = %d, want 0", got)
	}
	if got := snap.DidNotRunReinvokes("Validate"); got != 1 {
		t.Errorf("DidNotRunReinvokes(Validate) = %d, want 1 (untouched)", got)
	}
}

func TestApplyDidNotRunReinvokesReset(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	// Zero tally: no-op, no Change (I6).
	applyExpect(t, s, DidNotRunReinvokesReset{Repo: testRepo, Number: 1, StageName: "Review"}, 0)

	applyExpect(t, s, DidNotRunReinvokeRecorded{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	applyExpect(t, s, DidNotRunReinvokeRecorded{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	applyExpect(t, s, DidNotRunReinvokesReset{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	// Idempotent.
	applyExpect(t, s, DidNotRunReinvokesReset{Repo: testRepo, Number: 1, StageName: "Review"}, 0)

	snap, err := s.Get(testRepo, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := snap.DidNotRunReinvokes("Review"); got != 0 {
		t.Errorf("DidNotRunReinvokes(Review) = %d, want 0", got)
	}
	if got := snap.DidNotRunReinvokes("Validate"); got != 1 {
		t.Errorf("DidNotRunReinvokes(Validate) = %d, want 1 (untouched)", got)
	}
}
