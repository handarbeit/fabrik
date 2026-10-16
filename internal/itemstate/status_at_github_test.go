package itemstate

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ---- StatusAtGitHub (#2246, ADR-2246) ----
//
// GitHub's own time for the move into the current Status. Stamped inside the
// same "Status changed" guard as StatusEnteredAt: once per transition, never by
// a no-op re-apply, zero when the source carries no time.

var gitHubMoveT = time.Date(2026, 10, 14, 21, 43, 5, 0, time.UTC)

func statusAtGitHub(t *testing.T, s *Store) time.Time {
	t.Helper()
	return getItem(t, s, testRepo, 1).StatusAtGitHub
}

// A deep fetch writes Status only as a first population (#2344); a move of an
// existing item arrives through a listing path, so ShallowBoardItemUpdated stands in.
func TestStatusAtGitHubStampedByProjectItem(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Review"
	pi.StatusAtGitHub = gitHubMoveT
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.Equal(gitHubMoveT) {
		t.Errorf("StatusAtGitHub = %v, want %v", got, gitHubMoveT)
	}
}

func TestStatusAtGitHubStampedByShallowBoardItemUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Queued"
	pi.StatusAtGitHub = gitHubMoveT
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.Equal(gitHubMoveT) {
		t.Errorf("StatusAtGitHub = %v, want %v", got, gitHubMoveT)
	}
}

func TestStatusAtGitHubStampedByProbeBoardItemUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	probe := gh.BoardProbeItem{Repo: testRepo, Number: 1, Status: "Validate", StatusAtGitHub: gitHubMoveT}
	if _, _, err := s.Apply(ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.Equal(gitHubMoveT) {
		t.Errorf("StatusAtGitHub = %v, want %v", got, gitHubMoveT)
	}
}

func TestStatusAtGitHubStampedByProjectV2ItemEdited(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_test_2246"
	if _, _, err := s.Apply(IssueOpened{Item: pi}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_test_2246", NewStatus: "Done", At: gitHubMoveT}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.Equal(gitHubMoveT) {
		t.Errorf("StatusAtGitHub = %v, want %v", got, gitHubMoveT)
	}
	// A payload with no time is unknown, not the previous value.
	if _, _, err := s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_test_2246", NewStatus: "Queued"}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.IsZero() {
		t.Errorf("StatusAtGitHub = %v after a time-less edit, want zero", got)
	}
}

// The engine is the writer of its own moves: daemon time is the move time, so
// the GitHub time is cleared rather than left pointing at the previous Status.
func TestStatusAtGitHubClearedByLocalStatusUpdated(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Review"
	pi.StatusAtGitHub = gitHubMoveT
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Queued"}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.IsZero() {
		t.Errorf("StatusAtGitHub = %v after an engine write, want zero", got)
	}
}

// A no-op re-apply of the same Status, or a later listing that reads an older
// or newer time for it, never moves the stamp: a formed batch stays stable.
func TestStatusAtGitHubUnchangedByNoOpReapply(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Queued"
	pi.StatusAtGitHub = gitHubMoveT
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{gitHubMoveT.Add(-time.Hour), gitHubMoveT.Add(time.Hour), {}} {
		pi.StatusAtGitHub = at
		if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
			t.Fatal(err)
		}
		probe := gh.BoardProbeItem{Repo: testRepo, Number: 1, Status: "Queued", StatusAtGitHub: at}
		if _, _, err := s.Apply(ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: probe}); err != nil {
			t.Fatal(err)
		}
		if got := statusAtGitHub(t, s); !got.Equal(gitHubMoveT) {
			t.Fatalf("StatusAtGitHub = %v after a no-op re-apply with %v, want %v", got, at, gitHubMoveT)
		}
	}
}

// Reset (restart bootstrap) seeds true entry times, removing ADR-1833's
// "restart reshuffles the batch once" limitation when GitHub supplies them.
func TestStatusAtGitHubSeededByReset(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Queued"
	pi.StatusAtGitHub = gitHubMoveT
	s.Reset([]gh.ProjectItem{pi})
	if got := statusAtGitHub(t, s); !got.Equal(gitHubMoveT) {
		t.Errorf("StatusAtGitHub = %v, want %v", got, gitHubMoveT)
	}
	// StatusEnteredAt stays observation time ("first seen by this process").
	withinTestTolerance(t, getItem(t, s, testRepo, 1).StatusEnteredAt)
}

func TestStatusAtGitHubZeroStaysZero(t *testing.T) {
	s := newStoreWithItem(t, testRepo, 1)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Queued"
	if _, _, err := s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: pi}); err != nil {
		t.Fatal(err)
	}
	if got := statusAtGitHub(t, s); !got.IsZero() {
		t.Errorf("StatusAtGitHub = %v, want zero", got)
	}
}
