package simgh

import (
	"testing"
	"time"
)

// TestFetchLabelRemovedAt_PauseResumeRepause pins the #2059 accessor: it reports
// the newest `unlabeled` event for a label, is zero while no removal exists, and
// moves forward on each later removal. It reads the event log because removal
// deletes the applied-at entry.
func TestFetchLabelRemovedAt_PauseResumeRepause(t *testing.T) {
	s, clk := seedBasicBoard(t)
	const label = "fabrik:paused"

	got, err := s.FetchLabelRemovedAt("acme", "widgets", 7, label)
	if err != nil || !got.IsZero() {
		t.Fatalf("before any event: got (%v, %v), want zero, nil", got, err)
	}

	if err := s.AddLabelToIssue("acme", "widgets", 7, label); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, _ = s.FetchLabelRemovedAt("acme", "widgets", 7, label)
	if !got.IsZero() {
		t.Fatalf("a label that was only applied has a removal time %v", got)
	}

	clk.Advance(time.Hour)
	if err := s.RemoveLabelFromIssue("acme", "widgets", 7, label); err != nil {
		t.Fatalf("resume: %v", err)
	}
	firstResume := clk.Now()
	got, _ = s.FetchLabelRemovedAt("acme", "widgets", 7, label)
	if !got.Equal(firstResume) {
		t.Fatalf("after resume: got %v, want %v", got, firstResume)
	}

	clk.Advance(time.Hour)
	_ = s.AddLabelToIssue("acme", "widgets", 7, label)
	clk.Advance(time.Hour)
	if err := s.RemoveLabelFromIssue("acme", "widgets", 7, label); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	got, _ = s.FetchLabelRemovedAt("acme", "widgets", 7, label)
	if !got.Equal(clk.Now()) {
		t.Fatalf("after second resume: got %v, want %v", got, clk.Now())
	}

	// Other labels are unaffected.
	other, _ := s.FetchLabelRemovedAt("acme", "widgets", 7, "fabrik:awaiting-ci")
	if !other.IsZero() {
		t.Fatalf("unrelated label has a removal time %v", other)
	}
}

// TestDropLabelWrite pins the #2551 fault: a dropped write returns success and
// changes nothing, a dropped removal likewise, only `times` writes are dropped,
// and other labels are unaffected.
func TestDropLabelWrite(t *testing.T) {
	s, _ := seedBasicBoard(t)
	const label = "fabrik:locked:me"

	s.DropLabelWrite(LabelEventLabeled, label, 1)
	if err := s.AddLabelToIssue("acme", "widgets", 7, label); err != nil {
		t.Fatalf("dropped add must report success: %v", err)
	}
	if labels, _ := s.FetchLabels("acme", "widgets", 7); contains(labels, label) {
		t.Fatalf("dropped add stuck: %v", labels)
	}
	if evs, _ := s.LabelEvents("acme", "widgets", 7); len(evs) != 0 {
		t.Fatalf("dropped add left events: %v", evs)
	}
	if err := s.AddLabelToIssue("acme", "widgets", 7, label); err != nil {
		t.Fatal(err)
	}
	if labels, _ := s.FetchLabels("acme", "widgets", 7); !contains(labels, label) {
		t.Fatalf("second add (rule spent) did not stick: %v", labels)
	}

	s.DropLabelWrite(LabelEventUnlabeled, label, 1)
	if err := s.RemoveLabelFromIssue("acme", "widgets", 7, label); err != nil {
		t.Fatalf("dropped removal must report success: %v", err)
	}
	if labels, _ := s.FetchLabels("acme", "widgets", 7); !contains(labels, label) {
		t.Fatalf("dropped removal removed the label: %v", labels)
	}
	if err := s.RemoveLabelFromIssue("acme", "widgets", 7, label); err != nil {
		t.Fatal(err)
	}
	if labels, _ := s.FetchLabels("acme", "widgets", 7); contains(labels, label) {
		t.Fatalf("second removal (rule spent) did not stick: %v", labels)
	}
	if err := s.AddLabelToIssue("acme", "widgets", 7, "other"); err != nil {
		t.Fatal(err)
	}
}
