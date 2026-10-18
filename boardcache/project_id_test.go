package boardcache

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

func TestNoteProjectID_EmptyNeverOverwritesKnown(t *testing.T) {
	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
	c.NoteProjectID("")
	if got := c.ProjectID(); got != "" {
		t.Fatalf("empty note recorded %q", got)
	}
	c.NoteProjectID("PID")
	c.NoteProjectID("")
	if got := c.ProjectID(); got != "PID" {
		t.Fatalf("ProjectID = %q, want PID", got)
	}
	c.Reconcile(&gh.ProjectBoard{ProjectID: ""})
	if got := c.ProjectID(); got != "PID" {
		t.Fatalf("Reconcile blanked the ID: %q", got)
	}
	c.BootstrapFromProbe(nil, "")
	if got := c.ProjectID(); got != "PID" {
		t.Fatalf("BootstrapFromProbe blanked the ID: %q", got)
	}
}

func TestFetchProjectBoard_FallbackBoardRecordsProjectID(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{
		ProjectID: "PID", Items: []gh.ProjectItem{{Number: 5, Repo: "o/r"}},
	}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	if _, err := c.FetchProjectBoard("o", "r", 1, "organization"); err != nil {
		t.Fatal(err)
	}
	if got := c.ProjectID(); got != "PID" {
		t.Fatalf("ProjectID = %q, want PID", got)
	}
}

func TestFetchProjectBoard_NeverServesEmptyProjectIDWithItems(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{
		ProjectID: "PID", Items: []gh.ProjectItem{{Number: 5, Repo: "o/r"}},
	}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	// Fill the store without recording an ID (the degraded-start shape).
	c.store.Reset([]gh.ProjectItem{{Number: 5, Repo: "o/r", Status: "Plan"}})
	if c.ProjectID() != "" {
		t.Fatal("precondition: ID must be empty")
	}
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatal(err)
	}
	if board.ProjectID != "PID" {
		t.Fatalf("served ProjectID %q, want PID", board.ProjectID)
	}
	if c.ProjectID() != "PID" {
		t.Fatalf("ID not recorded from the fallback board: %q", c.ProjectID())
	}
}

func TestFetchProjectBoard_EmptyIDGuardNeutralised(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{ProjectID: "PID"}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.SetEmptyIDBoardGuardDisabledForTest(true)
	c.store.Reset([]gh.ProjectItem{{Number: 5, Repo: "o/r", Status: "Plan"}})
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatal(err)
	}
	if board.ProjectID != "" {
		t.Fatalf("neutralised guard should serve the empty ID, got %q", board.ProjectID)
	}
}

func TestFetchProjectBoard_KnownIDServedFromCache(t *testing.T) {
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{ProjectID: "OTHER"}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.store.Reset([]gh.ProjectItem{{Number: 5, Repo: "o/r", Status: "Plan"}})
	c.NoteProjectID("PID")
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatal(err)
	}
	if board.ProjectID != "PID" || len(board.Items) != 1 {
		t.Fatalf("expected cache-served board with PID, got %+v", board)
	}
}

func TestLightReconcile_RecordsProjectIDAtZeroDrift(t *testing.T) {
	item := gh.ProjectItem{Number: 5, Repo: "o/r", Status: "Plan"}
	mc := &mockClient{projectBoardResult: &gh.ProjectBoard{ProjectID: "PID", Items: []gh.ProjectItem{item}}}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	c.store.Reset([]gh.ProjectItem{item})
	if _, _, _, err := c.LightReconcile("o", "r", 1, "organization"); err != nil {
		t.Fatal(err)
	}
	if got := c.ProjectID(); got != "PID" {
		t.Fatalf("ProjectID = %q, want PID", got)
	}
}

func TestProjectIDRecordingNeutralised(t *testing.T) {
	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
	c.SetProjectIDRecordingDisabledForTest(true)
	c.NoteProjectID("PID")
	if c.ProjectID() != "" {
		t.Fatal("recording should be disabled")
	}
}
