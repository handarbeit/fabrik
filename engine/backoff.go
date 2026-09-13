package engine

import "time"

// rateLimitBackoffThreshold is the fraction of GraphQL rate limit remaining
// below which the engine activates poll backoff and logs a warning.
const rateLimitBackoffThreshold = 0.20

// rateLimitHealthyThreshold is the fraction of GraphQL rate limit remaining
// above which the engine clears an active rate-limit backoff. Using a higher
// threshold than rateLimitBackoffThreshold (hysteresis) prevents thrashing on
// busy boards where quota fluctuates near the activation point.
const rateLimitHealthyThreshold = 0.50

// rateLimitMaxBackoffMultiplier caps the backoff interval as a multiple of the
// configured poll interval (e.g. 10× = 10 * PollSeconds).
const rateLimitMaxBackoffMultiplier = 10

// rateLimitNearZeroPercent is the threshold (as a percentage of Limit) below
// which the previous remaining count is considered "near zero" for the
// recovery wake signal. Budget transitions from near-zero to healthy happen
// at the hourly reset boundary — not via gradual organic replenishment — so
// this guard prevents spurious wakes from ordinary hysteresis crossings.
const rateLimitNearZeroPercent = 1

// maxIdleBackoff is the absolute maximum poll interval during idle backoff,
// regardless of the configured poll interval.
const maxIdleBackoff = 5 * time.Minute

// rateLimitResetBuffer is added to the wait when pausing for a REST rate-limit
// reset, so GitHub has actually rolled the hourly window before we resume (the
// reset timestamp is second-granular and clocks may differ slightly).
const rateLimitResetBuffer = 5 * time.Second

// minPollInterval is R3's defense-in-depth floor: the minimum time
// PollWithBackoff will allow between two actual poll attempts, regardless of
// what triggered the call (ticker or wake). It is not itself the fix for
// #1716's wake-path bypass (that's Run()'s wakeBlockedByRateLimitBackoff gate,
// below) — it's a backstop against any future bypass of that gate, turning an
// unbounded-rate loop into a bounded-rate one. Chosen with a 2x safety margin
// below the lowest realistic --poll value (1s, tests/sim's own default): a
// deployment configuring --poll below ~1s would need to revisit this. A fixed
// unexported constant, not a CLI flag — this is not a tunable operational
// knob, it's a guard against a defect class.
const minPollInterval = 500 * time.Millisecond

// shouldPauseForRESTRateLimit reports whether the engine should skip the entire
// poll work phase because the REST/core budget is exhausted and has not yet
// reset. Unlike GraphQL — which the poll read consumes and which the interval
// backoff (computeEffectiveInterval) throttles organically — the REST/core
// budget is spent by per-item mutations (reactions, labels, comments, merges)
// and janitor fetches. Stretching the poll interval does not conserve it, so
// exhaustion requires a hard pause until the hourly reset rather than a retry
// storm of 403s. Returns false when limit is unknown (limit == 0, e.g. before
// the first REST call) or once reset+rateLimitResetBuffer has passed.
//
// The buffer is applied to the predicate itself, not just to the ticker reset in
// doPollCycle: because doPollCycle can also be invoked directly from the wakeCh
// branch (which bypasses the ticker), a wake in the (reset, reset+buffer) window
// must still pause — otherwise work would resume before GitHub has surely rolled
// the hourly window, risking a fresh burst of 403s.
func shouldPauseForRESTRateLimit(remaining, limit int, reset, now time.Time) bool {
	return isRateLimitNearZero(remaining, limit) && reset.Add(rateLimitResetBuffer).After(now)
}

// idleBackoffMultiplier returns the backoff multiplier for the given idle duration.
// Schedule: 0–5min → 1x, 5–10min → 2x, 10–20min → 4x, 20+ min → 0 (use maxIdleBackoff).
func idleBackoffMultiplier(idleDuration time.Duration) int {
	switch {
	case idleDuration < 5*time.Minute:
		return 1
	case idleDuration < 10*time.Minute:
		return 2
	case idleDuration < 20*time.Minute:
		return 4
	default:
		return 0
	}
}

// nextRateLimitLow applies two-threshold hysteresis to the rate-limit backoff state.
// Activate when ratio < rateLimitBackoffThreshold (20%) and not already low.
// Clear when ratio > rateLimitHealthyThreshold (50%) and currently low.
// Between the two thresholds, state is unchanged (sticky).
func nextRateLimitLow(current bool, ratio float64) bool {
	if !current && ratio < rateLimitBackoffThreshold {
		return true
	}
	if current && ratio > rateLimitHealthyThreshold {
		return false
	}
	return current
}

// isRateLimitNearZero reports whether remaining is at or near zero relative to
// limit (within rateLimitNearZeroPercent). Returns false when limit is 0.
func isRateLimitNearZero(remaining, limit int) bool {
	return limit > 0 && remaining*100 <= limit*rateLimitNearZeroPercent
}

// wakeBlockedByRateLimitBackoff reports whether Run()'s wake path
// (case <-e.wakeCh:) should drop the wake instead of triggering an immediate
// poll (R1, #1716). True while either GraphQL rate-limit backoff
// (e.backoffRateLimitLow) or the REST hard gate (e.backoffRestPaused) is
// active — both are Engine fields PollWithBackoff already persists across
// calls (ADR-1592), so this is a pure read, no new state of its own.
//
// A blocked wake is not re-armed: it is simply dropped, and the ticker —
// already reset to the correct backed-off NextInterval by the prior
// PollWithBackoff call — is what eventually triggers the next poll. This
// deliberately avoids a "pending wake, fire the instant backoff clears" flag:
// that would reintroduce exactly the "hang if the gate is mishandled" risk a
// dropped-with-no-re-arm bug would be silent about, for a benefit (avoiding
// up to one ticker interval of latency on a webhook-driven change arriving
// mid-backoff) that's already the same bound the ticker itself has.
//
// This must NOT filter out the legitimate rate-limit-recovery self-wake
// (poll.go's e.wakeCh <- struct{}{} send, fired the moment GraphQL is
// observed to have recovered from a near-zero state). It doesn't, by
// construction of the call ordering: PollWithBackoff runs synchronously to
// completion — including the e.backoffRateLimitLow = newRateLimitLow
// reassignment, which happens after the recovery wake is sent — before
// control returns to doPollCycle, which itself returns before Run()'s select
// loop re-evaluates and can consume the queued wake. So by the time this
// method is actually called for that wake, e.backoffRateLimitLow has already
// settled to false. A genuine mid-backoff wake (arriving from a webhook while
// e.backoffRateLimitLow is still true) sees the field still true and is
// blocked, exactly as intended. This is an ordering PROPERTY of the current
// single-goroutine code, not an enforced invariant — see
// TestRun_RecoverySelfWake_NotBlockedByBackoffGate for the regression guard.
func (e *Engine) wakeBlockedByRateLimitBackoff() bool {
	return e.backoffRateLimitLow || e.backoffRestPaused
}

// effectiveIdleCap returns the idle backoff cap based on webhook stream health.
// When the webhook stream is healthy or starting up, the cap is extended to
// webhookIdleCap (60 min) since the stream covers events that would otherwise
// require frequent polling. Falls back to maxIdleBackoff (5 min) when unhealthy.
func effectiveIdleCap(webhookHealthy bool) time.Duration {
	if webhookHealthy {
		return webhookIdleCap
	}
	return maxIdleBackoff
}

// computeEffectiveInterval returns the effective poll interval considering both
// idle backoff and rate-limit backoff. The result is max(idle, rateLimit).
// The idle component is capped at effectiveIdleCap(webhookHealthy); the rate-limit
// component uses its own cap (rateLimitMaxBackoffMultiplier × configured).
//
// rateLimitRatio is the remaining-to-total GraphQL quota fraction. Pass 1.0 when
// no rate-limit backoff is active; pass the actual fraction when backoff is active.
// The stepwise escalation schedule (activates when ratio < 1.0):
//
//	>=10% remaining: 2× configured  (includes sticky hysteresis zone 20%–50%)
//	>=5% and <10%:   4× configured
//	>=1% and <5%:    6× configured
//	    <1%:        10× configured  (rateLimitMaxBackoffMultiplier)
func computeEffectiveInterval(configuredInterval time.Duration, idleDuration time.Duration, rateLimitRatio float64, webhookHealthy bool) time.Duration {
	cap := effectiveIdleCap(webhookHealthy)

	var idleInterval time.Duration
	mult := idleBackoffMultiplier(idleDuration)
	if mult == 0 {
		idleInterval = cap
	} else {
		idleInterval = configuredInterval * time.Duration(mult)
	}
	if idleInterval > cap {
		idleInterval = cap
	}

	rateLimitInterval := configuredInterval
	if rateLimitRatio < 1.0 {
		var rlMult int
		switch {
		case rateLimitRatio >= 0.10:
			rlMult = 2
		case rateLimitRatio >= 0.05:
			rlMult = 4
		case rateLimitRatio >= 0.01:
			rlMult = 6
		default:
			rlMult = rateLimitMaxBackoffMultiplier
		}
		rateLimitInterval = configuredInterval * time.Duration(rlMult)
		maxRL := configuredInterval * time.Duration(rateLimitMaxBackoffMultiplier)
		if rateLimitInterval > maxRL {
			rateLimitInterval = maxRL
		}
	}

	effective := idleInterval
	if rateLimitInterval > effective {
		effective = rateLimitInterval
	}
	return effective
}
