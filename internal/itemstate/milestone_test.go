package itemstate

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestMilestoneApplyAndFlags(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: gh.ProjectItem{ID: "I_1", Number: 1, Repo: "o/r", Status: "Plan"}})

	snap, _ := s.Get("o/r", 1)
	if st := snap.State(); st.MilestoneKnown || st.Milestone != nil {
		t.Fatalf("fresh item must have unknown milestone, got %+v / %v", st.Milestone, st.MilestoneKnown)
	}

	_, ch, _ := s.Apply(IssueMilestoneUpdated{Repo: "o/r", Number: 1, Milestone: &gh.Milestone{Title: "v1", Number: 3}})
	if len(ch) != 1 || ch[0].Fields&MilestoneChanged == 0 {
		t.Fatalf("milestoned: changes = %+v, want MilestoneChanged", ch)
	}
	// Idempotent: same value emits no change.
	if _, ch, _ = s.Apply(IssueMilestoneUpdated{Repo: "o/r", Number: 1, Milestone: &gh.Milestone{Title: "v1", Number: 3}}); len(ch) != 0 {
		t.Fatalf("repeat milestoned should be a no-op, got %+v", ch)
	}

	// A board item that does not carry the milestone (Known=false) never wipes it.
	s.Apply(ItemDeepFetched{Repo: "o/r", Number: 1, FreshState: gh.ProjectItem{ID: "I_1", Number: 1, Repo: "o/r", Status: "Plan", Body: "b"}})
	snap, _ = s.Get("o/r", 1)
	if m := snap.State().Milestone; m == nil || m.Number != 3 {
		t.Fatalf("milestone wiped by a non-carrying write: %+v", m)
	}

	// Mutating the snapshot copy must not reach the store.
	st := snap.State()
	st.Milestone.Title = "mutated"
	snap2, _ := s.Get("o/r", 1)
	if snap2.State().Milestone.Title != "v1" {
		t.Fatal("Snapshot.State() leaked the milestone pointer")
	}

	_, ch, _ = s.Apply(IssueMilestoneUpdated{Repo: "o/r", Number: 1, Milestone: nil})
	if len(ch) != 1 || ch[0].Fields&MilestoneChanged == 0 {
		t.Fatalf("demilestoned: changes = %+v", ch)
	}
	snap, _ = s.Get("o/r", 1)
	if st := snap.State(); !st.MilestoneKnown || st.Milestone != nil {
		t.Fatalf("after demilestoned want known-none, got %+v / %v", st.Milestone, st.MilestoneKnown)
	}
}

type countingFallback struct{ calls int }

func (f *countingFallback) FetchItem(repo string, number int) (gh.ProjectItem, error) {
	f.calls++
	return gh.ProjectItem{Repo: repo, Number: number}, nil
}

func TestPeekNeverCallsFallback(t *testing.T) {
	fb := &countingFallback{}
	s := NewStore(fb)
	if _, ok := s.Peek("o/r", 1); ok {
		t.Fatal("Peek on a miss must report ok=false")
	}
	if fb.calls != 0 {
		t.Fatalf("Peek invoked the FallbackFetcher %d times", fb.calls)
	}
	s.Apply(IssueOpened{Item: gh.ProjectItem{ID: "I_1", Number: 1, Repo: "o/r"}})
	if snap, ok := s.Peek("o/r", 1); !ok || snap.Number() != 1 {
		t.Fatal("Peek should find a cached item")
	}
	if fb.calls != 0 {
		t.Fatalf("FallbackFetcher called %d times", fb.calls)
	}
}

func TestScanVisitsEveryItemAndRepoWorkerKeys(t *testing.T) {
	s := NewStore(nil)
	for n := 1; n <= 3; n++ {
		s.Apply(IssueOpened{Item: gh.ProjectItem{ID: "I", Number: n, Repo: "o/r", Status: "Plan"}})
	}
	seen := map[int]bool{}
	s.Scan(func(it *ItemState) { seen[it.Number] = true })
	if len(seen) != 3 {
		t.Fatalf("Scan visited %v, want 3 items", seen)
	}
	s.EnterRepoWorker("o/r:dev")
	s.EnterRepoWorker("o/r")
	if keys := s.RepoWorkerKeys(); len(keys) != 2 {
		t.Fatalf("RepoWorkerKeys = %v, want 2", keys)
	}
}

func TestScanRaceWithApply(t *testing.T) {
	s := NewStore(nil)
	for n := 1; n <= 20; n++ {
		s.Apply(IssueOpened{Item: gh.ProjectItem{ID: "I", Number: n, Repo: "o/r"}})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			s.Apply(IssueLabeled{Repo: "o/r", Number: i%20 + 1, Label: "x"})
		}
	}()
	for i := 0; i < 200; i++ {
		s.Scan(func(it *ItemState) { _ = len(it.Labels) })
		s.Peek("o/r", 3)
	}
	<-done
}
