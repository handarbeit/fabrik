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
