package engine

import gh "github.com/handarbeit/fabrik/github"

// SetReplayAPIForTest makes the delivery replay pass (#2144) talk to api
// instead of the engine's GitHub App reconciler. Test seam: call before
// setUpIngestion; production code never sets it.
func (e *Engine) SetReplayAPIForTest(api interface {
	ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error)
	RedeliverAppHookDelivery(deliveryID int64) error
}) {
	e.replayAPIOverride = api
}
