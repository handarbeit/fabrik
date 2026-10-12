package engine

import (
	"time"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Test seams for the channel-event hub (#1968), for tests/sim, which drives the
// engine through PollOnce rather than Run() and so never reaches
// startChannelEvents. Production never calls them.

// StartChannelEventsForTest starts the channel hub with its state under dir and
// returns it (nil when it failed to open), so a test can subscribe and attach a
// sink. Pair with StopChannelEventsForTest.
func (e *Engine) StartChannelEventsForTest(dir string) *channelevents.Hub {
	e.startChannelEventsIn(dir)
	if ce := e.channelEvents(); ce != nil {
		return ce.hub
	}
	return nil
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
