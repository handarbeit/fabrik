package engine

import (
	"sort"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// The final flush and the live-label-delta rule (#2450, ADR 2450).

func labelEventsOf(evs []channelevents.Event, typ channelevents.EventType) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev.Label)
		}
	}
	sort.Strings(out)
	return out
}

// flushQueue publishes what is queued, once, and nothing else.
func TestFlushQueuePublishesQueuedEventsOnce(t *testing.T) {
	e, sink := downtimeEngine(t, t.TempDir(), false)
	e.seedAPIItem(t, 1, "Implement", "a")
	drainFor(sink, 200*time.Millisecond)

	e.SetChannelConsumerParkedForTest(true)
	e.ApplyLabelWebhookForTest("owner/repo", 1, "late", true, "alice")
	if got := drainFor(sink, 150*time.Millisecond); len(got) != 0 {
		t.Fatalf("a parked consumer must publish nothing, got %+v", got)
	}
	ce := e.channelEvents()
	ce.flushQueue()
	ce.flushQueue() // the second finds an empty queue
	got := drainFor(sink, 300*time.Millisecond)
	if l := labelEventsOf(got, channelevents.LabelApplied); len(l) != 1 || l[0] != "late" {
		t.Fatalf("want the queued label announced once, got %+v", got)
	}
}

// Stopping the consumer publishes a change still queued, and the next hub on the
// same directory holds it exactly once.
func TestCloseChannelEventsPublishesQueuedChange(t *testing.T) {
	for _, off := range []bool{false, true} {
		name := "flush"
		if off {
			name = "flush-disabled"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			e := apiEngine(t, 0)
			e.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
			if off {
				e.SetFinalChannelFlushDisabledForTest()
			}
			e.fabrikDir = t.TempDir()
			e.startChannelEventsIn(dir, false)
			ce := e.channelEvents()
			if _, err := ce.hub.Subscribe(channelevents.Subscription{Subscriber: "T"}); err != nil {
				t.Fatal(err)
			}
			e.seedAPIItem(t, 1, "Implement", "a")
			if err := e.DrainChannelEventsForTest(5 * time.Second); err != nil {
				t.Fatal(err)
			}
			e.SetChannelConsumerParkedForTest(true)
			e.ApplyLabelWebhookForTest("owner/repo", 1, "queued", true, "alice")
			e.closeChannelEvents()

			hub, err := channelevents.Open(channelevents.Options{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer hub.Close()
			sink := newChanSink()
			hub.Attach("T", sink)
			var got []channelevents.Event
			got = append(got, drainFor(sink, 400*time.Millisecond)...)
			n := len(labelEventsOf(got, channelevents.LabelApplied))
			if want := map[bool]int{false: 1, true: 0}[off]; n != want {
				t.Errorf("queued label held %d time(s), want %d: %+v", n, want, got)
			}
		})
	}
}

// An event queued for an item with a held downtime delta stays held through the
// flush and reaches the hub behind the delta (spillWarm), in order.
func TestFlushQueueKeepsWarmingOrder(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, map[string]channelevents.Observation{
		"owner/repo#1": {Labels: []string{"a"}, LabelsKnown: true, Status: "Implement"},
	})
	e, sink := downtimeEngine(t, dir, true)
	e.seedAPIItem(t, 1, "Review", "a") // moved while down: a held delta
	sink.quiet(t, 200*time.Millisecond)
	e.SetChannelConsumerParkedForTest(true)
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Done"})
	ce := e.channelEvents()
	ce.flushQueue()
	sink.quiet(t, 200*time.Millisecond) // held behind the delta, not published
	ce.spillWarm()
	var cols []channelevents.Event
	for _, ev := range drainFor(sink, 400*time.Millisecond) {
		if ev.Type == channelevents.ColumnChanged {
			cols = append(cols, ev)
		}
	}
	if len(cols) != 2 || cols[0].Meta[channelevents.MetaSource] != channelevents.SourceDowntimeDelta ||
		cols[1].Meta[channelevents.MetaSource] == channelevents.SourceDowntimeDelta {
		t.Fatalf("want the delta then the live move, got %+v", cols)
	}
}

func pendEngine(labels ...string) (*channelEvents, string) {
	const key = "owner/repo#1"
	ce := &channelEvents{pend: map[string]*priorEntry{key: {rec: channelevents.ItemRecord{Labels: labels, LabelsKnown: true}}}}
	return ce, key
}

func add(l string) itemstate.LabelDelta { return itemstate.LabelDelta{Label: l, Added: true} }
func del(l string) itemstate.LabelDelta { return itemstate.LabelDelta{Label: l} }

func TestNoteLiveLabelDeltasAdjustsThePendingPrior(t *testing.T) {
	ce, key := pendEngine("a", "c")
	skip := ce.noteLiveLabelDeltas(key, []itemstate.LabelDelta{add("b"), del("c")}, nil)
	if len(skip) != 0 {
		t.Errorf("changes that alter the prior are announced live, skip=%v", skip)
	}
	got := ce.pend[key].rec.Labels
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("prior = %v, want [a b]", got)
	}
}

func TestNoteLiveLabelDeltasAddRemoveReaddNetsOutPerChange(t *testing.T) {
	ce, key := pendEngine("a")
	for i, d := range []itemstate.LabelDelta{add("x"), del("x"), add("x")} {
		if skip := ce.noteLiveLabelDeltas(key, []itemstate.LabelDelta{d}, nil); len(skip) != 0 {
			t.Errorf("change %d is a real one, got skip=%v", i, skip)
		}
	}
	got := ce.pend[key].rec.Labels
	if len(got) != 2 || got[0] != "a" || got[1] != "x" {
		t.Errorf("prior = %v, want [a x]", got)
	}
}

func TestNoteLiveLabelDeltasSkipsAReplayOfWhatThePriorHolds(t *testing.T) {
	ce, key := pendEngine("a")
	skip := ce.noteLiveLabelDeltas(key, []itemstate.LabelDelta{add("a"), del("zz")}, nil)
	if !skip[labelDeltaKey(add("a"))] || !skip[labelDeltaKey(del("zz"))] {
		t.Errorf("replays of recorded state must not be announced again, skip=%v", skip)
	}
}

func TestNoteLiveLabelDeltasLeavesCoveredAndChurnAlone(t *testing.T) {
	ce, key := pendEngine("a")
	covered := map[string]bool{labelDeltaKey(add("b")): true}
	skip := ce.noteLiveLabelDeltas(key, []itemstate.LabelDelta{add("b"), add("fabrik:locked:me")}, covered)
	if !skip[labelDeltaKey(add("b"))] {
		t.Errorf("a change the diff just announced is skipped, skip=%v", skip)
	}
	if got := ce.pend[key].rec.Labels; len(got) != 1 || got[0] != "a" {
		t.Errorf("covered and churn labels must not enter the prior, got %v", got)
	}
}

func TestNoteLiveLabelDeltasNoPendingPriorIsANoOp(t *testing.T) {
	ce := &channelEvents{pend: map[string]*priorEntry{}}
	if skip := ce.noteLiveLabelDeltas("owner/repo#9", []itemstate.LabelDelta{add("x")}, nil); len(skip) != 0 {
		t.Errorf("no prior, nothing to skip: %v", skip)
	}
	ce.pend["k"] = &priorEntry{rec: channelevents.ItemRecord{Labels: []string{"a"}}, labelsDone: true}
	if skip := ce.noteLiveLabelDeltas("k", []itemstate.LabelDelta{add("a")}, nil); len(skip) != 0 {
		t.Errorf("a finished diff is not adjusted: %v", skip)
	}
}
