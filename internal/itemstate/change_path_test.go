package itemstate

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestChangePath_ReconcileDeepFetchProbeAreNamed(t *testing.T) {
	s := NewStore(nil)
	item := testProjectItem(testRepo, 1)
	item.Status = "Plan"
	s.Apply(IssueOpened{Item: item})
	seen := collectChanges(s)

	moved := item
	moved.Status = "Implement"
	s.Apply(ShallowBoardItemUpdated{Repo: testRepo, Number: 1, Item: moved})
	moved.Status = "Review"
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: moved})
	s.Apply(FromProbe{Inner: ProbeBoardItemUpdated{Repo: testRepo, Number: 1, Item: gh.BoardProbeItem{Repo: testRepo, Number: 1, Status: "Validate"}}})

	got := seen()
	if len(got) != 3 {
		t.Fatalf("saw %d changes, want 3: %+v", len(got), got)
	}
	want := []struct {
		path     ChangePath
		from, to string
	}{
		{PathReconcile, "Plan", "Implement"},
		{PathDeepFetch, "Implement", "Review"},
		{PathProbe, "Review", "Validate"},
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].StatusFrom != w.from || got[i].StatusTo != w.to {
			t.Errorf("change %d = path %v %q->%q, want %v %q->%q", i, got[i].Path, got[i].StatusFrom, got[i].StatusTo, w.path, w.from, w.to)
		}
	}
}

func TestChangePath_EngineStatusWriteAndWebhookMove(t *testing.T) {
	s := NewStore(nil)
	item := testProjectItem(testRepo, 1)
	item.Status = "Plan"
	s.Apply(IssueOpened{Item: item})
	s.Apply(ItemIDRegistered{Repo: testRepo, Number: 1, ItemID: "PVTI_1"})
	seen := collectChanges(s)

	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Implement"})
	s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: "Review"})

	got := seen()
	if len(got) != 2 {
		t.Fatalf("saw %d changes, want 2", len(got))
	}
	if !got[0].EngineStatusWrite || got[0].Path != PathNone || got[0].StatusFrom != "Plan" || got[0].StatusTo != "Implement" {
		t.Errorf("engine write change = %+v", got[0])
	}
	if got[1].EngineStatusWrite || got[1].Origin != OriginWebhook || got[1].StatusTo != "Review" {
		t.Errorf("webhook move change = %+v", got[1])
	}
}

func TestChangePath_BoardReconciledIsReconcile(t *testing.T) {
	s := NewStore(nil)
	item := testProjectItem(testRepo, 1)
	item.Status = "Plan"
	item.Labels = []string{"a"}
	s.Apply(IssueOpened{Item: item})
	seen := collectChanges(s)

	item.Status = "Implement"
	item.Labels = nil
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{item}})
	got := seen()
	if len(got) != 1 || got[0].Path != PathReconcile || got[0].StatusTo != "Implement" || len(got[0].LabelDeltas) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
}
