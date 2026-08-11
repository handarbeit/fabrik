package itemstate

import (
	"reflect"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

const testRepo = "owner/repo"

func newStoreWithItem(t *testing.T, repo string, number int) *Store {
	t.Helper()
	s := NewStore(nil)
	if _, _, err := s.Apply(IssueOpened{Item: testProjectItem(repo, number)}); err != nil {
		t.Fatalf("seed IssueOpened: %v", err)
	}
	return s
}

func getItem(t *testing.T, s *Store, repo string, number int) ItemState {
	t.Helper()
	snap, err := s.Get(repo, number)
	if err != nil {
		t.Fatalf("Get(%q, %d): %v", repo, number, err)
	}
	return snap.State()
}

func applyExpect(t *testing.T, s *Store, m Mutation, wantFlags ChangeFlags) Snapshot {
	t.Helper()
	snap, changes, err := s.Apply(m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if wantFlags != 0 {
		if len(changes) == 0 {
			t.Fatalf("Apply(%T): expected Change with flags %b, got no changes", m, wantFlags)
		}
		if changes[0].Fields&wantFlags == 0 {
			t.Errorf("Apply(%T): flags %b does not include expected %b", m, changes[0].Fields, wantFlags)
		}
	}
	return snap
}

// ---- IssueOpened ----

func TestApplyIssueOpened(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_abc"
	snap := applyExpect(t, s, IssueOpened{Item: pi}, TitleBodyChanged)
	st := snap.State()
	if st.Title != "Test issue" {
		t.Errorf("Title = %q; want %q", st.Title, "Test issue")
	}
	if st.Status != "Implement" {
		t.Errorf("Status = %q; want %q", st.Status, "Implement")
	}
	if st.Repo != testRepo || st.Number != 1 {
		t.Errorf("Repo/Number = %q/%d; want %q/1", st.Repo, st.Number, testRepo)
	}
}

// ---- IssueLabeled / IssueUnlabeled ----

func TestApplyIssueLabeled(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, IssueLabeled{Repo: testRepo, Number: 1, Label: "enhancement"}, LabelsChanged)
	st := getItem(t, s, testRepo, 1)
	if !containsString(st.Labels, "enhancement") {
		t.Error("label 'enhancement' not added")
	}
}

func TestApplyIssueUnlabeled(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1) // seeded with Labels: ["bug"]
	applyExpect(t, s, IssueUnlabeled{Repo: testRepo, Number: 1, Label: "bug"}, LabelsChanged)
	st := getItem(t, s, testRepo, 1)
	if containsString(st.Labels, "bug") {
		t.Error("label 'bug' not removed")
	}
}

// ---- IssueClosed / IssueReopened ----

func TestApplyIssueClosed(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, IssueClosed{Repo: testRepo, Number: 1}, StateChanged)
	st := getItem(t, s, testRepo, 1)
	if !st.IsClosed || st.State != "closed" {
		t.Error("IssueClosed did not set IsClosed/State")
	}
}

func TestApplyIssueReopened(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(IssueClosed{Repo: testRepo, Number: 1})
	applyExpect(t, s, IssueReopened{Repo: testRepo, Number: 1}, StateChanged)
	st := getItem(t, s, testRepo, 1)
	if st.IsClosed || st.State != "open" {
		t.Error("IssueReopened did not restore state")
	}
}

// ---- IssueCommentCreated ----

func TestApplyIssueCommentCreated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "c1", Body: "hello"}
	applyExpect(t, s, IssueCommentCreated{Repo: testRepo, Number: 1, Comment: c}, CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	found := false
	for _, got := range st.Comments {
		if got.ID == "c1" {
			found = true
		}
	}
	if !found {
		t.Error("comment not appended")
	}
}

// ---- PRReviewSubmitted ----

func TestApplyPRReviewSubmitted(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	rev := gh.PRReview{Author: "bob", State: "APPROVED"}
	applyExpect(t, s, PRReviewSubmitted{Repo: testRepo, Number: 1, Review: rev}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || len(st.LinkedPR.Reviews) == 0 {
		t.Fatal("LinkedPR.Reviews not set")
	}
	if st.LinkedPR.Reviews[0].Author != "bob" {
		t.Errorf("Reviews[0].Author = %q; want %q", st.LinkedPR.Reviews[0].Author, "bob")
	}
}

// TestApplyPRReviewSubmittedDoesNotSetPRStateChanged is a regression guard: a
// new PR review is not a PR *state* transition (merged/closed/draft<->ready),
// so it must not set PRStateChanged — consumers keying off that narrower flag
// (e.g. the comment-processing circuit breaker's PR-state reset trigger,
// #1089) would otherwise reset on every review, defeating their purpose.
func TestApplyPRReviewSubmittedDoesNotSetPRStateChanged(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	rev := gh.PRReview{Author: "bob", State: "APPROVED"}
	_, changes, err := s.Apply(PRReviewSubmitted{Repo: testRepo, Number: 1, Review: rev})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected a Change")
	}
	if changes[0].Fields&PRStateChanged != 0 {
		t.Errorf("Fields %b unexpectedly includes PRStateChanged", changes[0].Fields)
	}
}

// ---- PRReviewCommentCreated ----

func TestApplyPRReviewCommentCreated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "rc1", Body: "nit"}
	applyExpect(t, s, PRReviewCommentCreated{Repo: testRepo, PRNumber: 1, Comment: c}, LinkedPRChanged|CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || len(st.LinkedPR.ThreadComments) == 0 {
		t.Fatal("LinkedPR.ThreadComments not set")
	}
}

// TestApplyPRReviewCommentCreatedDoesNotSetPRStateChanged mirrors
// TestApplyPRReviewSubmittedDoesNotSetPRStateChanged for inline review
// comments — the exact event that fed the false-trip-defeating bug this
// guards against (caught during PR review of #1089).
func TestApplyPRReviewCommentCreatedDoesNotSetPRStateChanged(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "rc2", Body: "nit"}
	_, changes, err := s.Apply(PRReviewCommentCreated{Repo: testRepo, PRNumber: 1, Comment: c})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected a Change")
	}
	if changes[0].Fields&PRStateChanged != 0 {
		t.Errorf("Fields %b unexpectedly includes PRStateChanged", changes[0].Fields)
	}
}

// ---- LocalStatusUpdated ----

func TestApplyLocalStatusUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"}, StatusChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Status != "Review" {
		t.Errorf("Status = %q; want Review", st.Status)
	}
}

// ---- LocalLabelAdded / LocalLabelRemoved ----

func TestApplyLocalLabelAdded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:yolo"}, LabelsChanged)
	if !containsString(getItem(t, s, testRepo, 1).Labels, "fabrik:yolo") {
		t.Error("label not added")
	}
}

func TestApplyLocalLabelRemoved(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:yolo"})
	applyExpect(t, s, LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "fabrik:yolo"}, LabelsChanged)
	if containsString(getItem(t, s, testRepo, 1).Labels, "fabrik:yolo") {
		t.Error("label not removed")
	}
}

// ---- LocalCommentAdded ----

func TestApplyLocalCommentAdded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "local1", Body: "Fabrik posted"}
	applyExpect(t, s, LocalCommentAdded{Repo: testRepo, Number: 1, Comment: c}, CommentsChanged)
	st := getItem(t, s, testRepo, 1)
	found := false
	for _, got := range st.Comments {
		if got.ID == "local1" {
			found = true
		}
	}
	if !found {
		t.Error("local comment not appended")
	}
}

func TestApplyLocalCommentAdded_DedupsByDatabaseID(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	c := gh.Comment{ID: "local1", DatabaseID: 42, Body: "Fabrik posted"}
	applyExpect(t, s, LocalCommentAdded{Repo: testRepo, Number: 1, Comment: c}, CommentsChanged)
	// Re-applying the same local add (e.g. a retried mutation after a crash)
	// must not duplicate the comment.
	snap, changes, err := s.Apply(LocalCommentAdded{Repo: testRepo, Number: 1, Comment: c})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("re-applying an existing local comment produced changes %v; want none", changes)
	}
	st := snap.State()
	count := 0
	for _, got := range st.Comments {
		if got.DatabaseID == 42 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("Comments has %d entries with DatabaseID 42; want 1", count)
	}
}

// ---- LocalLockAcquired / LocalLockReleased ----

func TestApplyLocalLockAcquired(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	acquiredAt := time.Now()
	w := &WorkerHandle{PID: 999, StageName: "Implement", StartedAt: acquiredAt}
	applyExpect(t, s, LocalLockAcquired{Repo: testRepo, Number: 1, User: "alice", Worker: w, AcquiredAt: acquiredAt}, LockChanged|WorkerChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Lock == nil || st.Lock.HolderUser != "alice" || !st.Lock.HeldByThis {
		t.Error("Lock not set correctly")
	}
	if !st.Lock.AcquiredAt.Equal(acquiredAt) {
		t.Error("Lock.AcquiredAt not set from mutation")
	}
	if st.Worker == nil || st.Worker.PID != 999 {
		t.Error("Worker not set correctly")
	}
}

func TestApplyLocalLockReleased(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "alice", Worker: nil, AcquiredAt: time.Now()})
	applyExpect(t, s, LocalLockReleased{Repo: testRepo, Number: 1}, LockChanged)
	if getItem(t, s, testRepo, 1).Lock != nil {
		t.Error("Lock not cleared")
	}
}

// ---- ItemDeepFetched ----

func TestApplyItemDeepFetched(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	fresh := testProjectItem(testRepo, 1)
	fresh.Title = "Updated Title"
	applyExpect(t, s, ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: fresh}, DeepFetchChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Title != "Updated Title" {
		t.Errorf("Title = %q; want Updated Title", st.Title)
	}
	if st.LastDeepFetchAt.IsZero() {
		t.Error("LastDeepFetchAt not set")
	}
}

// ---- SelfWriteObserved ----

func TestApplySelfWriteObserved(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := time.Now()
	applyExpect(t, s, SelfWriteObserved{Repo: testRepo, Number: 1}, SelfWriteBaselineChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LastSeenSourceUpdatedAt.Before(before) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want >= %v", st.LastSeenSourceUpdatedAt, before)
	}
}

// TestApplySelfWriteObservedMonotonic verifies the "never move the baseline
// backward" requirement (#1090): a SelfWriteObserved applied after a deep
// fetch that already recorded a later source updatedAt (e.g. clock skew, or a
// deep fetch racing ahead of the self-write's cache write-through) must not
// regress LastSeenSourceUpdatedAt to the earlier local wall-clock value.
func TestApplySelfWriteObservedMonotonic(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	future := time.Now().Add(time.Hour)
	fresh := testProjectItem(testRepo, 1)
	fresh.UpdatedAt = future
	if _, _, err := s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: fresh}); err != nil {
		t.Fatalf("seed ItemDeepFetched: %v", err)
	}

	snap, changes, err := s.Apply(SelfWriteObserved{Repo: testRepo, Number: 1})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 0 && changes[0].Fields&SelfWriteBaselineChanged != 0 {
		t.Error("SelfWriteObserved advanced the baseline backward past a later recorded value")
	}
	if st := snap.State(); !st.LastSeenSourceUpdatedAt.Equal(future) {
		t.Errorf("LastSeenSourceUpdatedAt = %v; want unchanged %v", st.LastSeenSourceUpdatedAt, future)
	}
}

// TestApplySelfWriteObservedTouchesOnlyBaseline verifies the requirement that
// SelfWriteObserved touch no field other than LastSeenSourceUpdatedAt (#1090).
func TestApplySelfWriteObservedTouchesOnlyBaseline(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	before := getItem(t, s, testRepo, 1)
	if _, _, err := s.Apply(SelfWriteObserved{Repo: testRepo, Number: 1}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after := getItem(t, s, testRepo, 1)
	before.LastSeenSourceUpdatedAt = after.LastSeenSourceUpdatedAt
	if !reflect.DeepEqual(before, after) {
		t.Errorf("SelfWriteObserved touched a field other than LastSeenSourceUpdatedAt:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// ---- StageAttempted ----

func TestApplyStageAttempted(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	applyExpect(t, s, StageAttempted{Repo: testRepo, Number: 1, StageName: "Implement", At: at}, StageStateChanged)
	st := getItem(t, s, testRepo, 1)
	// StageAttempted only sets LastAttemptAt; it does NOT increment Attempts.
	// Attempts is incremented exclusively by StageRetryIncremented (on stage failure).
	if st.StageState.Attempts["Implement"] != 0 {
		t.Errorf("Attempts[Implement] = %d; want 0 (StageAttempted must not touch Attempts)", st.StageState.Attempts["Implement"])
	}
	if !st.StageState.LastAttemptAt["Implement"].Equal(at) {
		t.Error("LastAttemptAt not set")
	}
}

// ---- StageRetryIncremented ----

func TestApplyStageRetryIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	// StageAttempted does not increment Attempts; StageRetryIncremented does.
	s.Apply(StageAttempted{Repo: testRepo, Number: 1, StageName: "Review", At: time.Now()})
	applyExpect(t, s, StageRetryIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.Attempts["Review"] != 1 {
		t.Error("Attempts not incremented to 1")
	}
}

// ---- StageRetryCleared ----

func TestApplyStageRetryCleared(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(StageRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"})
	s.Apply(SliceRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"})
	s.Apply(ToolsDeniedRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"})
	applyExpect(t, s, StageRetryCleared{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.Attempts["Implement"] != 0 {
		t.Error("Attempts not cleared")
	}
	if getItem(t, s, testRepo, 1).StageState.SliceRetries["Implement"] != 0 {
		t.Error("SliceRetries not cleared alongside Attempts")
	}
	if getItem(t, s, testRepo, 1).StageState.ToolsDeniedRetries["Implement"] != 0 {
		t.Error("ToolsDeniedRetries not cleared alongside Attempts")
	}
}

// ---- SliceRetryIncremented ----

func TestApplySliceRetryIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, SliceRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.SliceRetries["Implement"] != 1 {
		t.Error("SliceRetries not incremented to 1")
	}
	// SliceRetries is independent of Attempts — a turn-cap preemption must not
	// also count against the failure counter (#1199).
	if getItem(t, s, testRepo, 1).StageState.Attempts["Implement"] != 0 {
		t.Error("Attempts must not be touched by SliceRetryIncremented")
	}
}

// ---- ToolsDeniedRetryIncremented ----

func TestApplyToolsDeniedRetryIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, ToolsDeniedRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.ToolsDeniedRetries["Implement"] != 1 {
		t.Error("ToolsDeniedRetries not incremented to 1")
	}
	// ToolsDeniedRetries is independent of Attempts — a permission-denial exit
	// must not also count against the failure counter (#1523). This is the
	// assertion that must fail if the max_retries exemption is removed
	// (mirrors AC8's non-vacuousness requirement at the itemstate layer).
	if getItem(t, s, testRepo, 1).StageState.Attempts["Implement"] != 0 {
		t.Error("Attempts must not be touched by ToolsDeniedRetryIncremented")
	}
}

// ---- ReviewCycleIncremented ----

func TestApplyReviewCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, ReviewCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.ReviewCycles["Review"] != 1 {
		t.Error("ReviewCycles not incremented")
	}
}

// ---- ReviewCycleDecremented (#1045) ----

func TestApplyReviewCycleDecremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, ReviewCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	applyExpect(t, s, ReviewCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	applyExpect(t, s, ReviewCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if got := getItem(t, s, testRepo, 1).StageState.ReviewCycles["Review"]; got != 1 {
		t.Errorf("ReviewCycles = %d, want 1", got)
	}
}

func TestApplyReviewCycleDecremented_FlooredAtZero(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	// Decrementing an already-zero counter is a genuine no-op field-wise
	// (the value stays 0), so the store must report no Change (invariant
	// I6 — no Observers notified) — assert the returned []Change is empty,
	// not just discard it.
	_, changes, err := s.Apply(ReviewCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("Apply(ReviewCycleDecremented) on zero counter: expected no Change (I6), got %v", changes)
	}
	if got := getItem(t, s, testRepo, 1).StageState.ReviewCycles["Review"]; got != 0 {
		t.Errorf("ReviewCycles = %d, want 0 (floored)", got)
	}
	applyExpect(t, s, ReviewCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	applyExpect(t, s, ReviewCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	_, changes, err = s.Apply(ReviewCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("Apply(ReviewCycleDecremented) double decrement at zero: expected no Change (I6), got %v", changes)
	}
	if got := getItem(t, s, testRepo, 1).StageState.ReviewCycles["Review"]; got != 0 {
		t.Errorf("ReviewCycles = %d, want 0 (floored, double decrement)", got)
	}
}

// ---- ReviewBlockedCycleIncremented (ADR-1518) ----

func TestApplyReviewBlockedCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, ReviewBlockedCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if got := getItem(t, s, testRepo, 1).StageState.ReviewBlockedCycles["Review"]; got != 1 {
		t.Errorf("ReviewBlockedCycles = %d, want 1", got)
	}
	applyExpect(t, s, ReviewBlockedCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if got := getItem(t, s, testRepo, 1).StageState.ReviewBlockedCycles["Review"]; got != 2 {
		t.Errorf("ReviewBlockedCycles = %d, want 2", got)
	}
	// Independent of ReviewCycles — the two counters track different things
	// (attempts vs. attempts-that-happened-while-genuinely-blocked) and
	// ReviewBlockedCycleIncremented must never touch the other map.
	if got := getItem(t, s, testRepo, 1).StageState.ReviewCycles["Review"]; got != 0 {
		t.Errorf("ReviewCycles = %d, want 0 (must not be touched by ReviewBlockedCycleIncremented)", got)
	}
}

// TestReviewBlockedCyclesNeverRefunded documents, at the store level, the
// defining property of ReviewBlockedCycles (ADR-1518, R3): unlike
// ReviewCycles, it has no decrement mutation at all — nothing in this package
// can lower it once incremented, short of EngineCyclesCleared's full reset.
// This is what lets it survive #1045's ReviewCycleDecremented refund
// indefinitely forgiving ReviewCycles for the same sequence of no-op
// reinvokes.
func TestReviewBlockedCyclesNeverRefunded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	for i := 0; i < 5; i++ {
		s.Apply(ReviewBlockedCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"})
		// Every increment is paired with the exact refund #1045 applies to
		// ReviewCycles for a no-op reinvoke — ReviewBlockedCycles must be
		// unaffected by it.
		s.Apply(ReviewCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"})
		s.Apply(ReviewCycleDecremented{Repo: testRepo, Number: 1, StageName: "Review"})
	}
	st := getItem(t, s, testRepo, 1)
	if got := st.StageState.ReviewBlockedCycles["Review"]; got != 5 {
		t.Errorf("ReviewBlockedCycles = %d, want 5 (never refunded)", got)
	}
	if got := st.StageState.ReviewCycles["Review"]; got != 0 {
		t.Errorf("ReviewCycles = %d, want 0 (refunded every time, #1045)", got)
	}
}

// ---- CIFixCycleIncremented ----

func TestApplyCIFixCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, CIFixCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.CIFixCycles["Validate"] != 1 {
		t.Error("CIFixCycles not incremented")
	}
}

// ---- RebaseCycleIncremented ----

func TestApplyRebaseCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, RebaseCycleIncremented{Repo: testRepo, Number: 1, StageName: "Review"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.RebaseCycles["Review"] != 1 {
		t.Error("RebaseCycles not incremented")
	}
}

// ---- EnqueueCycleIncremented (ADR-058 D4 FR-3) ----

func TestApplyEnqueueCycleIncremented(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, EnqueueCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	if getItem(t, s, testRepo, 1).StageState.EnqueueCycles["Validate"] != 1 {
		t.Error("EnqueueCycles not incremented")
	}
	// Second increment accumulates independently per stage.
	applyExpect(t, s, EnqueueCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	if got := getItem(t, s, testRepo, 1).StageState.EnqueueCycles["Validate"]; got != 2 {
		t.Errorf("EnqueueCycles[Validate] = %d, want 2", got)
	}
	if got := getItem(t, s, testRepo, 1).StageState.EnqueueCycles["Review"]; got != 0 {
		t.Errorf("EnqueueCycles[Review] = %d, want 0 (per-stage isolation)", got)
	}
}

// ---- StageTurnUsageRecorded (#1146 stall detection) ----

func TestApplyStageTurnUsageRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, StageTurnUsageRecorded{Repo: testRepo, Number: 1, StageName: "Implement", TurnsUsed: 51, Capped: true}, StageStateChanged)
	st := getItem(t, s, testRepo, 1)
	if st.StageState.LastTurnsUsed["Implement"] != 51 {
		t.Errorf("LastTurnsUsed[Implement] = %d, want 51", st.StageState.LastTurnsUsed["Implement"])
	}
	if !st.StageState.LastTurnsCapped["Implement"] {
		t.Error("LastTurnsCapped[Implement] not set")
	}
	snap, _ := s.Get(testRepo, 1)
	if got := snap.LastTurnsUsed("Implement"); got != 51 {
		t.Errorf("Snapshot.LastTurnsUsed() = %d, want 51", got)
	}
	if !snap.LastTurnsCapped("Implement") {
		t.Error("Snapshot.LastTurnsCapped() = false, want true")
	}
	// A later, uncapped, declining invocation overwrites both fields — this is what
	// makes stall detection self-limiting to a single corrective hint per episode.
	applyExpect(t, s, StageTurnUsageRecorded{Repo: testRepo, Number: 1, StageName: "Implement", TurnsUsed: 12, Capped: false}, StageStateChanged)
	st = getItem(t, s, testRepo, 1)
	if st.StageState.LastTurnsUsed["Implement"] != 12 {
		t.Errorf("LastTurnsUsed[Implement] = %d, want 12 after overwrite", st.StageState.LastTurnsUsed["Implement"])
	}
	if st.StageState.LastTurnsCapped["Implement"] {
		t.Error("LastTurnsCapped[Implement] should be false after an uncapped attempt")
	}
}

func TestSnapshot_LastTurnsUsed_ZeroWhenUnset(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	snap, _ := s.Get(testRepo, 1)
	if got := snap.LastTurnsUsed("Implement"); got != 0 {
		t.Errorf("LastTurnsUsed() = %d, want 0 when never recorded", got)
	}
	if snap.LastTurnsCapped("Implement") {
		t.Error("LastTurnsCapped() = true, want false when never recorded")
	}
}

func TestStageTurnUsageSnapshotIsDeepCopy(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	held := applyExpect(t, s, StageTurnUsageRecorded{Repo: testRepo, Number: 1, StageName: "Implement", TurnsUsed: 51, Capped: true}, StageStateChanged)
	if held.LastTurnsUsed("Implement") != 51 {
		t.Fatalf("precondition: held snapshot LastTurnsUsed = %d, want 51", held.LastTurnsUsed("Implement"))
	}
	s.Apply(StageTurnUsageRecorded{Repo: testRepo, Number: 1, StageName: "Implement", TurnsUsed: 12, Capped: false})
	if held.LastTurnsUsed("Implement") != 51 {
		t.Errorf("held snapshot mutated by later record: LastTurnsUsed = %d, want 51", held.LastTurnsUsed("Implement"))
	}
	if !held.LastTurnsCapped("Implement") {
		t.Error("held snapshot mutated by later record: LastTurnsCapped = false, want true")
	}
}

// ---- StallHintArmed / StallHintConsumed (#1146 stall detection) ----

func TestApplyStallHintArmedAndConsumed(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, StallHintArmed{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	snap, _ := s.Get(testRepo, 1)
	if !snap.StallHintPending("Implement") {
		t.Error("StallHintPending(Implement) = false, want true after arming")
	}
	// Arming one stage must not leak to another.
	if snap.StallHintPending("Review") {
		t.Error("StallHintPending(Review) = true, want false (per-stage isolation)")
	}
	applyExpect(t, s, StallHintConsumed{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	snap, _ = s.Get(testRepo, 1)
	if snap.StallHintPending("Implement") {
		t.Error("StallHintPending(Implement) = true, want false after consuming")
	}
}

func TestApplyStageRetryClearedClearsStallState(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(StageTurnUsageRecorded{Repo: testRepo, Number: 1, StageName: "Implement", TurnsUsed: 51, Capped: true})
	s.Apply(StallHintArmed{Repo: testRepo, Number: 1, StageName: "Implement"})
	applyExpect(t, s, StageRetryCleared{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	snap, _ := s.Get(testRepo, 1)
	if snap.LastTurnsUsed("Implement") != 0 {
		t.Error("LastTurnsUsed not cleared by StageRetryCleared")
	}
	if snap.LastTurnsCapped("Implement") {
		t.Error("LastTurnsCapped not cleared by StageRetryCleared")
	}
	if snap.StallHintPending("Implement") {
		t.Error("StallHintPending not cleared by StageRetryCleared")
	}
}

// ---- PREnqueueRecorded (ADR-058 D4 FR-3) ----

func TestApplyPREnqueueRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, PREnqueueRecorded{Repo: testRepo, Number: 1, SHA: "abc123"}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || st.LinkedPR.LastEnqueuedSHA != "abc123" {
		t.Errorf("LastEnqueuedSHA not recorded; got %+v", st.LinkedPR)
	}
	// A later enqueue at a new SHA overwrites the record.
	applyExpect(t, s, PREnqueueRecorded{Repo: testRepo, Number: 1, SHA: "def456"}, LinkedPRChanged)
	if got := getItem(t, s, testRepo, 1).LinkedPR.LastEnqueuedSHA; got != "def456" {
		t.Errorf("LastEnqueuedSHA = %q, want def456 after re-enqueue", got)
	}
}

// ---- CIFixNoOpRecorded (#958 leg 2) ----

func TestApplyCIFixNoOpRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, CIFixNoOpRecorded{Repo: testRepo, Number: 1, SHA: "sha_noop_1"}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || st.LinkedPR.LastCIFixNoOpSHA != "sha_noop_1" {
		t.Errorf("LastCIFixNoOpSHA not recorded; got %+v", st.LinkedPR)
	}
	snap, _ := s.Get(testRepo, 1)
	if got := snap.LastCIFixNoOpSHA(); got != "sha_noop_1" {
		t.Errorf("Snapshot.LastCIFixNoOpSHA() = %q, want sha_noop_1", got)
	}
	// A later no-op at a new SHA overwrites the record.
	applyExpect(t, s, CIFixNoOpRecorded{Repo: testRepo, Number: 1, SHA: "sha_noop_2"}, LinkedPRChanged)
	if got := getItem(t, s, testRepo, 1).LinkedPR.LastCIFixNoOpSHA; got != "sha_noop_2" {
		t.Errorf("LastCIFixNoOpSHA = %q, want sha_noop_2 after second no-op", got)
	}
}

func TestSnapshot_LastCIFixNoOpSHA_EmptyWhenUnset(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	snap, _ := s.Get(testRepo, 1)
	if got := snap.LastCIFixNoOpSHA(); got != "" {
		t.Errorf("LastCIFixNoOpSHA() = %q, want empty when never recorded", got)
	}
}

// TestEnqueueCyclesSnapshotIsDeepCopy verifies that a Snapshot taken before a
// later EnqueueCycleIncremented retains its original count — confirming
// copyStageState deep-copies the EnqueueCycles map (no shared backing map).
func TestEnqueueCyclesSnapshotIsDeepCopy(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	held := applyExpect(t, s, EnqueueCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	if held.EnqueueCycles("Validate") != 1 {
		t.Fatalf("precondition: held snapshot EnqueueCycles = %d, want 1", held.EnqueueCycles("Validate"))
	}
	// Mutate the store further.
	s.Apply(EnqueueCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"})
	if held.EnqueueCycles("Validate") != 1 {
		t.Errorf("held snapshot mutated by later increment: EnqueueCycles = %d, want 1", held.EnqueueCycles("Validate"))
	}
}

// TestEngineCyclesClearedClearsEnqueueCycles verifies EngineCyclesCleared zeroes
// EnqueueCycles alongside the review/CI/rebase counters (FR-3: a successful
// settle/unpause must not leave a stale re-enqueue count that prematurely pauses).
func TestEngineCyclesClearedClearsEnqueueCycles(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(EnqueueCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"})
	s.Apply(RebaseCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"})
	s.Apply(ReviewBlockedCycleIncremented{Repo: testRepo, Number: 1, StageName: "Validate"})
	if getItem(t, s, testRepo, 1).StageState.EnqueueCycles["Validate"] != 1 {
		t.Fatal("precondition: EnqueueCycles not set")
	}
	applyExpect(t, s, EngineCyclesCleared{Repo: testRepo, Number: 1, StageName: "Validate"}, StageStateChanged)
	st := getItem(t, s, testRepo, 1)
	if got := st.StageState.EnqueueCycles["Validate"]; got != 0 {
		t.Errorf("EnqueueCycles not cleared: got %d, want 0", got)
	}
	if got := st.StageState.RebaseCycles["Validate"]; got != 0 {
		t.Errorf("RebaseCycles not cleared by EngineCyclesCleared: got %d, want 0", got)
	}
	// ADR-1518: a resumed item must not immediately re-trip the new
	// non-convergence terminal check on its very next pass — the same
	// "unpause -> re-pause in one poll" bug #1460 already fixed for the other
	// counters.
	if got := st.StageState.ReviewBlockedCycles["Validate"]; got != 0 {
		t.Errorf("ReviewBlockedCycles not cleared by EngineCyclesCleared: got %d, want 0", got)
	}
}

// TestSliceRetriesSnapshotIsDeepCopy verifies that a Snapshot taken before a
// later SliceRetryIncremented retains its original count — confirming
// copyStageState deep-copies the SliceRetries map (no shared backing map).
func TestSliceRetriesSnapshotIsDeepCopy(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	held := applyExpect(t, s, SliceRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if held.SliceRetries("Implement") != 1 {
		t.Fatalf("precondition: held snapshot SliceRetries = %d, want 1", held.SliceRetries("Implement"))
	}
	s.Apply(SliceRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"})
	if held.SliceRetries("Implement") != 1 {
		t.Errorf("held snapshot mutated by later increment: SliceRetries = %d, want 1", held.SliceRetries("Implement"))
	}
}

// TestToolsDeniedRetriesSnapshotIsDeepCopy verifies that a Snapshot taken
// before a later ToolsDeniedRetryIncremented retains its original count —
// confirming copyStageState deep-copies the ToolsDeniedRetries map (no shared
// backing map). Mirrors TestSliceRetriesSnapshotIsDeepCopy.
func TestToolsDeniedRetriesSnapshotIsDeepCopy(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	held := applyExpect(t, s, ToolsDeniedRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if held.ToolsDeniedRetries("Implement") != 1 {
		t.Fatalf("precondition: held snapshot ToolsDeniedRetries = %d, want 1", held.ToolsDeniedRetries("Implement"))
	}
	s.Apply(ToolsDeniedRetryIncremented{Repo: testRepo, Number: 1, StageName: "Implement"})
	if held.ToolsDeniedRetries("Implement") != 1 {
		t.Errorf("held snapshot mutated by later increment: ToolsDeniedRetries = %d, want 1", held.ToolsDeniedRetries("Implement"))
	}
}

// ---- EnginePaused ----

func TestApplyEnginePaused(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, EnginePaused{Repo: testRepo, Number: 1, StageName: "Implement"}, StageStateChanged)
	if !getItem(t, s, testRepo, 1).StageState.PausedByEngine["Implement"] {
		t.Error("PausedByEngine not set")
	}
}

// ---- CooldownRecorded ----

func TestApplyCooldownRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	until := time.Now().Add(10 * time.Minute)
	applyExpect(t, s, CooldownRecorded{Repo: testRepo, Number: 1, Reason: "retry", Until: until}, CooldownChanged)
	got := getItem(t, s, testRepo, 1).CooldownAt["retry"]
	if !got.Equal(until) {
		t.Errorf("CooldownAt[retry] = %v; want %v", got, until)
	}
}

// ---- LabelAppliedAtRecorded ----

func TestApplyLabelAppliedAtRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	snap := applyExpect(t, s, LabelAppliedAtRecorded{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci", At: at}, LabelAppliedAtChanged)
	got := snap.LabelAppliedAt("fabrik:awaiting-ci")
	if !got.Equal(at) {
		t.Errorf("LabelAppliedAt(fabrik:awaiting-ci) = %v; want %v", got, at)
	}
}

// TestLabelAppliedAtRecorded_AppliedRemovedReapplied proves the record-at-write
// cache survives the "most recent application" correctness requirement (#1314):
// a label applied, (conceptually) removed, and re-applied must read back the
// *latest* timestamp, not the first. LabelAppliedAtRecorded always overwrites
// (see its doc comment) — this test pins that overwrite behavior directly
// against the mutation, independent of any engine-level guard.
func TestLabelAppliedAtRecorded_AppliedRemovedReapplied(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	first := time.Now().Add(-time.Hour)
	second := time.Now()

	s.Apply(LabelAppliedAtRecorded{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci", At: first})
	// Removal doesn't touch the cache (no mutation for it) — the entry from the
	// first application is still readable until the next genuine re-apply.
	if got := getItem(t, s, testRepo, 1).LabelAppliedAt["fabrik:awaiting-ci"]; !got.Equal(first) {
		t.Fatalf("after first apply: LabelAppliedAt = %v; want %v", got, first)
	}
	s.Apply(LabelAppliedAtRecorded{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci", At: second})

	got := getItem(t, s, testRepo, 1).LabelAppliedAt["fabrik:awaiting-ci"]
	if !got.Equal(second) {
		t.Errorf("after re-apply: LabelAppliedAt = %v; want the latest (second) application %v, not the first %v", got, second, first)
	}
}

// TestLabelAppliedAtRecorded_DoesNotAliasCooldown pins the reason LabelAppliedAt
// is a distinct map from CooldownAt (see ItemState.LabelAppliedAt's doc
// comment): a raw applied-at timestamp is always in the past the instant it's
// recorded, so if it were folded into CooldownAt, HasExpiredCooldown would
// misread it as a permanently expired cooldown and spuriously wake dispatch
// for the item on every check thereafter.
func TestLabelAppliedAtRecorded_DoesNotAliasCooldown(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	past := time.Now().Add(-time.Hour)
	snap := applyExpect(t, s, LabelAppliedAtRecorded{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci", At: past}, LabelAppliedAtChanged)

	if snap.HasExpiredCooldown(time.Now()) {
		t.Error("HasExpiredCooldown should be false: LabelAppliedAtRecorded must never populate CooldownAt")
	}
	if snap.HasActiveCooldown(time.Now()) {
		t.Error("HasActiveCooldown should be false: LabelAppliedAtRecorded must never populate CooldownAt")
	}
	if len(getItem(t, s, testRepo, 1).CooldownAt) != 0 {
		t.Error("CooldownAt should remain empty; LabelAppliedAtRecorded must write only to LabelAppliedAt")
	}
}

// ---- WorkerEntered / WorkerHeartbeat / WorkerExited ----

func TestApplyWorkerEntered(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	applyExpect(t, s, WorkerEntered{Repo: testRepo, Number: 1, StageName: "Plan", StartedAt: now}, WorkerChanged|WorkerLifecycleChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Worker == nil {
		t.Fatal("Worker should be non-nil after WorkerEntered")
	}
	if st.Worker.StageName != "Plan" {
		t.Errorf("Worker.StageName = %q; want %q", st.Worker.StageName, "Plan")
	}
	if !st.Worker.StartedAt.Equal(now) {
		t.Errorf("Worker.StartedAt = %v; want %v", st.Worker.StartedAt, now)
	}
	if !st.Worker.LastSignAt.Equal(now) {
		t.Errorf("Worker.LastSignAt = %v; want %v", st.Worker.LastSignAt, now)
	}
}

func TestWorkerEnteredOverwrittenByLocalLockAcquired(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	s.Apply(WorkerEntered{Repo: testRepo, Number: 1, StageName: "Plan", StartedAt: now})
	later := now.Add(time.Second)
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "bot", AcquiredAt: later,
		Worker: &WorkerHandle{StageName: "Plan", StartedAt: now, PID: 42}})
	st := getItem(t, s, testRepo, 1)
	if st.Worker == nil || st.Worker.PID != 42 {
		t.Errorf("LocalLockAcquired should overwrite WorkerEntered placeholder; Worker.PID = %v", func() int {
			if st.Worker != nil {
				return st.Worker.PID
			}
			return -1
		}())
	}
}

func TestWorkerEnteredThenExited(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	s.Apply(WorkerEntered{Repo: testRepo, Number: 1, StageName: "Research", StartedAt: now})
	applyExpect(t, s, WorkerExited{Repo: testRepo, Number: 1}, WorkerChanged|WorkerLifecycleChanged)
	if getItem(t, s, testRepo, 1).Worker != nil {
		t.Error("Worker should be nil after WorkerExited")
	}
}

func TestApplyWorkerHeartbeat(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
	at := time.Now()
	applyExpect(t, s, WorkerHeartbeat{Repo: testRepo, Number: 1, At: at}, WorkerChanged)
	st := getItem(t, s, testRepo, 1)
	if st.Worker == nil || !st.Worker.LastSignAt.Equal(at) {
		t.Error("Worker.LastSignAt not set")
	}
}

func TestApplyWorkerExited(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	now := time.Now()
	s.Apply(LocalLockAcquired{Repo: testRepo, Number: 1, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
	applyExpect(t, s, WorkerExited{Repo: testRepo, Number: 1}, WorkerChanged|WorkerLifecycleChanged)
	if getItem(t, s, testRepo, 1).Worker != nil {
		t.Error("Worker not cleared")
	}
}

// ---- InvocationRecorded ----

func TestApplyInvocationRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	usage := TokenUsage{InputTokens: 100, OutputTokens: 200, TurnsUsed: 5, MaxTurns: 50}
	applyExpect(t, s, InvocationRecorded{
		Repo:      testRepo,
		Number:    1,
		Completed: true,
		Blocked:   false,
		Usage:     usage,
	}, InvocationChanged)
	st := getItem(t, s, testRepo, 1)
	if !st.LastInvocationCompleted {
		t.Error("LastInvocationCompleted not set")
	}
	if st.LastTokenUsage.InputTokens != 100 {
		t.Errorf("LastTokenUsage.InputTokens = %d; want 100", st.LastTokenUsage.InputTokens)
	}
}

func TestApplyInvocationRecordedIsComment(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, InvocationRecorded{
		Repo:      testRepo,
		Number:    1,
		IsComment: true,
	}, InvocationChanged)
	st := getItem(t, s, testRepo, 1)
	if !st.LastInvocationIsComment {
		t.Error("LastInvocationIsComment not set when IsComment=true")
	}

	// A subsequent stage-run invocation (IsComment=false) clears the flag.
	applyExpect(t, s, InvocationRecorded{
		Repo:      testRepo,
		Number:    1,
		IsComment: false,
	}, InvocationChanged)
	st = getItem(t, s, testRepo, 1)
	if st.LastInvocationIsComment {
		t.Error("LastInvocationIsComment should be false after IsComment=false invocation")
	}
}

func TestApplyInvocationRecordedDuration(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	d := 42 * time.Second
	applyExpect(t, s, InvocationRecorded{
		Repo:     testRepo,
		Number:   1,
		Duration: d,
	}, InvocationChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LastInvocationDuration != d {
		t.Errorf("LastInvocationDuration = %v; want %v", st.LastInvocationDuration, d)
	}
}

// ---- DeepFetchFailed ----

func TestApplyDeepFetchFailed(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	applyExpect(t, s, DeepFetchFailed{Repo: testRepo, Number: 1, At: at}, DeepFetchChanged)
	if !getItem(t, s, testRepo, 1).LastDeepFetchFailureAt.Equal(at) {
		t.Error("LastDeepFetchFailureAt not set")
	}
}

// ---- BaseBranchWarnRecorded ----

func TestApplyBaseBranchWarnRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, BaseBranchWarnRecorded{Repo: testRepo, Number: 1, Branch: "feat/x"}, BaseBranchChanged)
	if !getItem(t, s, testRepo, 1).BaseBranchWarned["feat/x"] {
		t.Error("BaseBranchWarned not set")
	}
}

// ---- BoardReconciled ----

func TestApplyBoardReconciled(t *testing.T) {
	s := NewStore(nil)
	items := []gh.ProjectItem{
		testProjectItem(testRepo, 10),
		testProjectItem(testRepo, 11),
	}
	_, changes, err := s.Apply(BoardReconciled{Items: items})
	if err != nil {
		t.Fatalf("Apply(BoardReconciled): %v", err)
	}
	if len(changes) != 2 {
		t.Errorf("changes = %d; want 2", len(changes))
	}
	// Items should now be in the store.
	for _, pi := range items {
		if _, err := s.Get(pi.Repo, pi.Number); err != nil {
			t.Errorf("Get(%d) after BoardReconciled: %v", pi.Number, err)
		}
	}
}

// ---- ProjectV2ItemEdited (itemID-based routing) ----

func TestApplyProjectV2ItemEdited(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_xyz"
	s.Apply(IssueOpened{Item: pi})

	applyExpect(t, s, ProjectV2ItemEdited{ItemID: "PVTI_xyz", NewStatus: "Review"}, StatusChanged)
	if getItem(t, s, testRepo, 1).Status != "Review" {
		t.Error("Status not updated via ProjectV2ItemEdited")
	}
}

// ---- CheckRunCompleted (SHA-based routing) ----

func TestApplyCheckRunCompleted(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.LinkedPRNumber = 10
	s.Apply(IssueOpened{Item: pi})
	// Directly set HeadSHA via a mutation that exercises the field.
	s.mu.Lock()
	key := itemKeyFor(testRepo, 1)
	item := s.items[key]
	if item.LinkedPR == nil {
		item.LinkedPR = &LinkedPRState{Number: 10}
	}
	item.LinkedPR.HeadSHA = "abc123"
	s.shaToKey["abc123"] = key
	s.mu.Unlock()

	run := gh.CheckRun{ID: 1, Name: "CI", Status: "completed", Conclusion: "success"}
	applyExpect(t, s, CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: run}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || len(st.LinkedPR.CheckRuns) == 0 {
		t.Fatal("CheckRuns not set")
	}
	if !st.LinkedPR.HasHadChecks {
		t.Error("HasHadChecks not set")
	}
}

// ---- LastCIProgressAt (ADR-1410: CI-gate liveness detection) ----

// TestApplyCheckRunCompleted_SetsLastCIProgressAt verifies a new check-run ID
// is recorded as observable progress.
func TestApplyCheckRunCompleted_SetsLastCIProgressAt(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.LinkedPRNumber = 10
	s.Apply(IssueOpened{Item: pi})
	s.mu.Lock()
	key := itemKeyFor(testRepo, 1)
	item := s.items[key]
	item.LinkedPR = &LinkedPRState{Number: 10, HeadSHA: "abc123"}
	s.shaToKey["abc123"] = key
	s.mu.Unlock()

	if lpr := getItem(t, s, testRepo, 1).LinkedPR; lpr == nil || !lpr.LastCIProgressAt.IsZero() {
		t.Fatal("expected LastCIProgressAt to start zero")
	}

	run := gh.CheckRun{ID: 1, Name: "CI", Status: "in_progress"}
	s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: run})
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR.LastCIProgressAt.IsZero() {
		t.Error("expected LastCIProgressAt to be set after a new check-run ID appeared")
	}
}

// TestApplyCheckRunCompleted_StatusTransition_UpdatesLastCIProgressAt
// verifies an existing check run's Status/Conclusion transitioning is also
// recorded as progress, not just a brand-new ID appearing.
func TestApplyCheckRunCompleted_StatusTransition_UpdatesLastCIProgressAt(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.LinkedPRNumber = 10
	s.Apply(IssueOpened{Item: pi})
	s.mu.Lock()
	key := itemKeyFor(testRepo, 1)
	item := s.items[key]
	item.LinkedPR = &LinkedPRState{Number: 10, HeadSHA: "abc123"}
	s.shaToKey["abc123"] = key
	s.mu.Unlock()

	s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: gh.CheckRun{ID: 1, Name: "CI", Status: "queued"}})
	first := getItem(t, s, testRepo, 1).LinkedPR.LastCIProgressAt
	if first.IsZero() {
		t.Fatal("expected LastCIProgressAt set after the first observation")
	}

	time.Sleep(2 * time.Millisecond) // ensure a distinguishable timestamp
	s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: gh.CheckRun{ID: 1, Name: "CI", Status: "in_progress"}})
	second := getItem(t, s, testRepo, 1).LinkedPR.LastCIProgressAt
	if !second.After(first) {
		t.Error("expected LastCIProgressAt to advance after a Status transition on the same check-run ID")
	}
}

// TestApplyCheckRunCompleted_IdenticalDuplicate_DoesNotUpdateLastCIProgressAt
// is the negative case the liveness-stall dwell depends on: re-observing the
// exact same check-run content (e.g. a repeated poll while CI is genuinely
// stalled) must NOT look like progress.
func TestApplyCheckRunCompleted_IdenticalDuplicate_DoesNotUpdateLastCIProgressAt(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.LinkedPRNumber = 10
	s.Apply(IssueOpened{Item: pi})
	s.mu.Lock()
	key := itemKeyFor(testRepo, 1)
	item := s.items[key]
	item.LinkedPR = &LinkedPRState{Number: 10, HeadSHA: "abc123"}
	s.shaToKey["abc123"] = key
	s.mu.Unlock()

	run := gh.CheckRun{ID: 1, Name: "CI", Status: "in_progress"}
	s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: run})
	first := getItem(t, s, testRepo, 1).LinkedPR.LastCIProgressAt
	if first.IsZero() {
		t.Fatal("expected LastCIProgressAt set after the first observation")
	}

	time.Sleep(2 * time.Millisecond)
	s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "abc123", Run: run}) // identical duplicate
	second := getItem(t, s, testRepo, 1).LinkedPR.LastCIProgressAt
	if !second.Equal(first) {
		t.Errorf("expected LastCIProgressAt unchanged on an identical duplicate observation, got %v want %v", second, first)
	}
}

// TestApplyPRHeadSHAUpdated_SHAChange_SetsLastCIProgressAt verifies a head
// SHA advancing (a fresh push, which resets CI entirely) is itself counted as
// progress — this is what removes #342's dependence on when the wait started:
// a rebase-triggered head change needs no special handling because it's
// already progress.
func TestApplyPRHeadSHAUpdated_SHAChange_SetsLastCIProgressAt(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	s.Apply(IssueOpened{Item: pi})

	s.Apply(PRHeadSHAUpdated{Repo: testRepo, Number: 1, SHA: "sha-initial"})
	if lpr := getItem(t, s, testRepo, 1).LinkedPR; lpr == nil || !lpr.LastCIProgressAt.IsZero() {
		t.Fatal("expected LastCIProgressAt to remain zero on the first-ever link (no prior SHA to change from)")
	}

	s.Apply(PRHeadSHAUpdated{Repo: testRepo, Number: 1, SHA: "sha-new"})
	if lpr := getItem(t, s, testRepo, 1).LinkedPR; lpr == nil || lpr.LastCIProgressAt.IsZero() {
		t.Error("expected LastCIProgressAt to be set once the head SHA actually changes")
	}
}

// TestApplyPRHeadSHAUpdated_DrainedPendingRuns_SetsLastCIProgressAt verifies
// that check runs buffered before SHA linkage (the pendingCheckRuns path) are
// counted as progress once drained into the newly-linked item.
func TestApplyPRHeadSHAUpdated_DrainedPendingRuns_SetsLastCIProgressAt(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	s.Apply(IssueOpened{Item: pi})

	// Arrives before the SHA is linked to any item — buffered.
	s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "sha-prelink", Run: gh.CheckRun{ID: 1, Name: "CI", Status: "in_progress"}})

	s.Apply(PRHeadSHAUpdated{Repo: testRepo, Number: 1, SHA: "sha-prelink"})
	lpr := getItem(t, s, testRepo, 1).LinkedPR
	if lpr == nil || lpr.LastCIProgressAt.IsZero() {
		t.Error("expected LastCIProgressAt to be set when the drain actually adds buffered runs")
	}
	if len(lpr.CheckRuns) != 1 {
		t.Fatalf("expected 1 drained check run, got %d", len(lpr.CheckRuns))
	}
}

// ---- ChangeFlags coverage: every flag must be exercised ----

func TestChangeFlagsCoverage(t *testing.T) {
	// Maps every ChangeFlag to a mutation that should produce it.
	type flagCase struct {
		flag ChangeFlags
		name string
		mut  func(s *Store, number int) Mutation
	}
	cases := []flagCase{
		{StatusChanged, "StatusChanged", func(s *Store, n int) Mutation {
			return LocalStatusUpdated{Repo: testRepo, Number: n, NewStatus: "Review"}
		}},
		{LabelsChanged, "LabelsChanged", func(s *Store, n int) Mutation {
			return IssueLabeled{Repo: testRepo, Number: n, Label: "new-label"}
		}},
		{LockChanged, "LockChanged", func(s *Store, n int) Mutation {
			return LocalLockAcquired{Repo: testRepo, Number: n, User: "u"}
		}},
		{StageStateChanged, "StageStateChanged", func(s *Store, n int) Mutation {
			return StageAttempted{Repo: testRepo, Number: n, StageName: "S", At: time.Now()}
		}},
		{WorkerChanged, "WorkerChanged/Heartbeat", func(s *Store, n int) Mutation {
			now := time.Now()
			s.Apply(LocalLockAcquired{Repo: testRepo, Number: n, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
			return WorkerHeartbeat{Repo: testRepo, Number: n, At: now}
		}},
		{WorkerChanged, "WorkerChanged/PIDSet", func(s *Store, n int) Mutation {
			now := time.Now()
			s.Apply(LocalLockAcquired{Repo: testRepo, Number: n, User: "u", AcquiredAt: now, Worker: &WorkerHandle{StageName: "X", StartedAt: now}})
			return WorkerPIDSet{Repo: testRepo, Number: n, PID: 12345}
		}},
		{CooldownChanged, "CooldownChanged", func(s *Store, n int) Mutation {
			return CooldownRecorded{Repo: testRepo, Number: n, Reason: "r", Until: time.Now().Add(time.Minute)}
		}},
		{LinkedPRChanged, "LinkedPRChanged", func(s *Store, n int) Mutation {
			return PRReviewSubmitted{Repo: testRepo, Number: n, Review: gh.PRReview{Author: "r"}}
		}},
		{CommentsChanged, "CommentsChanged", func(s *Store, n int) Mutation {
			return IssueCommentCreated{Repo: testRepo, Number: n, Comment: gh.Comment{ID: "x"}}
		}},
		{AssigneesChanged, "AssigneesChanged", func(s *Store, n int) Mutation {
			pi := testProjectItem(testRepo, n)
			pi.Assignees = []string{"bob", "carol"}
			return IssueOpened{Item: pi}
		}},
		{TitleBodyChanged, "TitleBodyChanged", func(s *Store, n int) Mutation {
			pi := testProjectItem(testRepo, n)
			pi.Title = "Changed Title"
			return IssueOpened{Item: pi}
		}},
		{StateChanged, "StateChanged", func(s *Store, n int) Mutation {
			return IssueClosed{Repo: testRepo, Number: n}
		}},
		{BlockedByChanged, "BlockedByChanged", func(s *Store, n int) Mutation {
			pi := testProjectItem(testRepo, n)
			pi.BlockedBy = []gh.Dependency{{Number: 99}}
			return IssueOpened{Item: pi}
		}},
		{DeepFetchChanged, "DeepFetchChanged", func(s *Store, n int) Mutation {
			return DeepFetchFailed{Repo: testRepo, Number: n, At: time.Now()}
		}},
		{InvocationChanged, "InvocationChanged", func(s *Store, n int) Mutation {
			return InvocationRecorded{Repo: testRepo, Number: n, Completed: true}
		}},
		{BaseBranchChanged, "BaseBranchChanged", func(s *Store, n int) Mutation {
			return BaseBranchWarnRecorded{Repo: testRepo, Number: n, Branch: "b"}
		}},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(nil)
			number := i + 100 // unique number per sub-test
			s.Apply(IssueOpened{Item: testProjectItem(testRepo, number)})

			_, changes, err := s.Apply(c.mut(s, number))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(changes) == 0 {
				t.Fatalf("expected Change with flag %b, got no changes", c.flag)
			}
			if changes[0].Fields&c.flag == 0 {
				t.Errorf("Change.Fields %b does not include %b", changes[0].Fields, c.flag)
			}
		})
	}
}

// ---- I6: No-op mutation idempotency ----

// TestNoOpMutationProducesNoChange verifies that applying an identical mutation
// twice produces no observer event the second time (invariant I6).
func TestNoOpMutationProducesNoChange(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	// First application: changes status.
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})

	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))

	// Second application: same status — should be a no-op.
	_, changes, err := s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})
	if err != nil {
		t.Fatalf("Apply no-op: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("no-op Apply returned %d changes; want 0", len(changes))
	}
	if fired != 0 {
		t.Errorf("observer fired %d times for no-op mutation; want 0", fired)
	}
}

// TestNoOpLabelAddProducesNoChange verifies label idempotency.
func TestNoOpLabelAddProducesNoChange(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	// "bug" is already in the seed item labels.
	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))

	_, changes, _ := s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "bug"})
	if len(changes) != 0 || fired != 0 {
		t.Errorf("label no-op produced change (changes=%d, fired=%d)", len(changes), fired)
	}
}

// TestNoOpCooldownSameValue verifies CooldownRecorded is a no-op when value unchanged.
func TestNoOpCooldownSameValue(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	until := time.Unix(9999, 0)
	s.Apply(CooldownRecorded{Repo: testRepo, Number: 1, Reason: "retry", Until: until})

	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))
	_, changes, _ := s.Apply(CooldownRecorded{Repo: testRepo, Number: 1, Reason: "retry", Until: until})
	if len(changes) != 0 || fired != 0 {
		t.Errorf("cooldown no-op produced change (changes=%d, fired=%d)", len(changes), fired)
	}
}

// ---- CIMergePendingStarted / CIMergePendingCleared ----

func TestApplyCIMergePendingStarted(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Unix(12345, 0)
	applyExpect(t, s, CIMergePendingStarted{Repo: testRepo, Number: 1, At: at}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil {
		t.Fatal("LinkedPR is nil after CIMergePendingStarted")
	}
	if !st.LinkedPR.CIMergePendingSince.Equal(at) {
		t.Errorf("CIMergePendingSince = %v; want %v", st.LinkedPR.CIMergePendingSince, at)
	}
}

func TestApplyCIMergePendingCleared(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Unix(12345, 0)
	s.Apply(CIMergePendingStarted{Repo: testRepo, Number: 1, At: at})
	applyExpect(t, s, CIMergePendingCleared{Repo: testRepo, Number: 1}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR != nil && !st.LinkedPR.CIMergePendingSince.IsZero() {
		t.Errorf("CIMergePendingSince not cleared: %v", st.LinkedPR.CIMergePendingSince)
	}
}

func TestApplyCIMergePendingClearedWithNoLinkedPR(t *testing.T) {
	// CIMergePendingCleared on an item with no LinkedPR should be a no-op (not panic).
	s := newStoreWithItem(t, testRepo, 1)
	_, changes, err := s.Apply(CIMergePendingCleared{Repo: testRepo, Number: 1})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// No-op: LinkedPR is nil so clearing is trivially done; we allow no changes.
	_ = changes
}

// ---- PRChecksObserved ----

func TestApplyPRChecksObserved(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	applyExpect(t, s, PRChecksObserved{Repo: testRepo, Number: 1}, LinkedPRChanged)
	st := getItem(t, s, testRepo, 1)
	if st.LinkedPR == nil || !st.LinkedPR.HasHadChecks {
		t.Error("HasHadChecks not set after PRChecksObserved")
	}
}

func TestApplyPRChecksObservedIsMonotonic(t *testing.T) {
	// Applying PRChecksObserved twice: second should be no-op (stays true).
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(PRChecksObserved{Repo: testRepo, Number: 1})

	var fired int
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) { fired++ }))
	_, changes, _ := s.Apply(PRChecksObserved{Repo: testRepo, Number: 1})
	if len(changes) != 0 || fired != 0 {
		t.Errorf("second PRChecksObserved was not a no-op (changes=%d, fired=%d)", len(changes), fired)
	}
}

// ---- ItemDeepFetched clears LastDeepFetchFailureAt ----

func TestItemDeepFetchedClearsFailureAt(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Now()
	s.Apply(DeepFetchFailed{Repo: testRepo, Number: 1, At: at})
	if st := getItem(t, s, testRepo, 1); st.LastDeepFetchFailureAt.IsZero() {
		t.Fatal("precondition: LastDeepFetchFailureAt not set")
	}

	fresh := testProjectItem(testRepo, 1)
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: fresh})
	if st := getItem(t, s, testRepo, 1); !st.LastDeepFetchFailureAt.IsZero() {
		t.Errorf("LastDeepFetchFailureAt not cleared by ItemDeepFetched: %v", st.LastDeepFetchFailureAt)
	}
}

// ---- ProbeBoardItemUpdated ----

func TestProbeBoardItemUpdatedPreservesLabels(t *testing.T) {
	// IssueOpened seeds the item with Labels: ["bug"].
	s := newStoreWithItem(t, testRepo, 1)

	probe := gh.BoardProbeItem{
		ContentID:          "node1",
		ItemID:             "pvti1",
		Number:             1,
		IsClosed:           false,
		IsPR:               false,
		Status:             "Research",
		EffectiveUpdatedAt: time.Unix(2000, 0),
	}
	s.Apply(ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe})

	st := getItem(t, s, testRepo, 1)
	// Labels must not be wiped by the probe mutation.
	if len(st.Labels) == 0 {
		t.Fatal("labels were wiped by ProbeBoardItemUpdated; expected Labels to be preserved")
	}
	if !containsString(st.Labels, "bug") {
		t.Errorf("expected label 'bug' to be preserved, got %v", st.Labels)
	}
}

func TestProbeBoardItemUpdatedPropagatesIsClosed(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)

	probe := gh.BoardProbeItem{
		ContentID: "node1",
		Number:    1,
		IsClosed:  true,
		State:     "CLOSED",
	}
	snap := applyExpect(t, s, ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe}, StateChanged)
	st := snap.State()
	if !st.IsClosed {
		t.Error("IsClosed not set to true by ProbeBoardItemUpdated")
	}
	if st.State != "closed" {
		t.Errorf("State = %q; want %q", st.State, "closed")
	}
}

func TestProbeBoardItemUpdatedUpdatesStatus(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)

	probe := gh.BoardProbeItem{
		ContentID: "node1",
		Number:    1,
		Status:    "Review",
	}
	snap := applyExpect(t, s, ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe}, StatusChanged)
	st := snap.State()
	if st.Status != "Review" {
		t.Errorf("Status = %q; want %q", st.Status, "Review")
	}
}

func TestProbeBoardItemUpdatedNoChangeFlagsWhenSame(t *testing.T) {
	// Seed with IsClosed=false, Status="Implement".
	s := newStoreWithItem(t, testRepo, 1)

	probe := gh.BoardProbeItem{
		ContentID: "node1",
		Number:    1,
		IsClosed:  false,
		Status:    "Implement", // same as seeded
	}
	_, changes, err := s.Apply(ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range changes {
		if c.Fields&(StateChanged|StatusChanged|LabelsChanged) != 0 {
			t.Errorf("unexpected change flags %b when probe matches existing state", c.Fields)
		}
	}
}

// ---- CommentBreakerInvocationRecorded / CommentBreakerReset (#1089) ----

func TestApplyCommentBreakerInvocationRecorded(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	t1 := time.Unix(1000, 0)
	applyExpect(t, s, CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: t1, Author: "alice"}, CommentBreakerChanged)

	st := getItem(t, s, testRepo, 1)
	if len(st.CommentBreaker.InvocationsAt) != 1 || !st.CommentBreaker.InvocationsAt[0].Equal(t1) {
		t.Fatalf("InvocationsAt = %v, want [%v]", st.CommentBreaker.InvocationsAt, t1)
	}
	if st.CommentBreaker.LastAuthor != "alice" {
		t.Errorf("LastAuthor = %q, want %q", st.CommentBreaker.LastAuthor, "alice")
	}

	// A second invocation appends and updates LastAuthor.
	t2 := time.Unix(2000, 0)
	applyExpect(t, s, CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: t2, Author: "some-bot"}, CommentBreakerChanged)
	st = getItem(t, s, testRepo, 1)
	if len(st.CommentBreaker.InvocationsAt) != 2 {
		t.Fatalf("InvocationsAt len = %d, want 2", len(st.CommentBreaker.InvocationsAt))
	}
	if st.CommentBreaker.LastAuthor != "some-bot" {
		t.Errorf("LastAuthor = %q, want %q", st.CommentBreaker.LastAuthor, "some-bot")
	}
}

// TestApplyCommentBreakerInvocationRecordedPrunesBeforeCutoff verifies that a
// non-zero Cutoff drops any InvocationsAt entries older than it before
// appending the new one — bounding growth for an issue that receives
// invocations sparser than the configured window (caught during PR review of
// #1089: without this, InvocationsAt grew without bound for such issues).
func TestApplyCommentBreakerInvocationRecordedPrunesBeforeCutoff(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	old1 := time.Unix(1000, 0)
	old2 := time.Unix(1100, 0)
	fresh := time.Unix(5000, 0)
	s.Apply(CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: old1, Author: "alice"})
	s.Apply(CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: old2, Author: "bob"})

	// Cutoff excludes old1/old2 but not fresh itself.
	cutoff := time.Unix(4000, 0)
	applyExpect(t, s, CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: fresh, Author: "carol", Cutoff: cutoff}, CommentBreakerChanged)

	st := getItem(t, s, testRepo, 1)
	if len(st.CommentBreaker.InvocationsAt) != 1 || !st.CommentBreaker.InvocationsAt[0].Equal(fresh) {
		t.Fatalf("InvocationsAt = %v, want [%v] (stale entries pruned by Cutoff)", st.CommentBreaker.InvocationsAt, fresh)
	}
}

// TestApplyCommentBreakerInvocationRecordedZeroCutoffNoPrune verifies that a
// zero Cutoff (the default) performs no pruning, preserving the existing
// append-only behavior for callers that don't opt in.
func TestApplyCommentBreakerInvocationRecordedZeroCutoffNoPrune(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	old := time.Unix(1000, 0)
	fresh := time.Unix(5000, 0)
	s.Apply(CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: old, Author: "alice"})

	applyExpect(t, s, CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: fresh, Author: "bob"}, CommentBreakerChanged)

	st := getItem(t, s, testRepo, 1)
	if len(st.CommentBreaker.InvocationsAt) != 2 {
		t.Fatalf("InvocationsAt len = %d, want 2 (zero Cutoff must not prune)", len(st.CommentBreaker.InvocationsAt))
	}
}

func TestApplyCommentBreakerReset(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	s.Apply(CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: time.Unix(1000, 0), Author: "alice"})

	applyExpect(t, s, CommentBreakerReset{Repo: testRepo, Number: 1}, CommentBreakerChanged)

	st := getItem(t, s, testRepo, 1)
	if len(st.CommentBreaker.InvocationsAt) != 0 {
		t.Errorf("InvocationsAt = %v, want empty after reset", st.CommentBreaker.InvocationsAt)
	}
	if st.CommentBreaker.LastAuthor != "" {
		t.Errorf("LastAuthor = %q, want empty after reset", st.CommentBreaker.LastAuthor)
	}
}

// TestApplyCommentBreakerResetNoOpOnEmpty verifies that resetting an
// already-empty breaker produces no Change (invariant I6: no-op mutations
// don't notify observers).
func TestApplyCommentBreakerResetNoOpOnEmpty(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	_, changes, err := s.Apply(CommentBreakerReset{Repo: testRepo, Number: 1})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("Apply(CommentBreakerReset) on already-empty breaker: got %d changes, want 0 (no-op)", len(changes))
	}
}

func TestCommentBreakerSnapshotAccessors(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	at := time.Unix(1234, 0)
	s.Apply(CommentBreakerInvocationRecorded{Repo: testRepo, Number: 1, At: at, Author: "bob"})

	snap, err := s.Get(testRepo, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	invocations := snap.CommentBreakerInvocationsAt()
	if len(invocations) != 1 || !invocations[0].Equal(at) {
		t.Errorf("CommentBreakerInvocationsAt() = %v, want [%v]", invocations, at)
	}
	if got := snap.CommentBreakerLastAuthor(); got != "bob" {
		t.Errorf("CommentBreakerLastAuthor() = %q, want %q", got, "bob")
	}

	// Mutating the returned slice must not affect the Store's internal state.
	invocations[0] = time.Unix(9999, 0)
	again := snap.CommentBreakerInvocationsAt()
	if !again[0].Equal(at) {
		t.Errorf("CommentBreakerInvocationsAt() leaked mutation: got %v, want %v", again[0], at)
	}
}

// ---- Snapshot immutability for LinkedPR fields ----

func TestHeldSnapshotLinkedPRFieldsUnchanged(t *testing.T) {
	// Verify that mutations to CIMergePendingSince and HasHadChecks do not affect
	// a held Snapshot taken before the mutation.
	s := newStoreWithItem(t, testRepo, 99)
	held, _ := s.Get(testRepo, 99)
	// held.LinkedPR() is nil at this point.

	s.Apply(CIMergePendingStarted{Repo: testRepo, Number: 99, At: time.Unix(999, 0)})
	s.Apply(PRChecksObserved{Repo: testRepo, Number: 99})

	// Held snapshot should still have nil LinkedPR.
	if lpr := held.LinkedPR(); lpr != nil {
		t.Errorf("held snapshot LinkedPR became non-nil after mutations: %+v", lpr)
	}
}
