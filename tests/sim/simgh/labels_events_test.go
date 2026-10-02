package simgh

import (
	"reflect"
	"testing"
)

func TestLabelEventsRecordOnlyRealStateChanges(t *testing.T) {
	s, _ := seedBasicBoard(t)
	const num = 7
	before, err := s.LastLabelEventSeq("acme", "widgets", num)
	if err != nil {
		t.Fatalf("LastLabelEventSeq: %v", err)
	}

	steps := []struct {
		name string
		do   func() error
	}{
		{"add a", func() error { return s.AddLabelToIssue("acme", "widgets", num, "a") }},
		{"re-add a (no-op)", func() error { return s.AddLabelToIssue("acme", "widgets", num, "a") }},
		{"remove b (absent, errors)", func() error { _ = s.RemoveLabelFromIssue("acme", "widgets", num, "b"); return nil }},
		{"remove a", func() error { return s.RemoveLabelFromIssue("acme", "widgets", num, "a") }},
		{"add a again", func() error { return s.AddLabelToIssue("acme", "widgets", num, "a") }},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
	}

	evs, err := s.LabelEvents("acme", "widgets", num)
	if err != nil {
		t.Fatalf("LabelEvents: %v", err)
	}
	type kl struct {
		Kind  LabelEventKind
		Label string
	}
	var got []kl
	prev := before
	for _, e := range evs {
		if e.Seq <= prev {
			t.Errorf("Seq %d not strictly increasing after %d", e.Seq, prev)
		}
		prev = e.Seq
		got = append(got, kl{e.Kind, e.Label})
	}
	want := []kl{
		{LabelEventLabeled, "a"},
		{LabelEventUnlabeled, "a"},
		{LabelEventLabeled, "a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	last, _ := s.LastLabelEventSeq("acme", "widgets", num)
	if last != evs[len(evs)-1].Seq {
		t.Errorf("LastLabelEventSeq = %d, want %d", last, evs[len(evs)-1].Seq)
	}
}

func TestLabelEventsAreScopedPerIssue(t *testing.T) {
	s, _ := seedBasicBoard(t)
	if err := s.AddLabelToIssue("acme", "widgets", 7, "a"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	if _, err := s.LabelEvents("acme", "widgets", 999); err == nil {
		t.Error("LabelEvents on a missing issue: want error, got nil")
	}
	if _, err := s.LastLabelEventSeq("acme", "nope", 7); err == nil {
		t.Error("LastLabelEventSeq on a missing repo: want error, got nil")
	}
}
