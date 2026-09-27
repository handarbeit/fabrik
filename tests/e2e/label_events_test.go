//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func ev(id int64, kind, label string) issueEvent { return issueEvent{ID: id, Kind: kind, Label: label} }

func TestParseIssueEvents(t *testing.T) {
	// Two concatenated pages (as --paginate emits), a label-less event, and an
	// out-of-order id to show list order is preserved.
	body := `[{"id":10,"event":"labeled","label":{"name":"fabrik:yolo"}},
	          {"id":11,"event":"commented"}]
	         [{"id":12,"event":"unlabeled","label":{"name":"fabrik:yolo"}}]`
	got, err := parseIssueEvents([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []issueEvent{ev(10, "labeled", "fabrik:yolo"), ev(11, "commented", ""), ev(12, "unlabeled", "fabrik:yolo")}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if m := maxEventID(got); m != 12 {
		t.Errorf("maxEventID = %d, want 12", m)
	}
	if m := maxEventID(nil); m != 0 {
		t.Errorf("maxEventID(nil) = %d, want 0", m)
	}
	if _, err := parseIssueEvents([]byte(`not json`)); err == nil {
		t.Error("want error for malformed JSON")
	}
}

func TestCheckYoloRemovedMidValidate(t *testing.T) {
	good := []issueEvent{
		ev(1, "labeled", "fabrik:yolo"),
		ev(2, "labeled", "stage:Validate:in_progress"),
		ev(3, "unlabeled", "fabrik:yolo"),
		ev(4, "labeled", "fabrik:awaiting-ci"),
	}
	if err := checkYoloRemovedMidValidate(good); err != nil {
		t.Errorf("good ordering: %v", err)
	}

	cases := []struct {
		name   string
		events []issueEvent
		want   string
	}{
		{"validate never started", []issueEvent{ev(1, "unlabeled", "fabrik:yolo"), ev(2, "labeled", "fabrik:awaiting-ci")}, "Validate never started"},
		{"removal never landed", []issueEvent{ev(1, "labeled", "stage:Validate:in_progress"), ev(2, "labeled", "fabrik:awaiting-ci")}, "removal never landed"},
		{"validate never completed", []issueEvent{ev(1, "labeled", "stage:Validate:in_progress"), ev(2, "unlabeled", "fabrik:yolo")}, "never signalled completion"},
		{"removed before validate", []issueEvent{ev(1, "unlabeled", "fabrik:yolo"), ev(2, "labeled", "stage:Validate:in_progress"), ev(3, "labeled", "fabrik:awaiting-ci")}, "before Validate started"},
		{"removed after validate", []issueEvent{ev(1, "labeled", "stage:Validate:in_progress"), ev(2, "labeled", "fabrik:awaiting-ci"), ev(3, "unlabeled", "fabrik:yolo")}, "missed the in-progress window"},
		{"other labels ignored", []issueEvent{ev(1, "labeled", "stage:Review:in_progress"), ev(2, "unlabeled", "fabrik:cruise")}, "Validate never started"},
	}
	for _, c := range cases {
		err := checkYoloRemovedMidValidate(c.events)
		if err == nil {
			t.Errorf("%s: want error, got nil", c.name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "INCONCLUSIVE") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q, want INCONCLUSIVE containing %q", c.name, err, c.want)
		}
	}
}

func TestCheckReworkSequence(t *testing.T) {
	// id 5 is the genuine earlier completion; sinceID=5 is captured just before
	// the comment is posted.
	base := []issueEvent{
		ev(5, "labeled", "stage:Research:complete"),
		ev(6, "labeled", "fabrik:editing"),
		ev(7, "labeled", "fabrik:reworking:Research"),
		ev(8, "unlabeled", "stage:Research:complete"),
		ev(9, "labeled", "stage:Research:complete"),
		ev(10, "unlabeled", "fabrik:reworking:Research"),
		ev(11, "unlabeled", "fabrik:editing"),
	}
	if err := checkReworkSequence(base, 5, "Research"); err != nil {
		t.Errorf("good sequence: %v", err)
	}

	// Pre-#1802: complete is never cleared, so there is no unlabeled event.
	preFix := []issueEvent{
		ev(5, "labeled", "stage:Research:complete"),
		ev(6, "labeled", "fabrik:editing"),
		ev(11, "unlabeled", "fabrik:editing"),
	}
	if err := checkReworkSequence(preFix, 5, "Research"); err == nil || !strings.Contains(err.Error(), "step 1") {
		t.Errorf("pre-fix log: want step-1 error, got %v", err)
	}

	cases := []struct {
		name   string
		events []issueEvent
		step   string
	}{
		{"complete never cleared", []issueEvent{ev(7, "labeled", "fabrik:reworking:Research"), ev(10, "unlabeled", "fabrik:reworking:Research")}, "step 2"},
		{"complete never restored", []issueEvent{ev(7, "labeled", "fabrik:reworking:Research"), ev(8, "unlabeled", "stage:Research:complete"), ev(10, "unlabeled", "fabrik:reworking:Research")}, "step 3"},
		{"marker never removed", []issueEvent{ev(7, "labeled", "fabrik:reworking:Research"), ev(8, "unlabeled", "stage:Research:complete"), ev(9, "labeled", "stage:Research:complete")}, "step 4"},
		{"complete cleared before marker", []issueEvent{ev(6, "unlabeled", "stage:Research:complete"), ev(7, "labeled", "fabrik:reworking:Research"), ev(9, "labeled", "stage:Research:complete"), ev(10, "unlabeled", "fabrik:reworking:Research")}, "step 2"},
		{"marker removed before restore", []issueEvent{ev(7, "labeled", "fabrik:reworking:Research"), ev(8, "unlabeled", "stage:Research:complete"), ev(9, "unlabeled", "fabrik:reworking:Research"), ev(10, "labeled", "stage:Research:complete")}, "step 4"},
	}
	for _, c := range cases {
		err := checkReworkSequence(c.events, 5, "Research")
		if err == nil || !strings.Contains(err.Error(), c.step) {
			t.Errorf("%s: want %s error, got %v", c.name, c.step, err)
		}
	}
}

func TestCheckReworkSequenceSinceIDBound(t *testing.T) {
	// A full sequence from an earlier re-entry must not satisfy a later one.
	events := []issueEvent{
		ev(7, "labeled", "fabrik:reworking:Research"),
		ev(8, "unlabeled", "stage:Research:complete"),
		ev(9, "labeled", "stage:Research:complete"),
		ev(10, "unlabeled", "fabrik:reworking:Research"),
	}
	if err := checkReworkSequence(events, 0, "Research"); err != nil {
		t.Errorf("sinceID 0: %v", err)
	}
	if err := checkReworkSequence(events, 10, "Research"); err == nil {
		t.Error("sinceID 10: want error, the sequence is entirely before the bound")
	}
	// A different stage's events are not this stage's sequence.
	if err := checkReworkSequence(events, 0, "Plan"); err == nil {
		t.Error("stage Plan: want error, events are for Research")
	}
}

// TestReworkingLabelPrefixLiteral pins the carried literal to the engine's
// (engine/comments.go reworkingLabelPrefix): the scenario must derive the marker
// name from the exact prefix, not paraphrase it.
func TestReworkingLabelPrefixLiteral(t *testing.T) {
	if reworkingLabelPrefix != "fabrik:reworking:" {
		t.Fatalf("reworkingLabelPrefix = %q, want %q (engine/comments.go)", reworkingLabelPrefix, "fabrik:reworking:")
	}
}
