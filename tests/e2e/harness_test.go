//go:build e2e

package e2e

import (
	"math/rand/v2"
	"testing"
	"time"
)

// TestJitterWithRandBounds verifies jitterWithRand never returns a negative
// duration and always stays within the documented [base*0.8, base*1.2) range.
func TestJitterWithRandBounds(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	base := 15 * time.Second
	lo := time.Duration(float64(base) * 0.8)
	hi := time.Duration(float64(base) * 1.2)

	for i := 0; i < 10000; i++ {
		d := jitterWithRand(r, base)
		if d < 0 {
			t.Fatalf("sample %d: got negative duration %v", i, d)
		}
		if d < lo || d >= hi {
			t.Fatalf("sample %d: %v outside documented bounds [%v, %v)", i, d, lo, hi)
		}
	}
}

// TestJitterWithRandDistribution checks the sample mean stays close to base
// and that values actually vary (i.e. jitter isn't a no-op).
func TestJitterWithRandDistribution(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	base := 15 * time.Second

	const n = 10000
	var sum time.Duration
	min, max := base, base
	for i := 0; i < n; i++ {
		d := jitterWithRand(r, base)
		sum += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	mean := sum / n

	// Mean should land close to base — within 5% of it, well inside the ±20%
	// jitter band, since deviations should average out over n=10000 samples.
	tolerance := time.Duration(float64(base) * 0.05)
	if diff := mean - base; diff < -tolerance || diff > tolerance {
		t.Fatalf("sample mean %v too far from base %v (tolerance %v)", mean, base, tolerance)
	}
	if min == max {
		t.Fatalf("all %d samples identical (%v) — jitter is not varying", n, min)
	}
}

// TestJitterWithRandDeterministic confirms two generators seeded identically
// produce identical output sequences, satisfying the "deterministic under
// test" requirement.
func TestJitterWithRandDeterministic(t *testing.T) {
	base := 15 * time.Second
	r1 := rand.New(rand.NewPCG(42, 42))
	r2 := rand.New(rand.NewPCG(42, 42))

	for i := 0; i < 100; i++ {
		d1 := jitterWithRand(r1, base)
		d2 := jitterWithRand(r2, base)
		if d1 != d2 {
			t.Fatalf("sample %d: identically-seeded generators diverged: %v != %v", i, d1, d2)
		}
	}
}

// TestNewPollRandSeeded verifies that setting E2E_JITTER_SEED produces a
// reproducible RNG: two calls to newPollRand with the same seed value yield
// generators that produce identical jitter sequences.
func TestNewPollRandSeeded(t *testing.T) {
	t.Setenv("E2E_JITTER_SEED", "12345")
	r1 := newPollRand()
	r2 := newPollRand()

	base := 15 * time.Second
	for i := 0; i < 100; i++ {
		d1 := jitterWithRand(r1, base)
		d2 := jitterWithRand(r2, base)
		if d1 != d2 {
			t.Fatalf("sample %d: newPollRand with same E2E_JITTER_SEED diverged: %v != %v", i, d1, d2)
		}
	}
}

// TestNewPollRandMalformedSeedFallsBack verifies an invalid E2E_JITTER_SEED
// falls back to auto-seeding rather than panicking or erroring.
func TestNewPollRandMalformedSeedFallsBack(t *testing.T) {
	t.Setenv("E2E_JITTER_SEED", "not-a-number")
	r := newPollRand()
	if r == nil {
		t.Fatal("newPollRand returned nil for malformed E2E_JITTER_SEED")
	}
	// Should still produce a value in bounds.
	d := jitterWithRand(r, 15*time.Second)
	if d < 12*time.Second || d >= 18*time.Second {
		t.Fatalf("jitter from fallback-seeded generator out of bounds: %v", d)
	}
}
