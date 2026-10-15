package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Two engines in one test binary must not see each other's seams (#2210): a
// neutralisation or timing set on A leaves B on the production defaults.
func TestChannelSeamsAreScopedToOneEngine(t *testing.T) {
	a, b := apiEngine(t, 0), apiEngine(t, 0)

	if got := b.chanDebounce(); got != defaultChannelDebounce {
		t.Fatalf("fresh engine debounce = %s, want default %s", got, defaultChannelDebounce)
	}
	if got := b.decisionAnchorGrace(); got != defaultDecisionAnchorGrace {
		t.Fatalf("fresh engine anchor grace = %s", got)
	}

	a.SetChannelTimingForTest(5*time.Millisecond, 7*time.Millisecond)
	a.SetChannelReadyTimeoutForTest(time.Second)
	a.SetDecisionAnchorGraceForTest(0)
	a.SetSituationMinForTest(time.Second)
	a.SetDowntimeDeltasDisabledForTest()
	a.SetAnomalyDetectorsDisabledForTest(true, true, true)
	a.SetDecisionPendingDisabledForTest()
	a.SetSettlementWithdrawalDisabledForTest()
	a.SetSettleCauseDisabledForTest()
	a.SetSituationDedupDisabledForTest()
	a.SetReviewReinvokeDisabledForTest()

	if a.chanDebounce() != 5*time.Millisecond || a.chanTick() != 7*time.Millisecond ||
		a.chanReadyTimeout() != time.Second || a.decisionAnchorGrace() != 0 {
		t.Fatalf("engine A did not take its own settings")
	}
	if b.chanDebounce() != defaultChannelDebounce || b.chanTick() != defaultChannelTick ||
		b.chanReadyTimeout() != defaultChannelReadyTimeout || b.decisionAnchorGrace() != defaultDecisionAnchorGrace {
		t.Fatalf("engine B saw engine A's timing")
	}
	var zero channelTestSettings
	if got := b.chanTest.situationMin.Load(); got != 0 {
		t.Fatalf("B situationMin = %d", got)
	}
	flags := func(s *channelTestSettings) [9]bool {
		return [9]bool{s.downtimeOff.Load(), s.anomalyRetryOff.Load(), s.anomalyWedgeOff.Load(),
			s.anomalyDivOff.Load(), s.decisionPendingOff.Load(), s.withdrawalOff.Load(),
			s.settleCauseOff.Load(), s.situationDedupOff.Load(), s.reviewReinvokeOff.Load()}
	}
	if flags(&b.chanTest) != flags(&zero) {
		t.Fatalf("engine B saw engine A's neutralisations: %v", flags(&b.chanTest))
	}
	for i, on := range flags(&a.chanTest) {
		if !on {
			t.Errorf("engine A neutralisation %d did not take", i)
		}
	}

	// A restart helper carries the settings to the rebuilt engine.
	c := apiEngine(t, 0)
	c.CopyChannelTestSettingsFrom(a)
	if flags(&c.chanTest) != flags(&a.chanTest) || c.chanDebounce() != 5*time.Millisecond || c.decisionAnchorGrace() != 0 {
		t.Fatalf("CopyChannelTestSettingsFrom lost settings")
	}
}

// The restore function puts the previous value back, and only on its own engine.
func TestChannelSeamRestore(t *testing.T) {
	a, b := apiEngine(t, 0), apiEngine(t, 0)
	b.SetChannelTimingForTest(9*time.Millisecond, time.Hour)
	restore := a.SetChannelTimingForTest(5*time.Millisecond, time.Hour)
	restore()
	if a.chanDebounce() != defaultChannelDebounce {
		t.Fatalf("restore did not reset A: %s", a.chanDebounce())
	}
	if b.chanDebounce() != 9*time.Millisecond {
		t.Fatalf("restoring A changed B: %s", b.chanDebounce())
	}
	r := a.SetSettleCauseDisabledForTest()
	r()
	if a.chanTest.settleCauseOff.Load() {
		t.Fatal("restore did not clear the neutralisation")
	}
}

// A drain with no hub returns at once.
func TestDrainChannelEventsNoHub(t *testing.T) {
	e := apiEngine(t, 0)
	start := time.Now()
	if err := e.DrainChannelEventsForTest(time.Minute); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("drain with no hub blocked")
	}
}

// The drain returns as soon as the consumer has processed what a store write
// enqueued — without sitting out a long debounce — and the sink then holds it.
func TestDrainChannelEventsWaitsForTheConsumerNotTheDebounce(t *testing.T) {
	e := apiEngine(t, 0)
	e.SetChannelTimingForTest(time.Hour, time.Hour) // a debounce nothing would wait out
	e, sink := channelEngineOn(t, e, 0, nil)
	e.seedAPIItem(t, 1, "Implement")
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:paused"})

	start := time.Now()
	if err := e.DrainChannelEventsForTest(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("drain took %s, the debounce was waited out", d)
	}
	// Dirty items were forced due and evaluated; the label event was published.
	deadline := time.Now().Add(5 * time.Second)
	for e.ChannelDeliverableForTest("T") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ev := sink.nextOfType(t, channelevents.LabelApplied)
	if ev.Label != "fabrik:paused" {
		t.Fatalf("event = %+v", ev)
	}
}

// A consumer that cannot drain fails the wait with a message naming the cause
// rather than hanging. A hub-less channelEvents with a queued event and no
// consumer goroutine stands in for a stuck consumer.
func TestDrainChannelEventsTimesOut(t *testing.T) {
	e := apiEngine(t, 0)
	ce := &channelEvents{e: e, wake: make(chan struct{}, 1), dirty: map[string]*dirtyItem{}}
	ce.queue = []channelevents.Event{{Type: channelevents.Merged}}
	e.channelMu.Lock()
	e.channel = ce
	e.channelMu.Unlock()
	t.Cleanup(func() { e.channelMu.Lock(); e.channel = nil; e.channelMu.Unlock() })
	err := e.DrainChannelEventsForTest(50 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not drain") {
		t.Fatalf("err = %v", err)
	}
}
