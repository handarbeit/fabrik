package engine

import (
	"context"
	"time"
)

// SetLivenessWaitForTest replaces the sleep the webhook route-liveness
// controller (#2452) uses between wake-ups and while waiting for a probe's
// result, so a test controls it without real time. wait returns false if ctx
// ended first and must block (not return at once) when the test wants the
// controller idle. Call before setUpIngestion; production code never sets it.
func (e *Engine) SetLivenessWaitForTest(wait func(ctx context.Context, d time.Duration) bool) {
	e.livenessWaitOverride = wait
}

// SetLivenessConstantsForTest shrinks the controller's fixed windows for a test
// and returns a restore function. Zero values leave a constant unchanged.
func SetLivenessConstantsForTest(fresh, failWindow, quiet, minProbe, retry, sustained, resultWait, resultPoll time.Duration) (restore func()) {
	o := [8]time.Duration{livenessFreshWindow, livenessFailWindow, livenessQuiet, livenessMinProbeInterval, livenessRetryDelay, livenessSustained, livenessResultWait, livenessResultPoll}
	set := func(dst *time.Duration, v time.Duration) {
		if v != 0 {
			*dst = v
		}
	}
	set(&livenessFreshWindow, fresh)
	set(&livenessFailWindow, failWindow)
	set(&livenessQuiet, quiet)
	set(&livenessMinProbeInterval, minProbe)
	set(&livenessRetryDelay, retry)
	set(&livenessSustained, sustained)
	set(&livenessResultWait, resultWait)
	set(&livenessResultPoll, resultPoll)
	return func() {
		livenessFreshWindow, livenessFailWindow, livenessQuiet, livenessMinProbeInterval = o[0], o[1], o[2], o[3]
		livenessRetryDelay, livenessSustained, livenessResultWait, livenessResultPoll = o[4], o[5], o[6], o[7]
	}
}
