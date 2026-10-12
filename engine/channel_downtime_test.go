package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// writeLedger leaves the state a previous daemon run would have persisted.
func writeLedger(t *testing.T, dir string, recs map[string]channelevents.Observation) {
	t.Helper()
	l := channelevents.OpenLedger(dir, nil, nil)
	for k, o := range recs {
		l.Observe(k, o)
	}
	l.Close()
}

// downtimeEngine starts an engine on dir (warming per the argument) with the
// subscriber "T" attached to everything.
func downtimeEngine(t *testing.T, dir string, warming bool) (*Engine, *chanSink) {
	t.Helper()
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = 20*time.Millisecond, time.Hour
	t.Cleanup(func() { channelDebounce, channelTick = oldD, oldT })
	e := apiEngine(t, 0)
	e.fabrikDir = t.TempDir()
	e.startChannelEventsIn(dir, warming)
	t.Cleanup(e.closeChannelEvents)
	ce := e.channelEvents()
	if _, err := ce.hub.Subscribe(channelevents.Subscription{Subscriber: "T"}); err != nil {
		t.Fatal(err)
	}
	sink := newChanSink()
	ce.hub.Attach("T", sink)
	return e, sink
}

func drainFor(sink *chanSink, d time.Duration) []channelevents.Event {
	var out []channelevents.Event
	deadline := time.After(d)
	for {
		select {
		case ev := <-sink.ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}

func TestDowntimeCheckDiffsLabelsColumnAndClosed(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#1": {Labels: []string{"keep", "gone", "fabrik:locked:me"}, LabelsKnown: true, Status: "Implement"},
		"owner/repo#2": {Labels: []string{"same"}, LabelsKnown: true, Status: "Implement"},
		"owner/repo#3": {Labels: []string{"x"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, false)
	e.seedAPIItem(t, 1, "Review", "keep", "new")              // moved, one label out, one in
	e.seedAPIItem(t, 2, "Implement", "same")                  // unchanged
	e.seedAPIItem(t, 4, "Implement", "brand", "new2")         // no prior record: a baseline
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ // closed while down
		ID: "I_C", Number: 3, Repo: "owner/repo", Status: "Implement", Labels: []string{"x"}, IsClosed: true,
	}})

	evs := drainFor(sink, 400*time.Millisecond)
	got := map[string]channelevents.Event{}
	for _, ev := range evs {
		if ev.Meta["source"] != channelevents.SourceDowntimeDelta {
			t.Errorf("only downtime deltas expected, got %+v", ev)
		}
		if ev.Meta[channelevents.MetaUnverified] != "true" || ev.Meta["actor"] != "unknown" {
			t.Errorf("a delta is unverified with actor unknown: %+v", ev.Meta)
		}
		switch ev.Type {
		case channelevents.LabelApplied, channelevents.LabelRemoved:
			got[string(ev.Type)+":"+ev.Label] = ev
		default:
			got[string(ev.Type)+":"+ev.Meta["issue"]] = ev
		}
	}
	for _, want := range []string{"label-removed:gone", "label-applied:new", "column-changed:1", "closed:3"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s in %v", want, keys(got))
		}
	}
	if len(evs) != 4 {
		t.Errorf("want exactly 4 deltas (churn labels, unchanged and unrecorded items stay quiet), got %d: %+v", len(evs), evs)
	}
	if ev := got["column-changed:1"]; ev.Meta["from"] != "Implement" || ev.Meta["to"] != "Review" {
		t.Errorf("column delta: %+v", ev.Meta)
	}
}

func keys(m map[string]channelevents.Event) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDowntimeCheckCorruptLedgerIsAllBaselines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/items.json", "{corrupt")
	e, sink := downtimeEngine(t, dir, false)
	e.seedAPIItem(t, 1, "Review", "a")
	sink.quiet(t, 300*time.Millisecond)
}

func TestDowntimeDeltasDisabledIsPreChangeBehaviour(t *testing.T) {
	defer SetDowntimeDeltasDisabledForTest()()
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{"owner/repo#1": {Labels: []string{"a"}, LabelsKnown: true, Status: "Implement"}})
	e, sink := downtimeEngine(t, dir, false)
	e.seedAPIItem(t, 1, "Review")
	sink.quiet(t, 300*time.Millisecond)
}

// Warming: the deltas wait for the readiness pass, which queues the catch-up
// first, so the order is catch-up, then deltas.
func TestRestartWarmingQueuesCatchUpBeforeDeltas(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#1": {Labels: []string{"a"}, LabelsKnown: true, Status: "Implement"},
		"owner/repo#2": {Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, true)
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input") // needs a human: in the snapshot
	e.seedAPIItem(t, 1, "Implement")                                           // label "a" removed while down
	sink.quiet(t, 300*time.Millisecond)                                        // board not populated: nothing yet

	e.boardPopulated.Store(true)
	e.channelEvents().signal()
	var evs []channelevents.Event
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(evs) < 2 {
		evs = append(evs, drainFor(sink, 100*time.Millisecond)...)
	}
	if len(evs) != 2 {
		t.Fatalf("want catch-up then one delta, got %+v", evs)
	}
	if evs[0].Meta["source"] != channelevents.SourceCatchUp || evs[0].Issue != 2 {
		t.Errorf("first must be the catch-up snapshot: %+v", evs[0])
	}
	if evs[1].Meta["source"] != channelevents.SourceDowntimeDelta || evs[1].Type != channelevents.LabelRemoved || evs[1].Label != "a" {
		t.Errorf("second must be the downtime delta: %+v", evs[1])
	}
}

// An open item that is never deep-fetched must not hold the catch-up for ever.
func TestRestartReadinessDeadline(t *testing.T) {
	defer SetChannelReadyTimeoutForTest(150 * time.Millisecond)()
	dir := t.TempDir()
	e, sink := downtimeEngine(t, dir, true)
	e.boardPopulated.Store(true)
	// An open Implement item whose labels never become known.
	e.store.Reset([]gh.ProjectItem{{Number: 1, Repo: "owner/repo", Status: "Implement", LabelsAbsent: true}})
	ce := e.channelEvents()
	if ce.readyToCatchUp() {
		t.Fatal("an unlabelled open item in a managed stage must hold readiness")
	}
	// a paused item arrives labelled: it is in the snapshot once the deadline passes
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	var evs []channelevents.Event
	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !found {
		ce.signal()
		for _, ev := range drainFor(sink, 100*time.Millisecond) {
			evs = append(evs, ev)
			found = found || ev.Meta["source"] == channelevents.SourceCatchUp
		}
	}
	if !found {
		t.Fatalf("the deadline must release the catch-up: %+v", evs)
	}
}

func TestReadinessIgnoresParkingColumns(t *testing.T) {
	e, _ := downtimeEngine(t, t.TempDir(), true)
	e.boardPopulated.Store(true)
	e.store.Reset([]gh.ProjectItem{{Number: 1, Repo: "owner/repo", Status: "Backlog", LabelsAbsent: true}})
	if !e.channelEvents().readyToCatchUp() {
		t.Fatal("an item in a column with no managed stage is never deep-fetched and must not hold readiness")
	}
}

func TestFinishEventStampsSourceAndUnverifiedByOrigin(t *testing.T) {
	if got := finishEvent(channelevents.Event{Type: channelevents.Paused}); got.Meta["source"] != "live" {
		t.Errorf("default source = %q", got.Meta["source"])
	}
	if got := finishEvent(channelevents.Event{Type: channelevents.Paused, Meta: map[string]string{"catch_up": "true"}}); got.Meta["source"] != "catch_up" {
		t.Errorf("catch-up source = %q", got.Meta["source"])
	}
	if got := finishEvent(channelevents.Event{Type: channelevents.Paused, Meta: map[string]string{"source": "downtime-delta"}}); got.Meta["source"] != "downtime-delta" {
		t.Errorf("an explicit source must be kept: %q", got.Meta["source"])
	}
}
