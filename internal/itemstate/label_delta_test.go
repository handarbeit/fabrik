package itemstate

import (
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// collectChanges subscribes an observer and returns an accessor for the changes it saw.
func collectChanges(s *Store) func() []Change {
	var mu sync.Mutex
	var got []Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}))
	return func() []Change {
		mu.Lock()
		defer mu.Unlock()
		return append([]Change(nil), got...)
	}
}

func TestLabelDelta_EngineWriteThenEchoIsOneDelta(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	seen := collectChanges(s)

	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:paused"})
	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "fabrik:paused", Sender: "bot[bot]", EchoOfEngine: true})

	got := seen()
	if len(got) != 1 {
		t.Fatalf("observer saw %d changes, want 1 (echo must be a no-op)", len(got))
	}
	c := got[0]
	if c.Origin != OriginEngine || len(c.LabelDeltas) != 1 || c.LabelDeltas[0] != (LabelDelta{Label: "fabrik:paused", Added: true}) {
		t.Fatalf("unexpected change: %+v", c)
	}
}

func TestLabelDelta_WebhookFirstThenWriteThroughIsOneDelta(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	seen := collectChanges(s)

	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "x", Sender: "alice"})
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "x"})

	got := seen()
	if len(got) != 1 {
		t.Fatalf("observer saw %d changes, want 1", len(got))
	}
	if got[0].Origin != OriginWebhook || got[0].Sender != "alice" || got[0].EchoOfEngine {
		t.Fatalf("unexpected attribution: %+v", got[0])
	}
}

func TestLabelDelta_RemovalCarriesDirection(t *testing.T) {
	s := NewStore(nil)
	it := testProjectItem(testRepo, 1)
	it.Labels = []string{"a", "b"}
	s.Apply(IssueOpened{Item: it})
	seen := collectChanges(s)

	s.Apply(IssueUnlabeled{Repo: testRepo, Number: 1, Label: "a", Sender: "alice"})
	got := seen()
	if len(got) != 1 || len(got[0].LabelDeltas) != 1 || got[0].LabelDeltas[0] != (LabelDelta{Label: "a", Added: false}) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestLabelDelta_ReconcileUnchangedEmitsNothing(t *testing.T) {
	s := NewStore(nil)
	it := testProjectItem(testRepo, 1)
	it.Labels = []string{"a"}
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{it}})
	seen := collectChanges(s)

	s.Apply(BoardReconciled{Items: []gh.ProjectItem{it}})
	if got := seen(); len(got) != 0 {
		t.Fatalf("unchanged reconcile produced %d changes", len(got))
	}
}

func TestLabelDelta_ReconcileFirstObservationOfExistingItem(t *testing.T) {
	s := NewStore(nil)
	it := testProjectItem(testRepo, 1)
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{it}})
	seen := collectChanges(s)

	it.Labels = []string{"human-applied"}
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{it}})
	got := seen()
	want := LabelDelta{Label: "human-applied", Added: true}
	if len(got) != 1 || got[0].Origin != OriginOther || !containsDelta(got[0].LabelDeltas, want) {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestLabelDelta_FirstPopulationIsBaseline(t *testing.T) {
	s := NewStore(nil)
	seen := collectChanges(s)
	it := testProjectItem(testRepo, 1)
	it.Labels = []string{"a", "b", "fabrik:paused"}
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{it}})

	it2 := testProjectItem(testRepo, 2)
	it2.Labels = []string{"c"}
	s.Apply(IssueOpened{Item: it2})

	for _, c := range seen() {
		if len(c.LabelDeltas) != 0 {
			t.Fatalf("first population emitted deltas: %+v", c)
		}
	}
}

func TestLabelDelta_ResetEmitsNoDeltas(t *testing.T) {
	s := NewStore(nil)
	seen := collectChanges(s)
	it := testProjectItem(testRepo, 1)
	it.Labels = []string{"a"}
	s.Reset([]gh.ProjectItem{it})
	for _, c := range seen() {
		if len(c.LabelDeltas) != 0 {
			t.Fatalf("Reset emitted deltas: %+v", c)
		}
	}
}

func TestLabelDelta_WebhookLabelOnUncachedItemIsAChange(t *testing.T) {
	s := NewStore(nil)
	seen := collectChanges(s)
	s.Apply(IssueLabeled{Repo: testRepo, Number: 9, Label: "fabrik:paused", Sender: "alice"})
	got := seen()
	if len(got) != 1 || len(got[0].LabelDeltas) != 1 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func containsDelta(ds []LabelDelta, d LabelDelta) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}
