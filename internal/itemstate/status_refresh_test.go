package itemstate

import (
	"testing"
	"time"
)

// refreshStore holds issue 1 at status "Plan" with an item ID registered.
func refreshStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(nil)
	s.SetEngineWriteGraceForTest(testGrace)
	pi := testProjectItem(testRepo, 1)
	pi.Status = "Plan"
	s.Apply(IssueOpened{Item: pi})
	return s
}

func refresh(s *Store, status string, fetchedAt time.Time) {
	s.Apply(StatusRefreshed{Repo: testRepo, Number: 1, Status: status, FetchedAt: fetchedAt})
}

// A refresh read taken before an engine move must not revert the move.
func TestStatusRefresh_ReadBeforeEngineMoveKeepsMove(t *testing.T) {
	s := refreshStore(t)
	readStart := time.Now()
	time.Sleep(2 * time.Millisecond)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Implement"})

	refresh(s, "Plan", readStart)

	if got := stateOf(t, s).Status; got != "Implement" {
		t.Fatalf("Status = %q, want the engine-moved Implement", got)
	}
}

// Non-vacuity: the legacy path (an engine write) does revert the move.
func TestStatusRefresh_LegacyEngineWriteRevertsMove(t *testing.T) {
	s := refreshStore(t)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Implement"})
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Plan"})
	if got := stateOf(t, s).Status; got != "Plan" {
		t.Fatalf("legacy path should overwrite, got %q", got)
	}
}

// A read older than a move observed from any source (here a webhook) is dropped.
func TestStatusRefresh_ReadBeforeObservedWebhookMoveIsDropped(t *testing.T) {
	s := refreshStore(t)
	s.Apply(ItemIDRegistered{Repo: testRepo, Number: 1, ItemID: "PVTI_1"})
	readStart := time.Now()
	time.Sleep(2 * time.Millisecond)
	s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: "Implement", Sender: "someone"})
	seen := collectChanges(s)

	refresh(s, "Plan", readStart)

	if got := stateOf(t, s).Status; got != "Implement" {
		t.Fatalf("Status = %q, want Implement", got)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("dropped read produced %d changes", n)
	}
}

// A read newer than every prior write and differing from the cache is applied.
func TestStatusRefresh_NewerDifferingReadApplied(t *testing.T) {
	s := refreshStore(t)
	time.Sleep(2 * time.Millisecond)
	seen := collectChanges(s)

	refresh(s, "Implement", time.Now())

	st := stateOf(t, s)
	if st.Status != "Implement" {
		t.Fatalf("Status = %q, want Implement", st.Status)
	}
	got := seen()
	if len(got) != 1 || got[0].Fields&StatusChanged == 0 || got[0].StatusFrom != "Plan" || got[0].StatusTo != "Implement" {
		t.Fatalf("changes = %+v", got)
	}
}

// A read fetched after the grace window past an engine write wins.
func TestStatusRefresh_ReadAfterGraceWins(t *testing.T) {
	s := refreshStore(t)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Implement"})
	// Within grace: kept even though fetched after the write.
	refresh(s, "Review", time.Now())
	if got := stateOf(t, s).Status; got != "Implement" {
		t.Fatalf("within grace Status = %q, want Implement", got)
	}
	// Past grace: applied.
	refresh(s, "Review", time.Now().Add(2*testGrace))
	if got := stateOf(t, s).Status; got != "Review" {
		t.Fatalf("after grace Status = %q, want Review", got)
	}
}

// A refresh never creates an engine-write record, changed or unchanged, and the
// change it yields is neither an engine move nor attributed to the engine.
func TestStatusRefresh_NeverRecordsEngineWriteOrAttribution(t *testing.T) {
	s := refreshStore(t)
	seen := collectChanges(s)
	time.Sleep(2 * time.Millisecond)

	refresh(s, "Implement", time.Now()) // changed
	refresh(s, "Implement", time.Now()) // unchanged
	refresh(s, "Review", time.Time{})   // zero fetch time: unprotected, still no record

	st := stateOf(t, s)
	if st.Status != "Review" {
		t.Fatalf("Status = %q", st.Status)
	}
	if len(st.EngineWriteAt) != 0 {
		t.Fatalf("EngineWriteAt = %v, want none", st.EngineWriteAt)
	}
	for _, c := range seen() {
		if c.EngineStatusWrite || c.Origin == OriginEngine || c.Requester != "" {
			t.Errorf("change attributed to the engine: %+v", c)
		}
		if c.Path != PathRefresh {
			t.Errorf("Path = %v, want PathRefresh", c.Path)
		}
	}
}

// An equal read applies nothing: no change, no timestamps.
func TestStatusRefresh_EqualReadAppliesNothing(t *testing.T) {
	s := refreshStore(t)
	before := stateOf(t, s)
	seen := collectChanges(s)

	refresh(s, "Plan", time.Now())

	after := stateOf(t, s)
	if !after.StatusEnteredAt.Equal(before.StatusEnteredAt) || !after.StatusAtGitHub.Equal(before.StatusAtGitHub) {
		t.Fatalf("timestamps changed: %v/%v -> %v/%v", before.StatusEnteredAt, before.StatusAtGitHub, after.StatusEnteredAt, after.StatusAtGitHub)
	}
	if n := len(seen()); n != 0 {
		t.Fatalf("equal read produced %d changes", n)
	}
}

// A confirmed engine-write record is dropped by an agreeing read, without a Status change.
func TestStatusRefresh_AgreeingReadConfirmsRecord(t *testing.T) {
	s := refreshStore(t)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Implement"})
	refresh(s, "Implement", time.Now())
	if n := len(stateOf(t, s).EngineWriteAt); n != 0 {
		t.Fatalf("record not confirmed: %v", stateOf(t, s).EngineWriteAt)
	}
}

// On a change StatusAtGitHub becomes unknown; Terminal clears.
func TestStatusRefresh_ChangeZeroesGitHubTimeAndClearsTerminal(t *testing.T) {
	s := refreshStore(t)
	s.Apply(ItemIDRegistered{Repo: testRepo, Number: 1, ItemID: "PVTI_1"})
	s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: "Implement", Sender: "x", At: time.Unix(5000, 0)})
	if stateOf(t, s).StatusAtGitHub.IsZero() {
		t.Fatal("precondition: webhook should stamp StatusAtGitHub")
	}
	time.Sleep(2 * time.Millisecond)
	refresh(s, "Review", time.Now())
	st := stateOf(t, s)
	if st.Status != "Review" || !st.StatusAtGitHub.IsZero() || st.Terminal {
		t.Fatalf("state = status %q, atGitHub %v, terminal %v", st.Status, st.StatusAtGitHub, st.Terminal)
	}
}

// An empty read applies nothing; an empty cached Status is populated.
func TestStatusRefresh_EmptyHandling(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.Status = ""
	s.Apply(IssueOpened{Item: pi})
	refresh(s, "", time.Now())
	if got := stateOf(t, s).Status; got != "" {
		t.Fatalf("empty read set Status %q", got)
	}
	refresh(s, "Plan", time.Now().Add(-time.Hour)) // even an old read populates an empty Status
	st := stateOf(t, s)
	if st.Status != "Plan" || len(st.EngineWriteAt) != 0 {
		t.Fatalf("state = %q %v", st.Status, st.EngineWriteAt)
	}
}

// The closed record is never confirmed, dropped or kept by a status-only read.
func TestStatusRefresh_LeavesClosedRecordAlone(t *testing.T) {
	s := refreshStore(t)
	s.Apply(LocalIssueClosed{Repo: testRepo, Number: 1})
	if _, ok := stateOf(t, s).EngineWriteAt["closed"]; !ok {
		t.Fatal("precondition: LocalIssueClosed should record an engine write")
	}
	refresh(s, "Plan", time.Now())
	st := stateOf(t, s)
	if _, ok := st.EngineWriteAt["closed"]; !ok || !st.IsClosed {
		t.Fatalf("closed record disturbed: closed=%v rec=%v", st.IsClosed, st.EngineWriteAt)
	}
}
