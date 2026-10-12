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

func TestLabelDelta_RequesterCarriedOnlyWhereTrusted(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	seen := collectChanges(s)

	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "a", Requester: "x"})
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "b"})
	// An unechoed webhook never carries a requester, even if one is set.
	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "c", Sender: "alice", Requester: "y"})
	// An echoed webhook (item write-through missed) keeps it.
	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "d", Sender: "bot[bot]", EchoOfEngine: true, Requester: "z"})
	s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: "a", Requester: "x"})

	got := seen()
	want := []string{"x", "", "", "z", "x"}
	if len(got) != len(want) {
		t.Fatalf("observer saw %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Requester != w {
			t.Errorf("change %d Requester = %q, want %q", i, got[i].Requester, w)
		}
	}
}

// labelLess returns a probe-style item: no labels observed.
func labelLess(n int) gh.ProjectItem {
	it := testProjectItem(testRepo, n)
	it.Status = "Specify"
	it.LabelsAbsent = true
	return it
}

func labelled(n int, labels ...string) gh.ProjectItem {
	it := testProjectItem(testRepo, n)
	it.Status = "Specify"
	it.Labels = labels
	return it
}

func TestLabelBaseline_LabelLessSeedThenDeepFetchIsSilent(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		s := NewStore(nil)
		s.SetLegacyLabelBaselineForTest(legacy)
		s.Reset([]gh.ProjectItem{labelLess(1)})
		seen := collectChanges(s)

		s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: labelled(1, "stage:Specify:complete", "fabrik:cruise")})
		var deltas int
		for _, c := range seen() {
			deltas += len(c.LabelDeltas)
		}
		if legacy && deltas == 0 {
			t.Fatal("non-vacuity: the legacy rule must emit deltas for the first labelled fetch")
		}
		if !legacy && deltas != 0 {
			t.Fatalf("first label-bearing population emitted %d deltas, want 0", deltas)
		}
		snap, _ := s.Get(testRepo, 1)
		if got := snap.State().Labels; len(got) != 2 || !snap.State().LabelsKnown {
			t.Fatalf("labels not stored/known: %v", got)
		}
	}
}

func TestLabelBaseline_LabelLessSeedThenShallowReconcileIsSilent(t *testing.T) {
	s := NewStore(nil)
	s.Reset([]gh.ProjectItem{labelLess(1), labelLess(2)})
	seen := collectChanges(s)
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{labelled(1, "a", "b"), labelled(2, "fabrik:yolo")}})
	for _, c := range seen() {
		if len(c.LabelDeltas) != 0 {
			t.Fatalf("reconcile of label-less seed emitted deltas: %+v", c)
		}
	}
	// A real change afterwards is still one delta.
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{labelled(1, "a", "b", "c"), labelled(2, "fabrik:yolo")}})
	var deltas []LabelDelta
	for _, c := range seen() {
		deltas = append(deltas, c.LabelDeltas...)
	}
	if len(deltas) != 1 || deltas[0] != (LabelDelta{Label: "c", Added: true}) {
		t.Fatalf("want exactly the real change, got %+v", deltas)
	}
}

func TestLabelBaseline_ShallowItemUpdatedIsSilentWhenLabelsUnknown(t *testing.T) {
	s := NewStore(nil)
	s.Reset([]gh.ProjectItem{labelLess(1)})
	seen := collectChanges(s)
	s.Apply(ShallowBoardItemUpdated{Item: labelled(1, "x")})
	for _, c := range seen() {
		if len(c.LabelDeltas) != 0 {
			t.Fatalf("unexpected deltas: %+v", c)
		}
	}
}

func TestLabelBaseline_ExplicitLabelWriteOnUnknownLabelsStillEmits(t *testing.T) {
	s := NewStore(nil)
	s.Reset([]gh.ProjectItem{labelLess(1)})
	seen := collectChanges(s)
	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "fabrik:paused", Sender: "alice"})
	got := seen()
	if len(got) != 1 || len(got[0].LabelDeltas) != 1 {
		t.Fatalf("explicit label write must emit: %+v", got)
	}
}

func TestLabelBaseline_LabelAbsentOpenedKeepsKnownLabels(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: labelled(1, "keep")})
	s.Apply(IssueOpened{Item: labelLess(1)})
	snap, _ := s.Get(testRepo, 1)
	if got := snap.State().Labels; len(got) != 1 || got[0] != "keep" {
		t.Fatalf("label-less producer wiped labels: %v", got)
	}
}

func TestChange_CreatedPrevStatusAndClosed(t *testing.T) {
	s := NewStore(nil)
	seen := collectChanges(s)
	s.Apply(IssueOpened{Item: labelled(1, "x")})
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Research", Requester: "ov"})
	s.Apply(IssueClosed{Repo: testRepo, Number: 1, Sender: "alice"})
	got := seen()
	if len(got) != 3 {
		t.Fatalf("changes = %d, want 3: %+v", len(got), got)
	}
	if !got[0].Created || got[0].PrevStatus != "" {
		t.Fatalf("creation change: %+v", got[0])
	}
	if got[1].Created || got[1].PrevStatus != "Specify" || got[1].Origin != OriginEngine || got[1].Requester != "ov" {
		t.Fatalf("move change: %+v", got[1])
	}
	if !got[2].Closed || got[2].Sender != "alice" || got[2].Origin != OriginWebhook {
		t.Fatalf("close change: %+v", got[2])
	}
}

func TestChange_ResetNeverCreated(t *testing.T) {
	s := NewStore(nil)
	seen := collectChanges(s)
	s.Reset([]gh.ProjectItem{labelled(1, "x")})
	for _, c := range seen() {
		if c.Created {
			t.Fatalf("Reset must not mark Created: %+v", c)
		}
	}
}

func TestChange_StatusEchoIsOneChange(t *testing.T) {
	s := NewStore(nil)
	it := labelled(1, "x")
	it.ItemID = "PVTI_1"
	s.Apply(IssueOpened{Item: it})
	seen := collectChanges(s)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Research"})
	s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_1", NewStatus: "Research", Sender: "bot[bot]", EchoOfEngine: true})
	if got := seen(); len(got) != 1 || got[0].PrevStatus != "Specify" {
		t.Fatalf("want one change from Specify: %+v", got)
	}
}

func TestChange_UnknownPrevStatusIsBaseline(t *testing.T) {
	s := NewStore(nil)
	it := labelled(1)
	it.Status = ""
	s.Apply(IssueOpened{Item: it})
	seen := collectChanges(s)
	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Specify"})
	got := seen()
	if len(got) != 1 || got[0].PrevStatus != "" {
		t.Fatalf("first-known status must be a baseline: %+v", got)
	}
}

// The first label-bearing population is flagged even when the label set is empty
// (a downtime change to "no labels" must still be diffable, #2169), by every
// producer, and exactly once.
func TestChange_LabelsBaselinedOnFirstPopulationOnly(t *testing.T) {
	s := NewStore(nil)
	s.Reset([]gh.ProjectItem{labelLess(1), labelLess(2), labelLess(3)})
	seen := collectChanges(s)
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 1, FreshState: labelled(1)})
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{labelled(2, "a"), labelled(3)}})
	s.Apply(BoardReconciled{Items: []gh.ProjectItem{labelled(2, "a", "b")}})
	baselined := map[int]int{}
	for _, c := range seen() {
		if c.LabelsBaselined {
			baselined[c.Number]++
			if len(c.LabelDeltas) != 0 {
				t.Fatalf("baseline must carry no deltas: %+v", c)
			}
		}
	}
	for _, n := range []int{1, 2, 3} {
		if baselined[n] != 1 {
			t.Fatalf("item %d baselined %d times, want once (%v)", n, baselined[n], baselined)
		}
	}
}
