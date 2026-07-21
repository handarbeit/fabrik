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
