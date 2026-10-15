package channelevents

import (
	"testing"
	"time"
)

func sitEvent(content string) Event {
	return Event{Type: Situation, Content: content, Meta: map[string]string{MetaTrigger: TriggerCadence}}
}

func TestSituationIsASubscribableAccountWideImmediateEvent(t *testing.T) {
	info, ok := Lookup(Situation)
	if !ok || !info.AccountWide || !info.Immediate || info.Synthetic || info.ShimGenerated {
		t.Fatalf("catalogue entry = %+v, ok=%v", info, ok)
	}
	found := false
	for _, e := range Subscribable() {
		found = found || e == Situation
	}
	if !found {
		t.Fatal("situation is not subscribable")
	}
}

func TestTakesSituation(t *testing.T) {
	cases := []struct {
		name string
		s    Subscription
		want bool
	}{
		{"semantic default", Subscription{Subscriber: "x", EventsDefault: true}, true},
		{"grandfathered empty", Subscription{Subscriber: "x"}, false},
		{"explicit list naming it", Subscription{Subscriber: "x", Events: []EventType{Merged, Situation}}, true},
		{"explicit list without it", Subscription{Subscriber: "x", Events: []EventType{Merged}}, false},
	}
	for _, tc := range cases {
		if got := tc.s.TakesSituation(); got != tc.want {
			t.Errorf("%s: TakesSituation=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestSituationCadenceValidation(t *testing.T) {
	ok := Subscription{Subscriber: "x", EventsDefault: true, SituationSeconds: 120}
	if err := ok.Validate(MinDigest); err != nil {
		t.Fatalf("valid cadence refused: %v", err)
	}
	for name, s := range map[string]Subscription{
		"below the floor":      {Subscriber: "x", EventsDefault: true, SituationSeconds: 59},
		"above the ceiling":    {Subscriber: "x", EventsDefault: true, SituationSeconds: int((MaxSituation + time.Second) / time.Second)},
		"negative":             {Subscriber: "x", EventsDefault: true, SituationSeconds: -5},
		"situation not taken":  {Subscriber: "x", Events: []EventType{Merged}, SituationSeconds: 120},
		"grandfathered filter": {Subscriber: "x", SituationSeconds: 120},
	} {
		if err := s.Validate(MinDigest); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
	if err := (Subscription{Subscriber: "x", EventsDefault: true, SituationSeconds: 2}).ValidateMin(MinDigest, time.Second); err != nil {
		t.Errorf("a shortened floor should admit 2s: %v", err)
	}
	if err := (Subscription{Subscriber: "x", Events: []EventType{Situation}}).Validate(MinDigest); err != nil {
		t.Errorf("situation must be a valid event name: %v", err)
	}
}

func TestSituationCadenceIsPartOfSubscriptionIdentity(t *testing.T) {
	a := Subscription{Subscriber: "x", EventsDefault: true}
	b := a
	b.SituationSeconds = 120
	if a.canonical() == b.canonical() {
		t.Fatal("a different cadence is a different subscription")
	}
}

func TestScopeMatchesIgnoresTheEventFilter(t *testing.T) {
	s := Subscription{Subscriber: "x", Events: []EventType{Situation}, Repos: []string{"o/a"}, Issues: []IssueRef{{Repo: "o/a", Number: 3}}}
	if !s.ScopeMatches(Event{Repo: "o/a", Issue: 3}) {
		t.Error("in-scope item refused")
	}
	if s.ScopeMatches(Event{Repo: "o/a", Issue: 4}) || s.ScopeMatches(Event{Repo: "o/b", Issue: 3}) {
		t.Error("out-of-scope item admitted")
	}
	m := Subscription{Subscriber: "x", Milestone: "v1"}
	if m.ScopeMatches(Event{Repo: "o/a", Issue: 1}) {
		t.Error("an item with an unknown milestone must not match a milestone scope")
	}
	if !m.ScopeMatches(Event{Repo: "o/a", Issue: 1, MilestoneKnown: true, MilestoneTitle: "v1"}) {
		t.Error("milestone match refused")
	}
}

func situationHub(t *testing.T, dir string) (*Hub, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)}
	h, err := Open(Options{Dir: dir, MinDigest: time.Millisecond, MinSituation: time.Millisecond, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, clk
}

func TestPublishSituationDedupsPerSubscriber(t *testing.T) {
	h, _ := situationHub(t, t.TempDir())
	for _, n := range []string{"a", "b"} {
		if _, err := h.Subscribe(Subscription{Subscriber: n, EventsDefault: true}); err != nil {
			t.Fatal(err)
		}
	}
	if !h.PublishSituation("a", sitEvent("one"), "fp1", false) {
		t.Fatal("first digest not sent")
	}
	if h.PublishSituation("a", sitEvent("one"), "fp1", false) {
		t.Fatal("an unchanged view was re-sent")
	}
	if !h.PublishSituation("b", sitEvent("one"), "fp1", false) {
		t.Fatal("another subscriber must still get the same view: memory is per subscriber")
	}
	if !h.PublishSituation("a", sitEvent("two"), "fp2", false) {
		t.Fatal("a changed view was not sent")
	}
	// A, then B, then A again must resend A: this is what Publish's global
	// DedupKey cannot express.
	if !h.PublishSituation("a", sitEvent("one again"), "fp1", false) {
		t.Fatal("returning to an earlier view must be sent again")
	}
	if !h.PublishSituation("a", sitEvent("forced"), "fp1", true) {
		t.Fatal("force must bypass the dedup")
	}
}

func TestPublishSituationHoldsAtMostOnePerSubscriber(t *testing.T) {
	h, _ := situationHub(t, t.TempDir())
	if _, err := h.Subscribe(Subscription{Subscriber: "away", EventsDefault: true}); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{"1", "2", "3", "4"} {
		h.PublishSituation("away", sitEvent("view "+fp), fp, false)
	}
	if got := h.Queued("away"); got != 1 {
		t.Fatalf("a detached subscriber holds %d situation digests, want 1 (superseded in place)", got)
	}
	s := newSink()
	h.Attach("away", s)
	if got := s.wait(t); got.Content != "view 4" {
		t.Fatalf("delivered %q, want the newest view", got.Content)
	}
}

func TestPublishSituationNeedsATakingSubscription(t *testing.T) {
	h, _ := situationHub(t, t.TempDir())
	if _, err := h.Subscribe(Subscription{Subscriber: "old"}); err != nil { // grandfathered
		t.Fatal(err)
	}
	if _, err := h.Subscribe(Subscription{Subscriber: "narrow", Events: []EventType{Merged}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"old", "narrow", "nobody"} {
		if h.PublishSituation(n, sitEvent("x"), "fp", false) {
			t.Errorf("%s does not take situation but was sent one", n)
		}
	}
}

func TestSituationFingerprintSurvivesAReloadAndUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	h, _ := situationHub(t, dir)
	if _, err := h.Subscribe(Subscription{Subscriber: "X", EventsDefault: true}); err != nil {
		t.Fatal(err)
	}
	if !h.PublishSituation("X", sitEvent("v"), "fp", false) {
		t.Fatal("not sent")
	}
	h.Close()

	h2, _ := situationHub(t, dir)
	if got := h2.SituationFingerprint("X"); got != "fp" {
		t.Fatalf("fingerprint after reload = %q", got)
	}
	if h2.PublishSituation("X", sitEvent("v"), "fp", false) {
		t.Fatal("a restart must not re-send the digest the previous run already delivered")
	}
	h2.Unsubscribe("X", "")
	if got := h2.SituationFingerprint("X"); got != "" {
		t.Fatalf("fingerprint kept after the last unsubscribe: %q", got)
	}
}

func TestCatchUpWhereFiltersSubscriptions(t *testing.T) {
	h, _ := situationHub(t, t.TempDir())
	taking, err := h.Subscribe(Subscription{Subscriber: "X", EventsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe(Subscription{Subscriber: "X", Events: []EventType{Merged}}); err != nil {
		t.Fatal(err)
	}
	evs := []Event{{Type: Merged, Repo: "o/r", Issue: 1, Content: "m"}}
	if n := h.CatchUpWhere("X", "", evs, func(s Subscription) bool { return !s.TakesSituation() }); n != 1 {
		t.Fatalf("queued %d, want 1 (the non-taking subscription matches merged)", n)
	}
	h2, _ := situationHub(t, t.TempDir())
	if _, err := h2.Subscribe(Subscription{Subscriber: "X", EventsDefault: true}); err != nil {
		t.Fatal(err)
	}
	if n := h2.CatchUpWhere("X", taking.ID, evs, func(s Subscription) bool { return !s.TakesSituation() }); n != 0 {
		t.Fatalf("queued %d for a taking subscription, want 0", n)
	}
}
