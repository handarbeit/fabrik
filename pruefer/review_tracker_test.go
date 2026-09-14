package pruefer

import "testing"

func TestReviewTracker_RecordThenRecall_RoundTrips(t *testing.T) {
	tr := NewReviewTracker()
	if tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("Recall before any Record should report false")
	}
	tr.Record("owner", "repo", 1, "sha1")
	if !tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("Recall after Record for the same tuple should report true")
	}
}

func TestReviewTracker_DistinctHeadSHA_DoesNotCollide(t *testing.T) {
	tr := NewReviewTracker()
	tr.Record("owner", "repo", 1, "sha1")
	if tr.Recall("owner", "repo", 1, "sha2") {
		t.Fatal("a different head SHA on the same PR must not be recalled as reviewed")
	}
}

func TestReviewTracker_DistinctPRNumber_DoesNotCollide(t *testing.T) {
	tr := NewReviewTracker()
	tr.Record("owner", "repo", 1, "sha1")
	if tr.Recall("owner", "repo", 2, "sha1") {
		t.Fatal("a different PR number must not be recalled as reviewed")
	}
}

func TestReviewTracker_DistinctOwnerOrRepo_DoesNotCollide(t *testing.T) {
	tr := NewReviewTracker()
	tr.Record("owner", "repo", 1, "sha1")
	if tr.Recall("other-owner", "repo", 1, "sha1") {
		t.Fatal("a different owner must not be recalled as reviewed")
	}
	if tr.Recall("owner", "other-repo", 1, "sha1") {
		t.Fatal("a different repo must not be recalled as reviewed")
	}
}

func TestReviewTracker_CaseInsensitiveOwnerRepo(t *testing.T) {
	tr := NewReviewTracker()
	tr.Record("Owner", "Repo", 1, "sha1")
	if !tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("owner/repo comparison should be case-insensitive, matching alreadyReviewedAtHead's EqualFold convention")
	}
	if !tr.Recall("OWNER", "REPO", 1, "sha1") {
		t.Fatal("owner/repo comparison should be case-insensitive regardless of casing direction")
	}
}

func TestReviewTracker_NilReceiver_IsSafeNoOp(t *testing.T) {
	var tr *ReviewTracker
	if tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("nil tracker's Recall must always report false")
	}
	// Must not panic.
	tr.Record("owner", "repo", 1, "sha1")
	if tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("Record on a nil receiver must remain a no-op — Recall must still report false")
	}
}

func TestReviewTracker_ZeroValue_IsUsable(t *testing.T) {
	var tr ReviewTracker
	if tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("zero-value tracker's Recall before any Record should report false")
	}
	tr.Record("owner", "repo", 1, "sha1")
	if !tr.Recall("owner", "repo", 1, "sha1") {
		t.Fatal("zero-value tracker must lazily initialize its map on Record")
	}
}
