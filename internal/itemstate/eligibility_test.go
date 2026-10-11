package itemstate

import (
	gh "github.com/handarbeit/fabrik/github"
	"testing"
	"time"
)

func eligibilityOf(t *testing.T, s *Store, n int) time.Time {
	t.Helper()
	snap, ok := s.Peek(testRepo, n)
	if !ok {
		t.Fatalf("item %d missing", n)
	}
	return snap.State().EligibilityChangedAt
}

func TestEligibilityChangedAt_StampsOnEligibilityLabels(t *testing.T) {
	for _, label := range []string{"fabrik:cruise", "fabrik:yolo", "fabrik:blocked", "fabrik:paused", "stage:Specify:complete"} {
		s := NewStore(nil)
		s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
		if !eligibilityOf(t, s, 1).IsZero() {
			t.Fatalf("%s: a freshly populated item must carry no stamp", label)
		}
		before := time.Now()
		s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: label})
		if got := eligibilityOf(t, s, 1); got.Before(before) {
			t.Errorf("add %s: stamp %v not set", label, got)
		}
		// Removal stamps too (a decision resolved, a blocker cleared).
		first := eligibilityOf(t, s, 1)
		time.Sleep(2 * time.Millisecond)
		s.Apply(LocalLabelRemoved{Repo: testRepo, Number: 1, Label: label})
		if got := eligibilityOf(t, s, 1); !got.After(first) {
			t.Errorf("remove %s: stamp %v did not advance past %v", label, got, first)
		}
	}
}

func TestEligibilityChangedAt_IgnoresOtherLabelsAndBaselines(t *testing.T) {
	s := NewStore(nil)
	it := testProjectItem(testRepo, 1)
	it.Labels = []string{"fabrik:cruise", "stage:Specify:complete"}
	s.Apply(IssueOpened{Item: it})
	if !eligibilityOf(t, s, 1).IsZero() {
		t.Fatalf("a population carrying eligibility labels is a baseline, not a change")
	}
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "model:opus"})
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "stage:Research:in_progress"})
	s.Apply(LocalLabelAdded{Repo: testRepo, Number: 1, Label: "fabrik:awaiting-ci"})
	if !eligibilityOf(t, s, 1).IsZero() {
		t.Fatalf("unrelated labels must not stamp")
	}
}

func TestEligibilityChangedAt_WebhookAndBoardReconcile(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})
	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "fabrik:yolo", Sender: "alice"})
	if eligibilityOf(t, s, 1).IsZero() {
		t.Fatalf("a human-applied autonomy label (webhook) must stamp")
	}

	// A reconcile that changes the label set of a held item stamps as well.
	s2 := NewStore(nil)
	s2.Apply(IssueOpened{Item: testProjectItem(testRepo, 2)})
	changed := testProjectItem(testRepo, 2)
	changed.Labels = []string{"fabrik:cruise"}
	s2.Apply(BoardReconciled{Items: []gh.ProjectItem{changed}})
	if eligibilityOf(t, s2, 2).IsZero() {
		t.Fatalf("a reconcile that adds an autonomy label to a held item must stamp")
	}
}
