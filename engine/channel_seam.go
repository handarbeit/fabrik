package engine

import (
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Test seams for the channel-event hub (#1968), for tests/sim, which drives the
// engine through PollOnce rather than Run() and so never reaches
// startChannelEvents. Production never calls them.

// StartChannelEventsForTest starts the channel hub with its state under dir and
// returns it (nil when it failed to open), so a test can subscribe and attach a
// sink. Pair with StopChannelEventsForTest.
func (e *Engine) StartChannelEventsForTest(dir string) *channelevents.Hub {
	e.startChannelEventsIn(dir, false)
	if ce := e.channelEvents(); ce != nil {
		return ce.hub
	}
	return nil
}

// StartChannelEventsWarmingForTest is StartChannelEventsForTest with the
// production start-up path (#2169): the daemon is warming until the readiness
// pass has queued the restart catch-up to every subscriber and published the
// downtime deltas. Use it to model a daemon restart.
func (e *Engine) StartChannelEventsWarmingForTest(dir string) *channelevents.Hub {
	e.startChannelEventsIn(dir, true)
	if ce := e.channelEvents(); ce != nil {
		return ce.hub
	}
	return nil
}

// SetChannelReadyTimeoutForTest changes how long this engine's warming daemon
// waits for the store before running the readiness pass anyway, returning the
// restore function. Call it before the channel events start.
func (e *Engine) SetChannelReadyTimeoutForTest(d time.Duration) (restore func()) {
	old := e.chanTest.readyTimeout.Swap(int64(d))
	return func() { e.chanTest.readyTimeout.Store(old) }
}

// SetDowntimeDeltasDisabledForTest turns the downtime diff off (every first
// observation is a silent baseline, as before #2169) and returns the restore
// function. It is how the restart tests show they are not vacuous.
func (e *Engine) SetDowntimeDeltasDisabledForTest() (restore func()) {
	e.chanTest.downtimeOff.Store(true)
	return func() { e.chanTest.downtimeOff.Store(false) }
}

// StopChannelEventsForTest stops the deriver and closes the hub. Idempotent.
// Persisted state stays on disk, so a following StartChannelEventsForTest on
// the same dir models a daemon restart.
func (e *Engine) StopChannelEventsForTest() { e.closeChannelEvents() }

// SetChannelTimingForTest shortens this engine's deriver debounce and tick,
// returning the function that restores them. Call it before the channel events
// start: the tick is captured when the consumer starts. Per-engine since #2210,
// so channel tests can run in parallel.
func (e *Engine) SetChannelTimingForTest(debounce, tick time.Duration) (restore func()) {
	oldD, oldT := e.chanTest.debounce.Swap(int64(debounce)), e.chanTest.tick.Swap(int64(tick))
	return func() { e.chanTest.debounce.Store(oldD); e.chanTest.tick.Store(oldT) }
}

// MarkBoardPopulatedForTest opens the arrival gate before the first poll
// completes, so items the first poll populates are announced as arrivals. It
// exists to show that the "present at startup" tests are not vacuous.
func (e *Engine) MarkBoardPopulatedForTest() { e.boardPopulated.Store(true) }

// SetLegacyLabelBaselineForTest restores the pre-#2123 label baseline rule in the
// engine's store (see itemstate.Store.SetLegacyLabelBaselineForTest), so a test
// can show the restart flood the new rule removes.
func (e *Engine) SetLegacyLabelBaselineForTest(on bool) { e.store.SetLegacyLabelBaselineForTest(on) }

// SetAnomalyDetectorsDisabledForTest neutralises the retry-starved, item-wedged
// and cache-divergence detectors (#2170) on this engine, returning the function
// that restores them. It is how the acceptance tests show they are not vacuous
// (FR-014).
func (e *Engine) SetAnomalyDetectorsDisabledForTest(retry, wedge, divergence bool) (restore func()) {
	c := &e.chanTest
	o := [3]bool{c.anomalyRetryOff.Load(), c.anomalyWedgeOff.Load(), c.anomalyDivOff.Load()}
	c.anomalyRetryOff.Store(retry)
	c.anomalyWedgeOff.Store(wedge)
	c.anomalyDivOff.Store(divergence)
	return func() {
		c.anomalyRetryOff.Store(o[0])
		c.anomalyWedgeOff.Store(o[1])
		c.anomalyDivOff.Store(o[2])
	}
}

// SetAnomalyThresholdsForTest overrides the retry-starved and item-wedged
// thresholds and the cache-divergence window (a zero value keeps the default).
// Call it before the channel events start; the engine reads them without a lock.
// The 2 × poll-interval floor still applies.
func (e *Engine) SetAnomalyThresholdsForTest(retryStarved, itemWedged, divergenceWindow time.Duration) {
	e.cfg.RetryStarvedThreshold = retryStarved
	e.cfg.ItemWedgedThreshold = itemWedged
	e.cfg.CacheDivergenceWindow = divergenceWindow
}

// Neutralisation seams for #2120's tests: each disables one announcement so a
// test can show that the assertion it guards fails without it. Production never
// calls them.

// SetDecisionPendingDisabledForTest turns the decision-pending episode step off
// (no event, no episode counter) and returns the function that restores it.
func (e *Engine) SetDecisionPendingDisabledForTest() (restore func()) {
	e.chanTest.decisionPendingOff.Store(true)
	return func() { e.chanTest.decisionPendingOff.Store(false) }
}

// SetSettlementWithdrawalDisabledForTest stops settlement-withdrawn events from
// being announced (episodes still close) and returns the restore function.
func (e *Engine) SetSettlementWithdrawalDisabledForTest() (restore func()) {
	e.chanTest.withdrawalOff.Store(true)
	return func() { e.chanTest.withdrawalOff.Store(false) }
}

// SetDecisionAnchorGraceForTest changes how long a suspended item with no exact
// GitHub time waits before it is announced, returning the restore function.
func (e *Engine) SetDecisionAnchorGraceForTest(d time.Duration) (restore func()) {
	old := e.chanTest.anchorGrace.Swap(int64(d) + 1)
	return func() { e.chanTest.anchorGrace.Store(old) }
}

// SetSettleCauseDisabledForTest turns the re-settle cause off (no cause, no
// actor, and an autonomy change no longer re-settles) and returns the restore
// function: how the cause tests show they are not vacuous.
func (e *Engine) SetSettleCauseDisabledForTest() (restore func()) {
	e.chanTest.settleCauseOff.Store(true)
	return func() { e.chanTest.settleCauseOff.Store(false) }
}

// ApplyLabelWebhookForTest applies a label change to the engine's store as the
// `issues` labeled / unlabeled webhook would, naming sender (the tests/sim
// scenarios otherwise only see label changes through polling, whose actor is
// unknown by design, ADR 2125). Test seam only.
func (e *Engine) ApplyLabelWebhookForTest(repo string, number int, label string, added bool, sender string) {
	if added {
		e.store.Apply(itemstate.IssueLabeled{Repo: repo, Number: number, Label: label, Sender: sender})
		return
	}
	e.store.Apply(itemstate.IssueUnlabeled{Repo: repo, Number: number, Label: label, Sender: sender})
}

// ChannelStreamerForTest returns the localapi.Streamer the daemon hands its
// socket server (nil when the hub is not running), so a scenario can subscribe
// and attach through the same code the MCP shim reaches.
func (e *Engine) ChannelStreamerForTest() localapi.Streamer { return e.channelStreamer() }

// SetSituationMinForTest shortens the lowest situation_seconds the hub of this
// engine accepts, returning the restore function. Call it before the channel
// events start: the value is copied into the hub's options then.
func (e *Engine) SetSituationMinForTest(d time.Duration) (restore func()) {
	old := e.chanTest.situationMin.Swap(int64(d))
	return func() { e.chanTest.situationMin.Store(old) }
}

// SetSituationDedupDisabledForTest sends every situation digest regardless of
// whether the view changed, returning the restore function. It is how the
// dedup tests show they are not vacuous: against this builder the "two quiet
// ticks publish one digest" assertion must fail.
func (e *Engine) SetSituationDedupDisabledForTest() (restore func()) {
	e.chanTest.situationDedupOff.Store(true)
	return func() { e.chanTest.situationDedupOff.Store(false) }
}

// Seams for #2450 (announce-once across a SIGHUP restart). The first two are
// neutralisations: they show the twin's positive assertions fail without the
// fix. The third parks the consumer so a change is deterministically "queued
// after the last drain".

// SetFinalChannelFlushDisabledForTest stops the consumer publishing its queued
// events when it is stopped (the pre-#2450 behaviour) and returns the restore
// function.
func (e *Engine) SetFinalChannelFlushDisabledForTest() (restore func()) {
	e.chanTest.finalFlushOff.Store(true)
	return func() { e.chanTest.finalFlushOff.Store(false) }
}

// SetLiveLabelDeltaDedupDisabledForTest turns off the adjustment of a pending
// restart prior by live label deltas (the pre-#2450 behaviour) and returns the
// restore function.
func (e *Engine) SetLiveLabelDeltaDedupDisabledForTest() (restore func()) {
	e.chanTest.liveLabelDedupOff.Store(true)
	return func() { e.chanTest.liveLabelDedupOff.Store(false) }
}

// SetChannelConsumerParkedForTest parks or releases the channel consumer: while
// parked it does not drain on a wake, timer or tick, but still stops. Changes
// applied meanwhile stay queued.
func (e *Engine) SetChannelConsumerParkedForTest(parked bool) {
	e.chanTest.consumerParked.Store(parked)
	if ce := e.channelEvents(); ce != nil {
		ce.signal()
	}
}
