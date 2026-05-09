package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ---- helpers ----

func testProjectItem(repo string, number int) gh.ProjectItem {
	return gh.ProjectItem{
		Repo:      repo,
		Number:    number,
		Title:     "Test issue",
		Body:      "Test body",
		URL:       "https://github.com/" + repo + "/issues/" + itoa(number),
		Author:    "user",
		Labels:    []string{"bug"},
		Assignees: []string{"alice"},
		Status:    "Implement",
		UpdatedAt: time.Unix(1000, 0),
	}
}

func applyOpened(t *testing.T, s *Store, repo string, number int) Snapshot {
	t.Helper()
	snap, _, err := s.Apply(IssueOpened{Item: testProjectItem(repo, number)})
	if err != nil {
		t.Fatalf("Apply(IssueOpened): %v", err)
	}
	return snap
}

// ---- I1: Every state mutation flows through Apply; Snapshot is immutable ----

// TestSnapshotDoesNotAliasStore verifies that mutating fields on a Snapshot's
// internal state value does not affect the Store (invariant I1).
func TestSnapshotDoesNotAliasStore(t *testing.T) {
	s := NewStore(nil)
	snap := applyOpened(t, s, "owner/repo", 1)

	// Mutate the returned snapshot state — should not affect the store.
	st := snap.State()
	st.Status = "MUTATED"
	st.Labels = append(st.Labels, "extra")

	got, err := s.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State().Status == "MUTATED" {
		t.Error("Snapshot mutation bled into Store (Status)")
	}
	for _, l := range got.State().Labels {
		if l == "extra" {
			t.Error("Snapshot mutation bled into Store (Labels)")
		}
	}
}

// TestStateReturnIsDeepCopy verifies that mutating an element inside a slice
// returned by State() does not affect the Snapshot (covers index-mutation,
// not just append — the case that a shallow State() copy would fail).
func TestStateReturnIsDeepCopy(t *testing.T) {
	s := NewStore(nil)
	snap := applyOpened(t, s, "owner/repo", 10)

	// Index-assign into the Labels slice returned by State().
	st := snap.State()
	if len(st.Labels) == 0 {
		t.Skip("no labels to mutate")
	}
	original := st.Labels[0]
	st.Labels[0] = "MUTATED_IN_PLACE"

	// The Snapshot itself must be unchanged.
	st2 := snap.State()
	if st2.Labels[0] != original {
		t.Errorf("State() slice mutation affected Snapshot: got %q; want %q", st2.Labels[0], original)
	}
}

// TestSnapshotLabelSliceIsIndependent verifies that appending to a Labels slice
// returned by a Snapshot does not mutate the Snapshot itself or the Store.
func TestSnapshotLabelSliceIsIndependent(t *testing.T) {
	s := NewStore(nil)
	applyOpened(t, s, "owner/repo", 2)

	snap, err := s.Get("owner/repo", 2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	labels := snap.Labels()
	labels = append(labels, "injected")

	// The store copy must be unaffected.
	snap2, _ := s.Get("owner/repo", 2)
	for _, l := range snap2.Labels() {
		if l == "injected" {
			t.Error("appending to Labels() slice mutated the store")
		}
	}
}

// ---- I5: Snapshots are immutable and consistent ----

// TestHeldSnapshotUnchangedAfterMutations verifies that a Snapshot taken before
// subsequent Apply calls retains its original values (invariant I5).
func TestHeldSnapshotUnchangedAfterMutations(t *testing.T) {
	s := NewStore(nil)
	held := applyOpened(t, s, "owner/repo", 3)
	originalStatus := held.State().Status // "Implement"

	// Apply several mutations that change Status.
	for _, newStatus := range []string{"Review", "Validate", "Done"} {
		if _, _, err := s.Apply(LocalStatusUpdated{Repo: "owner/repo", Number: 3, NewStatus: newStatus}); err != nil {
			t.Fatalf("Apply(LocalStatusUpdated %s): %v", newStatus, err)
		}
	}

	// Original snapshot must still reflect the value at the time it was taken.
	if held.State().Status != originalStatus {
		t.Errorf("held snapshot Status = %q; want %q", held.State().Status, originalStatus)
	}
}

// TestHeldSnapshotCommentsUnchangedAfterAddition verifies that a held Snapshot's
// Comments slice is not affected by subsequent IssueCommentCreated mutations.
func TestHeldSnapshotCommentsUnchangedAfterAddition(t *testing.T) {
	s := NewStore(nil)
	held := applyOpened(t, s, "owner/repo", 4)
	originalCount := len(held.State().Comments)

	for i := 0; i < 5; i++ {
		s.Apply(IssueCommentCreated{
			Repo:    "owner/repo",
			Number:  4,
			Comment: gh.Comment{ID: itoa(i), Body: "comment"},
		})
	}

	if got := len(held.State().Comments); got != originalCount {
		t.Errorf("held snapshot Comments len = %d; want %d", got, originalCount)
	}
}

// TestHeldSnapshotLinkedPRUnchangedAfterReview verifies that a held Snapshot's
// LinkedPR is not affected by subsequent PRReviewSubmitted mutations.
func TestHeldSnapshotLinkedPRUnchangedAfterReview(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: func() gh.ProjectItem {
		pi := testProjectItem("owner/repo", 5)
		pi.LinkedPRNumber = 42
		return pi
	}()})
	held, _ := s.Get("owner/repo", 5)
	heldLinkedPR := held.LinkedPR()
	reviewsBefore := 0
	if heldLinkedPR != nil {
		reviewsBefore = len(heldLinkedPR.Reviews)
	}

	s.Apply(PRReviewSubmitted{
		Repo:   "owner/repo",
		Number: 5,
		Review: gh.PRReview{Author: "reviewer", State: "APPROVED"},
	})

	// Re-fetch held snapshot from the original variable — must be unchanged.
	var reviewsAfter int
	if lpr := held.LinkedPR(); lpr != nil {
		reviewsAfter = len(lpr.Reviews)
	}
	if reviewsAfter != reviewsBefore {
		t.Errorf("held snapshot LinkedPR reviews changed: before %d, after %d", reviewsBefore, reviewsAfter)
	}
}

// ---- TokenUsage field alignment with engine.TokenUsage ----

// TestTokenUsageFields is a compile-time check that our TokenUsage fields match
// the field names from engine.TokenUsage. If this test fails to compile, the
// Phase 3-E wire-in will require renaming.
func TestTokenUsageFields(t *testing.T) {
	var u TokenUsage
	// Assign each field to confirm the names exist at compile time.
	u.InputTokens = 1
	u.OutputTokens = 2
	u.CacheCreationTokens = 3
	u.CacheReadTokens = 4
	u.CostUSD = 1.5
	u.TurnsUsed = 10
	u.MaxTurns = 50
	_ = u
}

// TestItemKeyFor verifies the canonical item key format.
func TestItemKeyFor(t *testing.T) {
	cases := []struct {
		repo   string
		number int
		want   string
	}{
		{"owner/repo", 1, "owner/repo#1"},
		{"org/myproject", 999, "org/myproject#999"},
	}
	for _, c := range cases {
		got := itemKeyFor(c.repo, c.number)
		if got != c.want {
			t.Errorf("itemKeyFor(%q, %d) = %q; want %q", c.repo, c.number, got, c.want)
		}
	}
}

// TestParseKey verifies round-trip key parsing.
func TestParseKey(t *testing.T) {
	repo, number := parseKey("owner/repo#42")
	if repo != "owner/repo" || number != 42 {
		t.Errorf("parseKey = (%q, %d); want (\"owner/repo\", 42)", repo, number)
	}
}
