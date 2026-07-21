package engine

import (
	"testing"
	"time"
)

func TestIdleBackoffMultiplier(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want int
	}{
		{0, 1},
		{2 * time.Minute, 1},
		{4*time.Minute + 59*time.Second, 1},
		{5 * time.Minute, 2},
		{7 * time.Minute, 2},
		{9*time.Minute + 59*time.Second, 2},
		{10 * time.Minute, 4},
		{15 * time.Minute, 4},
		{19*time.Minute + 59*time.Second, 4},
		{20 * time.Minute, 0},
		{60 * time.Minute, 0},
	}
	for _, tc := range cases {
		got := idleBackoffMultiplier(tc.idle)
		if got != tc.want {
			t.Errorf("idleBackoffMultiplier(%v) = %d, want %d", tc.idle, got, tc.want)
		}
	}
}

func TestComputeEffectiveInterval(t *testing.T) {
	base := 30 * time.Second

	cases := []struct {
		name           string
		idle           time.Duration
		rateLimitRatio float64
		want           time.Duration
	}{
		{"no idle no rateLimit", 0, 1.0, 30 * time.Second},
		{"3min idle no rateLimit", 3 * time.Minute, 1.0, 30 * time.Second},
		{"6min idle (2x)", 6 * time.Minute, 1.0, 60 * time.Second},
		{"12min idle (4x)", 12 * time.Minute, 1.0, 2 * time.Minute},
		{"25min idle (max)", 25 * time.Minute, 1.0, 5 * time.Minute},
		{"rateLimit only (2x tier)", 0, 0.15, 60 * time.Second},
		{"idle 2x wins over rateLimit 2x", 6 * time.Minute, 0.15, 60 * time.Second},
		{"idle 4x wins over rateLimit 2x", 12 * time.Minute, 0.15, 2 * time.Minute},
		{"rateLimit 2x wins over idle 1x", 3 * time.Minute, 0.15, 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeEffectiveInterval(base, tc.idle, tc.rateLimitRatio, false)
			if got != tc.want {
				t.Errorf("computeEffectiveInterval(%v, %v, %v, false) = %v, want %v",
					base, tc.idle, tc.rateLimitRatio, got, tc.want)
			}
		})
	}
}

func TestComputeEffectiveInterval_CapAt5Min(t *testing.T) {
	// With a large configured interval (e.g. 3 minutes), 4x would be 12min,
	// but we cap at 5 minutes.
	base := 3 * time.Minute
	got := computeEffectiveInterval(base, 12*time.Minute, 1.0, false)
	if got != 5*time.Minute {
		t.Errorf("expected cap at 5m, got %v", got)
	}

	// Even 2x of 3 minutes (= 6min) should cap at 5min.
	got = computeEffectiveInterval(base, 6*time.Minute, 1.0, false)
	if got != 5*time.Minute {
		t.Errorf("expected cap at 5m for 2x of 3min base, got %v", got)
	}

	// With webhookHealthy=true the cap rises to 60 min.
	got = computeEffectiveInterval(base, 60*time.Minute, 1.0, true)
	if got != webhookIdleCap {
		t.Errorf("expected webhookIdleCap (%v) when webhook healthy, got %v", webhookIdleCap, got)
	}
}

func TestComputeEffectiveInterval_MaxIdleRateLimit(t *testing.T) {
	base := 30 * time.Second

	// Both backoffs active: idle at max (5min) and rate limit at 2× tier (0.15).
	// max(5min, 2*30s=60s) = 5min.
	got := computeEffectiveInterval(base, 25*time.Minute, 0.15, false)
	if got != 5*time.Minute {
		t.Errorf("expected 5m (idle wins over 2× rate-limit), got %v", got)
	}

	// Both backoffs active: idle at max (5min) and rate limit at 10× tier (0.005).
	// 10×30s = 300s = 5min. max(5min, 5min) = 5min (tie — idle cap governs).
	got = computeEffectiveInterval(base, 25*time.Minute, 0.005, false)
	if got != 5*time.Minute {
		t.Errorf("expected 5m (tie: idle cap == 10× rate-limit at 30s base), got %v", got)
	}
}

func TestComputeEffectiveInterval_RateLimitExceeds5Min(t *testing.T) {
	// Rate-limit backoff alone can exceed 5 minutes (the idle cap doesn't apply).
	// Idle is not active (0 duration), so idleInterval = 3min base.
	base := 3 * time.Minute

	// ratio=0.15 (2× tier): rate-limit interval = 2*3min = 6min.
	// max(3min, 6min) = 6min — the 5min idle cap must NOT clamp this.
	got := computeEffectiveInterval(base, 0, 0.15, false)
	if got != 6*time.Minute {
		t.Errorf("expected 6m (2× of 3min base), got %v", got)
	}

	// ratio=0.07 (4× tier): rate-limit interval = 4*3min = 12min.
	got = computeEffectiveInterval(base, 0, 0.07, false)
	if got != 12*time.Minute {
		t.Errorf("expected 12m (4× of 3min base), got %v", got)
	}

	// ratio=0.03 (6× tier): rate-limit interval = 6*3min = 18min.
	got = computeEffectiveInterval(base, 0, 0.03, false)
	if got != 18*time.Minute {
		t.Errorf("expected 18m (6× of 3min base), got %v", got)
	}

	// ratio=0.005 (10× tier): rate-limit interval = 10*3min = 30min.
	got = computeEffectiveInterval(base, 0, 0.005, false)
	if got != 30*time.Minute {
		t.Errorf("expected 30m (10× of 3min base), got %v", got)
	}
}

func TestComputeEffectiveInterval_RateLimitStepwise(t *testing.T) {
	base := 30 * time.Second

	cases := []struct {
		name  string
		ratio float64
		want  time.Duration
	}{
		{"not in backoff (1.0)", 1.0, 30 * time.Second},
		{"sticky zone (0.35)", 0.35, 60 * time.Second},
		{"active 10-20% (0.15)", 0.15, 60 * time.Second},
		{"boundary exactly 10% (0.10)", 0.10, 60 * time.Second}, // >=0.10 → 2× tier
		{"just below 10% (0.099)", 0.099, 2 * time.Minute},
		{"boundary exactly 5% (0.05)", 0.05, 2 * time.Minute}, // >=0.05 → 4× tier
		{"just below 5% (0.049)", 0.049, 3 * time.Minute},
		{"boundary exactly 1% (0.01)", 0.01, 3 * time.Minute}, // >=0.01 → 6× tier
		{"just below 1% (0.009)", 0.009, 5 * time.Minute},
		{"zero remaining (0.0)", 0.0, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeEffectiveInterval(base, 0, tc.ratio, false)
			if got != tc.want {
				t.Errorf("computeEffectiveInterval(30s, 0, %v, false) = %v, want %v",
					tc.ratio, got, tc.want)
			}
		})
	}
}

// TestNextRateLimitLow_ActivatesWhenLow verifies that nextRateLimitLow transitions
// from false to true when the ratio drops below rateLimitBackoffThreshold (20%).
func TestNextRateLimitLow_ActivatesWhenLow(t *testing.T) {
	if nextRateLimitLow(false, 0.15) != true {
		t.Error("expected true: ratio 15% should activate rate-limit backoff from false")
	}
	if nextRateLimitLow(false, 0.20) != false {
		t.Error("expected false: ratio exactly 20% should not activate (threshold is strictly <)")
	}
}

// TestNextRateLimitLow_StickyBetweenThresholds verifies that once rate-limit backoff
// is active, it remains active when the ratio is between the two thresholds (20%–50%).
func TestNextRateLimitLow_StickyBetweenThresholds(t *testing.T) {
	if nextRateLimitLow(true, 0.25) != true {
		t.Error("expected true: ratio 25% is between thresholds — backoff must stay active (sticky)")
	}
	if nextRateLimitLow(true, 0.49) != true {
		t.Error("expected true: ratio 49% is still below healthy threshold — backoff must stay active")
	}
}

// TestNextRateLimitLow_ClearsWhenHealthy verifies that rate-limit backoff clears
// only when the ratio rises above rateLimitHealthyThreshold (50%).
func TestNextRateLimitLow_ClearsWhenHealthy(t *testing.T) {
	if nextRateLimitLow(true, 0.51) != false {
		t.Error("expected false: ratio 51% exceeds healthy threshold — backoff should clear")
	}
	if nextRateLimitLow(true, 0.50) != true {
		t.Error("expected true: ratio exactly 50% should not clear (threshold is strictly >)")
	}
}

// TestNextRateLimitLow_NoActivationAboveThreshold verifies that when rate-limit
// backoff is not active and quota is healthy, it stays inactive.
func TestNextRateLimitLow_NoActivationAboveThreshold(t *testing.T) {
	if nextRateLimitLow(false, 0.80) != false {
		t.Error("expected false: healthy ratio should not activate backoff")
	}
}

// TestIsRateLimitNearZero_AtZero verifies that remaining=0 is always near zero.
func TestIsRateLimitNearZero_AtZero(t *testing.T) {
	if !isRateLimitNearZero(0, 5000) {
		t.Error("expected true: remaining=0 must be near zero")
	}
}

// TestIsRateLimitNearZero_AtBoundary verifies that remaining=50 with limit=5000 (exactly 1%) is near zero.
func TestIsRateLimitNearZero_AtBoundary(t *testing.T) {
	if !isRateLimitNearZero(50, 5000) {
		t.Error("expected true: remaining=50 (1% of 5000) is at the near-zero boundary")
	}
}

// TestIsRateLimitNearZero_JustAboveBoundary verifies that remaining=51 with limit=5000 (>1%) is not near zero.
func TestIsRateLimitNearZero_JustAboveBoundary(t *testing.T) {
	if isRateLimitNearZero(51, 5000) {
		t.Error("expected false: remaining=51 (>1% of 5000) is just above the near-zero boundary")
	}
}

// TestIsRateLimitNearZero_HealthyQuota verifies that a healthy remaining count is not near zero.
func TestIsRateLimitNearZero_HealthyQuota(t *testing.T) {
	if isRateLimitNearZero(1000, 5000) {
		t.Error("expected false: remaining=1000 is well above the near-zero threshold")
	}
}

// TestIsRateLimitNearZero_ZeroLimit verifies that limit=0 returns false (guards invalid/unknown limit).
func TestIsRateLimitNearZero_ZeroLimit(t *testing.T) {
	if isRateLimitNearZero(0, 0) {
		t.Error("expected false: limit=0 must always return false")
	}
}
