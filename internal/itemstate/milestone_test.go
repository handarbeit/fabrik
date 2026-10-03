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
