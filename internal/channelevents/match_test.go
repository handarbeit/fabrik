package channelevents

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"fabrik:*", "fabrik:paused", true},
		{"fabrik:*", "stage:Plan:complete", false},
		{"stage:*:complete", "stage:Validate:complete", true},
		{"stage:*:complete", "stage:Validate:in_progress", false},
		{"fabrik:locked:*", "fabrik:locked:alice", true},
		{"*", "anything:at/all", true},
		{"fabrik:editing", "fabrik:editing", true},
		{"fabrik:editing", "fabrik:editing2", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pat, c.name); got != c.want {
			t.Errorf("GlobMatch(%q,%q)=%v want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestSubscriptionDefaultLabelExclusion(t *testing.T) {
	s := Subscription{Subscriber: "x"}
	for _, l := range []string{"fabrik:locked:bob", "stage:Implement:in_progress", "fabrik:editing", "fabrik:credited-pr:3", "fabrik:spawned-child:1:5"} {
		if s.Matches(Event{Type: LabelApplied, Label: l}) {
			t.Errorf("default filter should suppress %q", l)
		}
	}
	if !s.Matches(Event{Type: LabelApplied, Label: "fabrik:paused"}) {
		t.Error("fabrik:paused should pass the default filter")
	}
	if !s.Matches(Event{Type: LabelRemoved, Label: "stage:Validate:complete"}) {
		t.Error("stage:*:complete should pass")
	}

	none := []string{}
	all := Subscription{Subscriber: "x", ExcludeLabels: &none}
	if !all.Matches(Event{Type: LabelApplied, Label: "fabrik:locked:bob"}) {
		t.Error("explicit empty exclude list opts into everything")
	}
}

func TestSubscriptionLabelIncludeAppliesOnlyToLabelEvents(t *testing.T) {
	s := Subscription{Subscriber: "x", Labels: []string{"fabrik:paused"}}
	if s.Matches(Event{Type: LabelApplied, Label: "fabrik:blocked"}) {
		t.Error("include list should reject non-matching label")
	}
	if !s.Matches(Event{Type: ValidateSettled}) {
		t.Error("label patterns must not filter non-label events")
	}
}

func TestSubscriptionScopes(t *testing.T) {
	ev := Event{Type: Paused, Repo: "o/r", Issue: 5, MilestoneKnown: true, MilestoneTitle: "v1", MilestoneNumber: 3}
	cases := []struct {
		name string
		sub  Subscription
		ev   Event
		want bool
	}{
		{"empty matches all", Subscription{}, ev, true},
		{"repo hit", Subscription{Repos: []string{"o/r"}}, ev, true},
		{"repo miss", Subscription{Repos: []string{"o/x"}}, ev, false},
		{"issue hit", Subscription{Issues: []IssueRef{{Repo: "o/r", Number: 5}}}, ev, true},
		{"issue wrong repo", Subscription{Issues: []IssueRef{{Repo: "o/x", Number: 5}}}, ev, false},
		{"issue any repo", Subscription{Issues: []IssueRef{{Number: 5}}}, ev, true},
		{"milestone title", Subscription{Milestone: "v1"}, ev, true},
		{"milestone number", Subscription{Milestone: "#3"}, ev, true},
		{"milestone miss", Subscription{Milestone: "v2"}, ev, false},
		{"milestone unknown never matches", Subscription{Milestone: "v1"}, Event{Type: Paused, Repo: "o/r", Issue: 5}, false},
		{"event filter miss", Subscription{Events: []EventType{Merged}}, ev, false},
		{"event filter hit", Subscription{Events: []EventType{Paused, Merged}}, ev, true},
		{"account-wide ignores scope", Subscription{Repos: []string{"o/x"}, Issues: []IssueRef{{Number: 1}}, Milestone: "v9"}, Event{Type: ClaudeUsageLimited}, true},
		{"account-wide still honours event filter", Subscription{Events: []EventType{Merged}}, Event{Type: ClaudeUsageLimited}, false},
	}
	for _, c := range cases {
		if got := c.sub.Matches(c.ev); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestSubscriptionValidate(t *testing.T) {
	bad := []Subscription{
		{},
		{Subscriber: "x", Events: []EventType{"nope"}},
		{Subscriber: "x", Events: []EventType{EventsDropped}},
		{Subscriber: "x", DigestSeconds: 1},
		{Subscriber: "x", DigestSeconds: 7200},
		{Subscriber: "x", Labels: []string{""}},
		{Subscriber: "x", Issues: []IssueRef{{Number: 0}}},
		{Subscriber: "bad\nname"},
	}
	for i, s := range bad {
		if s.Validate(MinDigest) == nil {
			t.Errorf("case %d should be invalid: %+v", i, s)
		}
	}
	if err := (Subscription{Subscriber: "ok", DigestSeconds: 60, Events: Subscribable()}).Validate(MinDigest); err != nil {
		t.Errorf("valid subscription rejected: %v", err)
	}
}

func TestCatalogImmediateSet(t *testing.T) {
	for _, e := range []EventType{ValidateSettled, Escalated, Paused, DaemonUnreachable, PipelineStalled, PipelineResumed} {
		if !IsImmediate(e) {
			t.Errorf("%s must be immediate", e)
		}
	}
	for _, e := range []EventType{LabelApplied, Stalled, Merged} {
		if IsImmediate(e) {
			t.Errorf("%s must not be immediate", e)
		}
	}
}

func TestPipelineEventsAreAccountWideAndSubscribable(t *testing.T) {
	narrow := Subscription{Subscriber: "x", Repos: []string{"o/x"}, Issues: []IssueRef{{Number: 1}}, Milestone: "v9"}
	for _, e := range []EventType{PipelineStalled, PipelineResumed} {
		if !IsAccountWide(e) {
			t.Errorf("%s must be account-wide", e)
		}
		if !narrow.Matches(Event{Type: e, Repo: "o/other"}) {
			t.Errorf("%s must reach a narrowly scoped subscriber", e)
		}
		found := false
		for _, s := range Subscribable() {
			found = found || s == e
		}
		if !found {
			t.Errorf("%s must be subscribable", e)
		}
		if err := (Subscription{Subscriber: "x", Events: []EventType{e}}).Validate(MinDigest); err != nil {
			t.Errorf("events filter rejects %s: %v", e, err)
		}
	}
}

func TestItemAddedMatchesArrivalLabels(t *testing.T) {
	ev := Event{Type: ItemAdded, Repo: "o/r", Issue: 4, Labels: []string{"fabrik:cruise", "fabrik:editing"}}
	if !(Subscription{Subscriber: "x"}).Matches(ev) {
		t.Error("an unscoped subscription takes item-added")
	}
	if !(Subscription{Subscriber: "x", Labels: []string{"fabrik:cruise"}}).Matches(ev) {
		t.Error("a label scope must match an arrival label")
	}
	if (Subscription{Subscriber: "x", Labels: []string{"fabrik:yolo"}}).Matches(ev) {
		t.Error("a label scope with no matching arrival label must not match")
	}
	// The default exclude list is not applied: arriving with fabrik:editing is
	// still an arrival.
	if !(Subscription{Subscriber: "x", Labels: []string{"fabrik:editing"}}).Matches(ev) {
		t.Error("exclude list must not hide an arrival")
	}
	if (Subscription{Subscriber: "x", Repos: []string{"o/other"}}).Matches(ev) {
		t.Error("repo scope must apply to item-added")
	}
	if (Subscription{Subscriber: "x", Milestone: "v1"}).Matches(ev) {
		t.Error("unknown milestone must not match a milestone scope")
	}
}

func TestProgressViaColumn(t *testing.T) {
	stage := []Event{
		{Type: LabelApplied, Label: "stage:Plan:complete"},
		{Type: LabelRemoved, Label: "stage:Plan:in_progress"},
	}
	none := []string{}
	on := Subscription{Subscriber: "x", ProgressViaColumn: true, ExcludeLabels: &none, Labels: []string{"stage:*"}}
	for _, ev := range stage {
		if on.Matches(ev) {
			t.Errorf("progress_via_column should drop %q even past labels/exclude_labels", ev.Label)
		}
	}
	if !on.Matches(Event{Type: ColumnChanged}) {
		t.Error("column-changed must still be delivered")
	}
	other := Subscription{Subscriber: "x", ProgressViaColumn: true}
	if !other.Matches(Event{Type: LabelApplied, Label: "fabrik:paused"}) {
		t.Error("other labels must still be delivered")
	}
	off := Subscription{Subscriber: "x"}
	if !off.Matches(stage[0]) {
		t.Error("without the option delivery is unchanged")
	}
	// Events list that omits column-changed: Validate rejects the combination.
	bad := Subscription{Subscriber: "x", ProgressViaColumn: true, Events: []EventType{LabelApplied}}
	if err := bad.Validate(MinDigest); err == nil {
		t.Error("progress_via_column without column-changed must be rejected")
	}
	ok := Subscription{Subscriber: "x", ProgressViaColumn: true, Events: []EventType{LabelApplied, ColumnChanged}}
	if err := ok.Validate(MinDigest); err != nil {
		t.Errorf("valid combination rejected: %v", err)
	}
}

func TestProgressViaColumnPersistedCompat(t *testing.T) {
	// An old persisted subscription (no field) loads with the option off, and the
	// canonical form of an option-off subscription omits the key.
	var s Subscription
	if err := json.Unmarshal([]byte(`{"id":"a","subscriber":"x"}`), &s); err != nil || s.ProgressViaColumn {
		t.Fatalf("old subscription: %v %+v", err, s)
	}
	if strings.Contains(s.canonical(), "progress_via_column") {
		t.Error("option-off canonical form must omit the key")
	}
}

func TestAnomalyEventsAreItemScopedAndSubscribable(t *testing.T) {
	immediate := []EventType{RetryStarved, RetryResumed, ItemWedged, ItemUnwedged}
	for _, e := range append(append([]EventType{}, immediate...), CacheDivergence) {
		if IsAccountWide(e) {
			t.Errorf("%s must be item-scoped, not account-wide", e)
		}
		if IsImmediate(e) != (e != CacheDivergence) {
			t.Errorf("%s immediacy = %v", e, IsImmediate(e))
		}
		found := false
		for _, s := range Subscribable() {
			found = found || s == e
		}
		if !found {
			t.Errorf("%s must be subscribable", e)
		}
		if err := (Subscription{Subscriber: "x", Events: []EventType{e}}).Validate(0); err != nil {
			t.Errorf("%s must validate: %v", e, err)
		}
		other := Subscription{Subscriber: "x", Repos: []string{"o/x"}}
		if other.Matches(Event{Type: e, Repo: "o/other", Issue: 1}) {
			t.Errorf("%s must respect the repo scope", e)
		}
	}
}
