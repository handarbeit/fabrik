package engine

import (
	"context"
	"time"
)

// funnelDNSWait is the sleep the Funnel DNS probe uses between probes (#2425).
func (e *Engine) funnelDNSWait() func(ctx context.Context, d time.Duration) bool {
	if e.funnelDNSWaitOverride != nil {
		return e.funnelDNSWaitOverride
	}
	return realReplayWait
}

// SetFunnelDNSWaitForTest replaces the probe's backoff sleep so a test controls
// it without real time. wait returns false if ctx ended first. Call before
// setUpIngestion; production code never sets it.
func (e *Engine) SetFunnelDNSWaitForTest(wait func(ctx context.Context, d time.Duration) bool) {
	e.funnelDNSWaitOverride = wait
}
