package engine

import (
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
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

// SetChannelReadyTimeoutForTest changes how long a warming daemon waits for the
// store before running the readiness pass anyway, returning the restore function.
func SetChannelReadyTimeoutForTest(d time.Duration) (restore func()) {
	old := channelReadyTimeout
	channelReadyTimeout = d
	return func() { channelReadyTimeout = old }
}

// SetDowntimeDeltasDisabledForTest turns the downtime diff off (every first
// observation is a silent baseline, as before #2169) and returns the restore
// function. It is how the restart tests show they are not vacuous.
func SetDowntimeDeltasDisabledForTest() (restore func()) {
	downtimeDisabled.Store(true)
	return func() { downtimeDisabled.Store(false) }
}

// StopChannelEventsForTest stops the deriver and closes the hub. Idempotent.
// Persisted state stays on disk, so a following StartChannelEventsForTest on
// the same dir models a daemon restart.
func (e *Engine) StopChannelEventsForTest() { e.closeChannelEvents() }

// SetChannelTimingForTest shortens the deriver's debounce and tick, returning
// the function that restores them.
func SetChannelTimingForTest(debounce, tick time.Duration) (restore func()) {
	oldD, oldT := channelDebounce, channelTick
	channelDebounce, channelTick = debounce, tick
	return func() { channelDebounce, channelTick = oldD, oldT }
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
// and cache-divergence detectors (#2170), returning the function that restores
// them. It is how the acceptance tests show they are not vacuous (FR-014).
func SetAnomalyDetectorsDisabledForTest(retry, wedge, divergence bool) (restore func()) {
	o := [3]bool{anomalyDisabled.retry.Load(), anomalyDisabled.wedge.Load(), anomalyDisabled.divergence.Load()}
	anomalyDisabled.retry.Store(retry)
	anomalyDisabled.wedge.Store(wedge)
	anomalyDisabled.divergence.Store(divergence)
	return func() {
		anomalyDisabled.retry.Store(o[0])
		anomalyDisabled.wedge.Store(o[1])
		anomalyDisabled.divergence.Store(o[2])
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
var (
	decisionPendingDisabled      atomic.Bool
	settlementWithdrawalDisabled atomic.Bool
)

// SetDecisionPendingDisabledForTest turns the decision-pending episode step off
// (no event, no episode counter) and returns the function that restores it.
func SetDecisionPendingDisabledForTest() (restore func()) {
	decisionPendingDisabled.Store(true)
	return func() { decisionPendingDisabled.Store(false) }
}

// SetSettlementWithdrawalDisabledForTest stops settlement-withdrawn events from
// being announced (episodes still close) and returns the restore function.
func SetSettlementWithdrawalDisabledForTest() (restore func()) {
	settlementWithdrawalDisabled.Store(true)
	return func() { settlementWithdrawalDisabled.Store(false) }
}

// SetDecisionAnchorGraceForTest changes how long a suspended item with no exact
// GitHub time waits before it is announced, returning the restore function.
func SetDecisionAnchorGraceForTest(d time.Duration) (restore func()) {
	old := decisionAnchorGrace
	decisionAnchorGrace = d
	return func() { decisionAnchorGrace = old }
}

// SetSettleCauseDisabledForTest turns the re-settle cause off (no cause, no
// actor, and an autonomy change no longer re-settles) and returns the restore
// function: how the cause tests show they are not vacuous.
func SetSettleCauseDisabledForTest() (restore func()) {
	settleCauseDisabled.Store(true)
	return func() { settleCauseDisabled.Store(false) }
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
