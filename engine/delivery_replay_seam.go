package engine

import (
	"context"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// SetReplayAPIForTest makes the delivery replay pass (#2144) talk to api
// instead of the engine's GitHub App reconciler. Test seam: call before
// setUpIngestion; production code never sets it.
func (e *Engine) SetReplayAPIForTest(api interface {
	ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error)
	RedeliverAppHookDelivery(deliveryID int64) error
}) {
	e.replayAPIOverride = api
}

// SetReplayWaitForTest replaces the sleep the delivery replayer uses for its
// startup grace (tailscale) and its follow-up delay, so a test controls both
// without real time. wait returns false if ctx ended first. Call before
// setUpIngestion; production code never sets it.
func (e *Engine) SetReplayWaitForTest(wait func(ctx context.Context, d time.Duration) bool) {
	e.replayWaitOverride = wait
}
