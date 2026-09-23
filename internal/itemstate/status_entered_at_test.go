package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ---- StatusEnteredAt (#1833) ----
//
// Verifies the record-on-write timestamp is stamped exactly once per genuine
// Status transition, at each of the five gated mutation sites, and left
// untouched by a no-op re-apply of the same Status. This is what makes
// engine's Queued-batch ordering stable poll-to-poll rather than merely
// randomized less often.

func withinTestTolerance(t *testing.T, got time.Time) {
	t.Helper()
	if got.IsZero() {
		t.Fatal("StatusEnteredAt is zero; expected it to be stamped to approximately now")
	}
	if time.Since(got) > 5*time.Second {
		t.Errorf("StatusEnteredAt = %v; expected approximately now", got)
	}
}

func TestStatusEnteredAtStampedByProjectItem(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	pi := testProjectItem(testRepo, 1)
	pi.Status = "Review"
	if _, _, err := s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi}); err != nil {
		t.Fatalf("Apply(ItemDeepFetched): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.After(before) {
		t.Errorf("StatusEnteredAt not advanced by a genuine Status transition: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtUnchangedByNoOpProjectItem(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	// Re-apply the same Status ("Implement", from testProjectItem) — a no-op.
	pi := testProjectItem(testRepo, 1)
	if _, _, err := s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: pi}); err != nil {
		t.Fatalf("Apply(ItemDeepFetched): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.Equal(before) {
		t.Errorf("StatusEnteredAt changed on a no-op Status re-apply: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtStampedByShallowBoardItemUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	pi := testProjectItem(testRepo, 1)
	pi.Status = "Queued"
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatalf("Apply(ShallowBoardItemUpdated): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.After(before) {
		t.Errorf("StatusEnteredAt not advanced by ShallowBoardItemUpdated status change: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtUnchangedByNoOpShallowBoardItemUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	pi := testProjectItem(testRepo, 1) // Status unchanged ("Implement")
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatalf("Apply(ShallowBoardItemUpdated): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.Equal(before) {
		t.Errorf("StatusEnteredAt changed on a no-op ShallowBoardItemUpdated: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtStampedByProbeBoardItemUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	probe := gh.BoardProbeItem{Repo: testRepo, Number: 1, Status: "Validate"}
	if _, _, err := s.Apply(ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe}); err != nil {
		t.Fatalf("Apply(ProbeBoardItemUpdated): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.After(before) {
		t.Errorf("StatusEnteredAt not advanced by ProbeBoardItemUpdated status change: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtUnchangedByNoOpProbeBoardItemUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	probe := gh.BoardProbeItem{Repo: testRepo, Number: 1, Status: "Implement"} // unchanged
	if _, _, err := s.Apply(ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe}); err != nil {
		t.Fatalf("Apply(ProbeBoardItemUpdated): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.Equal(before) {
		t.Errorf("StatusEnteredAt changed on a no-op ProbeBoardItemUpdated: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtStampedByLocalStatusUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	if _, _, err := s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"}); err != nil {
		t.Fatalf("Apply(LocalStatusUpdated): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.After(before) {
		t.Errorf("StatusEnteredAt not advanced by LocalStatusUpdated: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtUnchangedByNoOpLocalStatusUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	if _, _, err := s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Implement"}); err != nil {
		t.Fatalf("Apply(LocalStatusUpdated): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.Equal(before) {
		t.Errorf("StatusEnteredAt changed on a no-op LocalStatusUpdated: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtStampedByProjectV2ItemEdited(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_test_1833"
	if _, _, err := s.Apply(IssueOpened{Item: pi}); err != nil {
		t.Fatalf("seed IssueOpened: %v", err)
	}
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	if _, _, err := s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_test_1833", NewStatus: "Done"}); err != nil {
		t.Fatalf("Apply(ProjectV2ItemEdited): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.After(before) {
		t.Errorf("StatusEnteredAt not advanced by ProjectV2ItemEdited: before=%v after=%v", before, after)
	}
}

func TestStatusEnteredAtUnchangedByNoOpProjectV2ItemEdited(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_test_1833_noop"
	if _, _, err := s.Apply(IssueOpened{Item: pi}); err != nil {
		t.Fatalf("seed IssueOpened: %v", err)
	}
	before := getItem(t, s, testRepo, 1).StatusEnteredAt
	withinTestTolerance(t, before)

	time.Sleep(2 * time.Millisecond)

	if _, _, err := s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_test_1833_noop", NewStatus: "Implement"}); err != nil {
		t.Fatalf("Apply(ProjectV2ItemEdited): %v", err)
	}

	after := getItem(t, s, testRepo, 1).StatusEnteredAt
	if !after.Equal(before) {
		t.Errorf("StatusEnteredAt changed on a no-op ProjectV2ItemEdited: before=%v after=%v", before, after)
	}
}

// TestStatusEnteredAtOnResetFollowsInputSliceOrder pins the exact behavior at
// engine restart / bootstrap (Pruefer review finding on #1833's PR): Store.Reset
// gives every already-Queued item a fresh "now" StatusEnteredAt, since each is a
// brand-new zero-value ItemState transitioning from "" — there is no durable
// cross-restart record of true entry time (see ADR-1833's "Restart resets the
// clock"). What this test establishes is that the resulting order is NOT the
// Store.All()-style Go map randomization the merge train's sort exists to defeat:
// Reset (store.go) iterates `for i := range items` over the caller-supplied
// slice — deterministic, sequential, monotonically increasing time.Now() calls —
// so the post-restart ordering reflects the board-fetch slice order (e.g. GraphQL
// board position), not an arbitrary map shuffle. It is still not true historical
// entry order, and a restart can still cause a one-time batch reshuffle (bounded
// to once per restart, unlike the pre-#1833 defect's per-poll churn), but it is
// not random.
func TestStatusEnteredAtOnResetFollowsInputSliceOrder(t *testing.T) {
	s := NewStore(nil)
	items := []gh.ProjectItem{
		testProjectItem(testRepo, 3),
		testProjectItem(testRepo, 1),
		testProjectItem(testRepo, 2),
	}
	for i := range items {
		items[i].Status = "Queued"
	}
	s.Reset(items)

	t3 := getItem(t, s, testRepo, 3).StatusEnteredAt
	t1 := getItem(t, s, testRepo, 1).StatusEnteredAt
	t2 := getItem(t, s, testRepo, 2).StatusEnteredAt

	for name, got := range map[string]time.Time{"3": t3, "1": t1, "2": t2} {
		if got.IsZero() {
			t.Fatalf("item %s: StatusEnteredAt is zero after Reset; expected a fresh stamp", name)
		}
	}
	if !(t3.Before(t1) || t3.Equal(t1)) || !(t1.Before(t2) || t1.Equal(t2)) {
		t.Errorf("expected StatusEnteredAt to follow Reset's input slice order (3, 1, 2), got 3=%v 1=%v 2=%v", t3, t1, t2)
	}
}

// TestStatusEnteredAtSurvivesSnapshotTranslation confirms the field is a plain
// scalar copy through Snapshot/State() — no deep-copy machinery is needed for
// it (unlike the slice/map fields newSnapshot explicitly re-copies).
func TestStatusEnteredAtSurvivesSnapshotTranslation(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	want := getItem(t, s, testRepo, 1).StatusEnteredAt
	if want.IsZero() {
		t.Fatal("expected StatusEnteredAt to be stamped on first population")
	}

	snap, err := s.Get(testRepo, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := snap.State().StatusEnteredAt
	if !got.Equal(want) {
		t.Errorf("StatusEnteredAt did not survive Snapshot round-trip: got=%v want=%v", got, want)
	}
}
