package channelevents

import "testing"

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
		{"account-wide ignores scope", Subscription{Repos: []string{"o/x"}, Issues: []IssueRef{{Number: 1}}, Milestone: "v9"}, Event{Type: ClaudeLimitSuspended}, true},
		{"account-wide still honours event filter", Subscription{Events: []EventType{Merged}}, Event{Type: ClaudeLimitSuspended}, false},
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
	for _, e := range []EventType{ValidateSettled, Escalated, Paused, DaemonUnreachable} {
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
