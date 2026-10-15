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
func downtimeEngine(t *testing.T, dir string, warming bool, pre ...func(*Engine)) (*Engine, *chanSink) {
	t.Helper()
	e := apiEngine(t, 0)
	e.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
	for _, p := range pre {
		p(e) // seams that are read at channel start
	}
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
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{"owner/repo#1": {Labels: []string{"a"}, LabelsKnown: true, Status: "Implement"}})
	e, sink := downtimeEngine(t, dir, false)
	defer e.SetDowntimeDeltasDisabledForTest()()
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
	dir := t.TempDir()
	e, sink := downtimeEngine(t, dir, true, func(e *Engine) { e.SetChannelReadyTimeoutForTest(150 * time.Millisecond) })
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

// A close that the first full observation reports as a downtime delta is not
// reported a second time by the live path when the same mutation also closes the
// item (the prior record was still pending because the column was unknown).
func TestDowntimeClosedIsNotReportedTwice(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#7": {Labels: []string{"x"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, false)
	// Known open item with no column yet: the prior record stays pending.
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_7", Number: 7, Repo: "owner/repo", Labels: []string{"x"},
	}})
	drainFor(sink, 200*time.Millisecond)
	// One mutation gives it a column and closes it.
	// A listing path delivers the column and the close: a deep fetch no longer
	// writes either over a known item (#2344).
	e.store.Apply(itemstate.ShallowBoardItemUpdated{Repo: "owner/repo", Number: 7, Item: gh.ProjectItem{
		ID: "I_7", Number: 7, Repo: "owner/repo", Status: "Implement", Labels: []string{"x"}, IsClosed: true,
	}})
	closed := 0
	for _, ev := range drainFor(sink, 400*time.Millisecond) {
		if ev.Type == channelevents.Closed {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("one closed event expected, got %d", closed)
	}
}

// A downtime delta held back while warming must survive a hard kill: the ledger
// keeps the previous run's record for that item until the delta is published, so
// the next start diffs it again instead of finding nothing changed.
func TestWarmingKeepsLedgerAtPriorStateUntilDeltaPublished(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#1": {Labels: []string{"gone"}, LabelsKnown: true, Status: "Implement"},
		"owner/repo#2": {Labels: []string{"same"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, true)
	e.seedAPIItem(t, 1, "Review", "new")  // moved and relabelled while down: a held delta
	e.seedAPIItem(t, 2, "Review", "same") // moved while down too, but another item
	sink.quiet(t, 300*time.Millisecond)   // warming: nothing published

	ce := e.channelEvents()
	ce.ledger.Flush()
	// What a restart after a kill here would load.
	l := channelevents.OpenLedger(dir, nil, nil)
	r1, _ := l.TakePrior("owner/repo#1")
	if r1.Status != "Implement" || len(r1.Labels) != 1 || r1.Labels[0] != "gone" {
		t.Fatalf("the ledger must still hold the prior state of an item with a held delta: %+v", r1)
	}

	// Once the held deltas are handed to the hub the ledger follows.
	ce.spillWarm()
	ce.ledger.Flush()
	l2 := channelevents.OpenLedger(dir, nil, nil)
	r1, _ = l2.TakePrior("owner/repo#1")
	if r1.Status != "Review" || len(r1.Labels) != 1 || r1.Labels[0] != "new" {
		t.Fatalf("after the delta is published the ledger must record the current state: %+v", r1)
	}
}

// An item that closed while the daemon was down and is no longer on the board
// (no column) is still reported: the close is decided at the first observation.
func TestDowntimeClosedWithoutColumnIsReported(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#5": {Labels: []string{"x"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, false)
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_5", Number: 5, Repo: "owner/repo", Labels: []string{"x"}, IsClosed: true,
	}})
	closed := 0
	for _, ev := range drainFor(sink, 500*time.Millisecond) {
		if ev.Type == channelevents.Closed {
			closed++
			if ev.Meta[channelevents.MetaSource] != channelevents.SourceDowntimeDelta {
				t.Errorf("close must be a downtime delta: %+v", ev)
			}
		}
	}
	if closed != 1 {
		t.Fatalf("one downtime closed event expected, got %d", closed)
	}
}

// A subscription made while the restart snapshot was being built was refused its
// own catch-up (not ready yet) and is not in the covered set: catchUpLate queues
// it, and only it.
func TestRestartCatchUpLateReachesSubscriptionsMissedByTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	e, sinkT := downtimeEngine(t, dir, true)
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	ce := e.channelEvents()
	drainFor(sinkT, 300*time.Millisecond) // the item's own arrival/label noise, if any

	evs, covered := e.catchUpAll() // snapshot taken: only T exists
	if len(covered) != 1 {
		t.Fatalf("covered = %v", covered)
	}
	if _, err := ce.hub.Subscribe(channelevents.Subscription{Subscriber: "U"}); err != nil {
		t.Fatal(err)
	}
	sinkU := newChanSink()
	ce.hub.Attach("U", sinkU)
	if got := drainFor(sinkU, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("U must have nothing before the late pass: %+v", got)
	}
	before := len(drainFor(sinkT, 200*time.Millisecond))
	e.catchUpLate(evs, covered)
	got := drainFor(sinkU, 300*time.Millisecond)
	if len(got) == 0 {
		t.Fatal("the late subscription must receive the restart catch-up")
	}
	for _, ev := range got {
		if ev.Meta[channelevents.MetaSource] != channelevents.SourceCatchUp {
			t.Errorf("late catch-up event must be source=catch_up: %+v", ev)
		}
	}
	if extra := drainFor(sinkT, 200*time.Millisecond); len(extra) != 0 {
		t.Fatalf("T was covered and must not get a second snapshot (had %d): %+v", before, extra)
	}
}

// While warming, a live change to an item with a held downtime delta is held
// behind it, so the subscriber sees the item's changes in the order they happened.
func TestWarmingKeepsLiveChangesBehindTheHeldDelta(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#1": {Labels: []string{"a"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, true)
	e.seedAPIItem(t, 1, "Review", "a") // moved while down: a held delta
	sink.quiet(t, 300*time.Millisecond)
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Done"})
	sink.quiet(t, 300*time.Millisecond) // the live move is held behind the delta

	e.boardPopulated.Store(true)
	e.channelEvents().signal()
	var cols []channelevents.Event
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(cols) < 2 {
		for _, ev := range drainFor(sink, 100*time.Millisecond) {
			if ev.Type == channelevents.ColumnChanged {
				cols = append(cols, ev)
			}
		}
	}
	if len(cols) != 2 {
		t.Fatalf("want the delta then the live move, got %+v", cols)
	}
	if cols[0].Meta[channelevents.MetaSource] != channelevents.SourceDowntimeDelta || cols[1].Meta[channelevents.MetaSource] == channelevents.SourceDowntimeDelta {
		t.Errorf("order must be delta then live: %+v", cols)
	}
}
