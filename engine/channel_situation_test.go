package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// situationDigests collects the situation events delivered within window.
func situationDigests(sink *chanSink, window time.Duration) []channelevents.Event {
	return ofType(drain(sink, window), channelevents.Situation)
}

// cadenceEngine is situationEngine with subscriber "T" on a 60 s cadence.
func cadenceEngine(t *testing.T) (*Engine, *chanSink, *adjClock, *channelEvents) {
	t.Helper()
	e := apiEngine(t, 0)
	e.store = itemstate.NewStore(&countingFetcher{})
	e, sink := channelEngineOn(t, e, 0, &channelevents.Subscription{EventsDefault: true, SituationSeconds: 60})
	return e, sink, e.clock.(*adjClock), e.channelEvents()
}

// ---- fingerprint ----

func baseSituationView() localapi.SituationView {
	return localapi.SituationView{
		HumanInput: localapi.SituationSection[localapi.SituationEntry]{Entries: []localapi.SituationEntry{{
			Issue: "o/r#1", Title: "t", State: "needs-human", Code: "fabrik:paused", Rank: 0,
			Human: &localapi.SituationHuman{ResumeVia: "human-comment"},
		}}},
		Gates: localapi.SituationSection[localapi.SituationEntry]{Entries: []localapi.SituationEntry{{
			Issue: "o/r#2", State: "waiting", Code: "fabrik:awaiting-ci",
			Gate: &localapi.SituationGate{Gate: "ci", Reason: "waiting for CI checks", HeldSince: localapi.Unknown[time.Time](), HeldSeconds: localapi.Some[int64](60)},
		}}},
		MergeTrain: localapi.SituationSection[localapi.TrainPartition]{Entries: []localapi.TrainPartition{{
			Repo: "o/r", Base: "", Members: []string{"o/r#9"}, Phase: "trial-ci", OldestQueuedSeconds: localapi.Some[int64](30),
		}}},
		Health: localapi.SituationHealth{Workers: localapi.WorkerSlots{InUse: 1, MaxConcurrent: localapi.Some(3)}, TrainQueued: 1},
	}
}

func TestSituationFingerprint_IgnoresWhatMovesWithTheClock(t *testing.T) {
	base := situationFingerprint(baseSituationView())
	if base != situationFingerprint(baseSituationView()) {
		t.Fatal("fingerprint is not deterministic")
	}
	volatile := map[string]func(*localapi.SituationView){
		"gate hold age": func(v *localapi.SituationView) { v.Gates.Entries[0].Gate.HeldSeconds = localapi.Some[int64](9999) },
		"partition queue age": func(v *localapi.SituationView) {
			v.MergeTrain.Entries[0].OldestQueuedSeconds = localapi.Some[int64](9999)
		},
		"poll in-progress time": func(v *localapi.SituationView) { v.Health.Poll.InProgressSeconds = localapi.Some[int64](500) },
		"poll running now":      func(v *localapi.SituationView) { v.Health.Poll.InProgress = true },
		"poll last attempt":     func(v *localapi.SituationView) { v.Health.Poll.LastAttemptAt = localapi.KnownTime(time.Now()) },
		"entry reason prose":    func(v *localapi.SituationView) { v.HumanInput.Entries[0].Reason = "paused 5m ago" },
		"entry order":           func(v *localapi.SituationView) {},
	}
	for name, mut := range volatile {
		v := baseSituationView()
		mut(&v)
		if got := situationFingerprint(v); got != base {
			t.Errorf("%s changed the fingerprint: a clock-only change must not re-send a digest", name)
		}
	}
	substantive := map[string]func(*localapi.SituationView){
		"new item": func(v *localapi.SituationView) {
			v.Decisions.Entries = append(v.Decisions.Entries, localapi.SituationEntry{Issue: "o/r#3"})
		},
		"item code":     func(v *localapi.SituationView) { v.HumanInput.Entries[0].Code = "fabrik:awaiting-input" },
		"resume method": func(v *localapi.SituationView) { v.HumanInput.Entries[0].Human.ResumeVia = "remove-label" },
		"gate name":     func(v *localapi.SituationView) { v.Gates.Entries[0].Gate.Gate = "review" },
		"gate start": func(v *localapi.SituationView) {
			v.Gates.Entries[0].Gate.HeldSince = localapi.KnownTime(time.Unix(1, 0))
		},
		"omitted count":   func(v *localapi.SituationView) { v.Gates.Omitted = 2 },
		"partition phase": func(v *localapi.SituationView) { v.MergeTrain.Entries[0].Phase = "bisect" },
		"partition idle":  func(v *localapi.SituationView) { v.MergeTrain.Entries[0].IdleReason = "poll-overdue" },
		"worker slots":    func(v *localapi.SituationView) { v.Health.Workers.InUse = 2 },
		"train queued":    func(v *localapi.SituationView) { v.Health.TrainQueued = 4 },
		"poll overdue":    func(v *localapi.SituationView) { v.Health.Poll.Overdue = localapi.Some(true) },
		"anomaly": func(v *localapi.SituationView) {
			v.Anomalies.Entries = append(v.Anomalies.Entries, localapi.SituationAnomaly{Kind: "item-wedged", Subject: "o/r#1"})
		},
		"unclassified":  func(v *localapi.SituationView) { v.UnclassifiedItems = 1 },
		"nothing needs": func(v *localapi.SituationView) { v.NothingNeedsYou = true },
		"pending blocker": func(v *localapi.SituationView) {
			v.Decisions.Entries = []localapi.SituationEntry{{Issue: "o/r#4", Pending: &localapi.PendingInfo{Kind: "parked-blocker", Blockers: []localapi.PendingBlocker{{Issue: "o/r#5", Column: "Backlog"}}}}}
		},
	}
	for name, mut := range substantive {
		v := baseSituationView()
		mut(&v)
		if got := situationFingerprint(v); got == base {
			t.Errorf("%s did not change the fingerprint: a real change would be hidden", name)
		}
	}
}

func TestRenderSituation_IsBoundedAndCarriesCountsAndTrigger(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	for n := 1; n <= 25; n++ {
		e.seedAPIItem(t, n, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	}
	v := e.buildSituation(nil)
	ev := renderSituation(v, e.now(), channelevents.TriggerCadence)
	// 25 items are cut to the section bound before rendering: one line per kept
	// entry, not one per item.
	if lines := strings.Count(ev.Content, "owner/repo#"); lines != situationSectionMax {
		t.Errorf("rendered %d item lines, want the section bound %d\n%s", lines, situationSectionMax, ev.Content)
	}
	if ev.Meta["human_input"] != "25" || ev.Meta["omitted"] != "15" || ev.Meta[channelevents.MetaTrigger] != "cadence" || ev.Meta[channelevents.MetaSource] != "live" {
		t.Errorf("meta = %v", ev.Meta)
	}
	catch := renderSituation(v, e.now(), channelevents.TriggerCatchUp)
	if catch.Content != ev.Content || catch.Meta["catch_up"] != "true" || catch.Meta[channelevents.MetaSource] != "catch_up" {
		t.Errorf("catch-up differs from the digest in more than its trigger tags: %q vs %q / %v", catch.Content, ev.Content, catch.Meta)
	}
}

// ---- cadence and dedup ----

func TestSituationCadence_TwoQuietTicksPublishOneDigestAChangePublishesASecond(t *testing.T) {
	e, sink, clk, ce := cadenceEngine(t)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	drain(sink, 100*time.Millisecond) // the item's own pause event

	var got []channelevents.Event
	tick := func(advance time.Duration) {
		clk.set(clk.Now().Add(advance))
		ce.publishSituations()
		got = append(got, situationDigests(sink, 120*time.Millisecond)...)
	}
	tick(0)                // first look: send
	tick(61 * time.Second) // nothing changed
	tick(61 * time.Second) // still nothing changed
	if len(got) != 1 {
		t.Fatalf("two quiet cadence ticks published %d digests, want 1", len(got))
	}
	if got[0].Meta[channelevents.MetaTrigger] != "cadence" || !strings.Contains(got[0].Content, "owner/repo#1") {
		t.Errorf("digest = %+v", got[0])
	}

	// A change between ticks: a second digest, with the new item in it.
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	tick(61 * time.Second)
	if len(got) != 2 || !strings.Contains(got[1].Content, "owner/repo#2") {
		t.Fatalf("a change between ticks must publish a second digest: %d digests", len(got))
	}
	tick(61 * time.Second)
	if len(got) != 2 {
		t.Fatalf("an unchanged view after the second digest was sent again: %d", len(got))
	}
}

// The same scenario against a builder that ignores the dedup must fail the
// one-digest assertion — so that assertion is not vacuous.
func TestSituationCadence_NonVacuous_NoDedupSendsEveryTick(t *testing.T) {
	e, sink, clk, ce := cadenceEngine(t)
	restore := e.SetSituationDedupDisabledForTest()
	defer restore()
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	drain(sink, 100*time.Millisecond)
	n := 0
	for i := 0; i < 3; i++ {
		clk.set(clk.Now().Add(61 * time.Second))
		ce.publishSituations()
		n += len(situationDigests(sink, 120*time.Millisecond))
	}
	if n != 3 {
		t.Fatalf("with the dedup ignored every tick sends: got %d digests, want 3", n)
	}
}

func TestSituationCadence_OffByDefaultAndNotBeforeTheInterval(t *testing.T) {
	// No cadence: nothing is pushed however much time passes.
	e, sink, clk, ce, _ := situationEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	drain(sink, 100*time.Millisecond)
	for i := 0; i < 3; i++ {
		clk.set(clk.Now().Add(time.Hour))
		ce.publishSituations()
	}
	if got := situationDigests(sink, 150*time.Millisecond); len(got) != 0 {
		t.Fatalf("a subscription with no cadence was pushed %d digests", len(got))
	}

	// A cadence: a change inside the interval waits for the interval.
	e2, sink2, clk2, ce2 := cadenceEngine(t)
	e2.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	drain(sink2, 100*time.Millisecond)
	ce2.publishSituations()
	if got := situationDigests(sink2, 120*time.Millisecond); len(got) != 1 {
		t.Fatalf("first evaluation: %d digests", len(got))
	}
	e2.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	clk2.set(clk2.Now().Add(30 * time.Second))
	ce2.publishSituations()
	if got := situationDigests(sink2, 120*time.Millisecond); len(got) != 0 {
		t.Fatalf("evaluated before the cadence elapsed: %d digests", len(got))
	}
}

// ---- request ----

func TestSituationRequest_DedupedLikeAnyDigestUnlessForced(t *testing.T) {
	e, sink, _, _, _ := situationEngine(t, 0)
	e.seedSettled(t, 3)
	drain(sink, 100*time.Millisecond)
	b := e.LocalAPIBackend().(localAPIBackend)

	res, err := b.Situation(localapi.SituationParams{Subscriber: "T", Push: true})
	if err != nil || !res.Pushed || res.PushNote != "" {
		t.Fatalf("first request: %+v %v", res, err)
	}
	got := situationDigests(sink, 150*time.Millisecond)
	if len(got) != 1 || got[0].Meta[channelevents.MetaTrigger] != "request" {
		t.Fatalf("delivered %+v", got)
	}
	// The pushed content is the tool's own rendering of the same view.
	if want := renderSituation(res.View, e.now(), channelevents.TriggerRequest).Content; got[0].Content != want {
		t.Errorf("pushed content differs from the tool's view:\n%s\n---\n%s", got[0].Content, want)
	}

	res, _ = b.Situation(localapi.SituationParams{Subscriber: "T", Push: true})
	if res.Pushed || !strings.Contains(res.PushNote, "unchanged") {
		t.Fatalf("an unchanged view was pushed again: %+v", res)
	}
	if got := situationDigests(sink, 150*time.Millisecond); len(got) != 0 {
		t.Fatalf("unexpected digest %+v", got)
	}
	res, _ = b.Situation(localapi.SituationParams{Subscriber: "T", Push: true, Force: true})
	if !res.Pushed {
		t.Fatalf("force must send: %+v", res)
	}
	if got := situationDigests(sink, 150*time.Millisecond); len(got) != 1 {
		t.Fatalf("forced digest not delivered: %+v", got)
	}
}

// ---- catch-up is the digest ----

func TestSituationCatchUp_ContentEqualsTheDigestAndTheTool(t *testing.T) {
	e, _, _, ce, _ := situationEngine(t, 0)
	e.seedSettled(t, 3)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.seedSpecifyComplete(t, 6, 24*time.Hour)

	st := e.channelStreamer()
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "late"}); err != nil {
		t.Fatal(err)
	}
	collect := func(catchUp bool) []channelevents.Event {
		sink := newChanSink()
		detach, err := st.Attach(localapi.AttachParams{Subscriber: "late", CatchUp: catchUp}, sink)
		if err != nil {
			t.Fatal(err)
		}
		defer detach()
		// Live events about these items (a decision-pending announced as the
		// debounce settles) may interleave; the catch-up is the situation digest.
		return ofType(drain(sink, 300*time.Millisecond), channelevents.Situation)
	}

	first := collect(false) // the subscribe-time catch-up, held until attach
	if len(first) != 1 || first[0].Type != channelevents.Situation {
		t.Fatalf("a default subscription is caught up with exactly one situation digest, got %+v", first)
	}
	if first[0].Meta[channelevents.MetaTrigger] != "catch_up" || first[0].Meta["catch_up"] != "true" || first[0].Meta[channelevents.MetaSource] != "catch_up" {
		t.Errorf("catch-up tags = %v", first[0].Meta)
	}

	tool, _ := e.LocalAPIBackend().(localAPIBackend).Situation(localapi.SituationParams{})
	requested := renderSituation(tool.View, e.now(), channelevents.TriggerRequest)
	if first[0].Content != requested.Content {
		t.Fatalf("catch-up content differs from the digest/tool content:\n%s\n---\n%s", first[0].Content, requested.Content)
	}
	for _, k := range []string{"merge_decisions", "human_input", "decisions", "gates", "anomalies", "nothing_needs_you"} {
		if first[0].Meta[k] != requested.Meta[k] {
			t.Errorf("meta %s: catch-up %q vs digest %q", k, first[0].Meta[k], requested.Meta[k])
		}
	}

	if again := collect(false); len(again) != 0 { // a reconnect: nothing new
		t.Fatalf("a reconnect must not repeat the catch-up: %+v", again)
	}
	fresh := collect(true) // a new session under the same name
	if len(fresh) != 1 || fresh[0].Content != first[0].Content {
		t.Fatalf("a fresh session wants the same digest again: %+v", fresh)
	}

	// The catch-up recorded what it sent, so the cadence does not repeat it.
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "late", SituationSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	detach, _ := st.Attach(localapi.AttachParams{Subscriber: "late"}, sink)
	defer detach()
	drain(sink, 200*time.Millisecond) // the new subscription's own catch-up
	ce.publishSituations()
	if got := situationDigests(sink, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("the first scheduled digest repeated the catch-up it followed: %+v", got)
	}
}

func TestSituationCatchUp_SubscriptionsThatDoNotTakeItKeepThePerItemSnapshot(t *testing.T) {
	e, _, _, ce, _ := situationEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	// A subscription persisted before the digest existed: empty events, no
	// semantic-default marker.
	if _, err := ce.hub.Subscribe(channelevents.Subscription{Subscriber: "old"}); err != nil {
		t.Fatal(err)
	}
	if n := e.catchUp("old", ""); n == 0 {
		t.Fatal("a grandfathered subscription still gets its per-item catch-up")
	}
	sink := newChanSink()
	detach := ce.hub.Attach("old", sink)
	defer detach()
	evs := drain(sink, 300*time.Millisecond)
	if len(ofType(evs, channelevents.Situation)) != 0 {
		t.Errorf("a grandfathered subscription must not receive the digest: %+v", evs)
	}
	if len(ofType(evs, channelevents.AwaitingInput)) != 1 {
		t.Errorf("per-item catch-up = %+v", evs)
	}
}

func TestSituationCatchUp_AnEmptySystemStillSaysNothingNeedsYou(t *testing.T) {
	e, _, _, _, _ := situationEngine(t, 0)
	st := e.channelStreamer()
	if _, err := st.Subscribe(localapi.SubscribeParams{Subscriber: "late"}); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	detach, _ := st.Attach(localapi.AttachParams{Subscriber: "late"}, sink)
	defer detach()
	got := situationDigests(sink, 300*time.Millisecond)
	if len(got) != 1 || !strings.Contains(got[0].Content, "nothing needs you") {
		t.Fatalf("catch-up of an empty system = %+v", got)
	}
}

func TestSituationDigest_ScopedToTheSubscribersSubscriptions(t *testing.T) {
	e, _, _, ce, _ := situationEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.IssueOpened{Item: ghItemOther()})
	if _, err := ce.hub.Subscribe(channelevents.Subscription{Subscriber: "scoped", EventsDefault: true, Repos: []string{"other/repo"}}); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	detach := ce.hub.Attach("scoped", sink)
	defer detach()
	e.catchUp("scoped", "")
	got := situationDigests(sink, 300*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("digests = %+v", got)
	}
	if strings.Contains(got[0].Content, "owner/repo#1") || !strings.Contains(got[0].Content, "other/repo#9") {
		t.Errorf("a repo-scoped subscription sees only its repo's items:\n%s", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "Health:") {
		t.Errorf("account sections are never narrowed:\n%s", got[0].Content)
	}
}

func ghItemOther() gh.ProjectItem {
	return gh.ProjectItem{ID: "I_O", Number: 9, Repo: "other/repo", Title: "o", Status: "Implement", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}
}
