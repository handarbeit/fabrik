//go:build e2e

package e2e

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
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

// TestPollSleepBoundsAndConcurrency exercises pollSleep itself (not just the
// pure jitterWithRand helper), verifying the observed sleep duration is
// consistent with the documented ±20% band and that the shared, mutex-guarded
// pollRand survives concurrent callers under -race.
func TestPollSleepBoundsAndConcurrency(t *testing.T) {
	const base = 20 * time.Millisecond
	// time.Sleep only guarantees "at least" the requested duration, so bounds
	// are checked loosely (half the lower bound, 3x the upper bound) to avoid
	// flaking under scheduler slack on a loaded CI machine.
	lo := time.Duration(float64(base)*0.8) / 2
	hi := time.Duration(float64(base)*1.2) * 3

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			pollSleep(base)
			elapsed := time.Since(start)
			if elapsed < lo || elapsed > hi {
				t.Errorf("pollSleep(%v) returned after %v, outside sanity bounds [%v, %v]", base, elapsed, lo, hi)
			}
		}()
	}
	wg.Wait()
}

// TestWriteEnvFileValueReplacesExistingKey verifies an existing KEY=value
// line is replaced in place, and unrelated lines (including comments and
// blanks) are preserved verbatim.
func TestWriteEnvFileValueReplacesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	original := "# a comment\nFABRIK_TOKEN=abc123\n\nFABRIK_MERGE_TRAIN=on\nOTHER=1\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}

	if err := writeEnvFileValue(path, "FABRIK_MERGE_TRAIN", "off"); err != nil {
		t.Fatalf("writeEnvFileValue: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back .env: %v", err)
	}
	want := "# a comment\nFABRIK_TOKEN=abc123\n\nFABRIK_MERGE_TRAIN=off\nOTHER=1\n"
	if string(got) != want {
		t.Fatalf("writeEnvFileValue replace mismatch:\ngot:  %q\nwant: %q", string(got), want)
	}
}

// TestWriteEnvFileValueAppendsMissingKey verifies a key not already present
// is appended as a new line, and that a brand-new (nonexistent) file is
// created cleanly.
func TestWriteEnvFileValueAppendsMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	original := "FABRIK_TOKEN=abc123\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}

	if err := writeEnvFileValue(path, "FABRIK_MERGE_TRAIN", "on"); err != nil {
		t.Fatalf("writeEnvFileValue: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back .env: %v", err)
	}
	want := "FABRIK_TOKEN=abc123\nFABRIK_MERGE_TRAIN=on\n"
	if string(got) != want {
		t.Fatalf("writeEnvFileValue append mismatch:\ngot:  %q\nwant: %q", string(got), want)
	}

	// Nonexistent file: should be created from scratch with just the new line.
	freshPath := filepath.Join(t.TempDir(), "fresh.env")
	if err := writeEnvFileValue(freshPath, "FABRIK_MERGE_TRAIN", "off"); err != nil {
		t.Fatalf("writeEnvFileValue on nonexistent file: %v", err)
	}
	got, err = os.ReadFile(freshPath)
	if err != nil {
		t.Fatalf("read back fresh .env: %v", err)
	}
	if string(got) != "FABRIK_MERGE_TRAIN=off\n" {
		t.Fatalf("writeEnvFileValue on fresh file mismatch: got %q", string(got))
	}
}

// TestResolveTrainModePrecedence verifies E2E_TRAIN_MODE takes precedence
// over the bed's .env file, that unset E2E_TRAIN_MODE falls back to reading
// .env, and that an invalid E2E_TRAIN_MODE value fails the test rather than
// silently defaulting (unlike the lenient .env fallback).
func TestResolveTrainModePrecedence(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("FABRIK_MERGE_TRAIN=on\n"), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}
	env := &Env{FabrikTestDir: dir}

	t.Run("E2E_TRAIN_MODE takes precedence over .env", func(t *testing.T) {
		t.Setenv("E2E_TRAIN_MODE", "off")
		if got := resolveTrainMode(t, env); got != "off" {
			t.Fatalf("resolveTrainMode() = %q, want %q (E2E_TRAIN_MODE should override .env's \"on\")", got, "off")
		}
	})

	t.Run("falls back to .env when E2E_TRAIN_MODE unset", func(t *testing.T) {
		t.Setenv("E2E_TRAIN_MODE", "")
		if got := resolveTrainMode(t, env); got != "on" {
			t.Fatalf("resolveTrainMode() = %q, want %q (fallback to .env's FABRIK_MERGE_TRAIN=on)", got, "on")
		}
	})

	t.Run("case-insensitive E2E_TRAIN_MODE", func(t *testing.T) {
		t.Setenv("E2E_TRAIN_MODE", "ON")
		if got := resolveTrainMode(t, env); got != "on" {
			t.Fatalf("resolveTrainMode() = %q, want %q", got, "on")
		}
	})
}

// TestResolveTrainModeUnrecognizedEnvFallsBackToOff verifies a malformed
// .env value (not a resolveTrainMode concern directly, but exercises the
// fallback path resolveTrainMode delegates to when E2E_TRAIN_MODE is unset)
// still normalizes to "off" per readEnvFileMergeTrainMode's documented
// lenient behavior.
func TestResolveTrainModeUnrecognizedEnvFallsBackToOff(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("FABRIK_MERGE_TRAIN=garbage\n"), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}
	env := &Env{FabrikTestDir: dir}
	t.Setenv("E2E_TRAIN_MODE", "")

	if got := resolveTrainMode(t, env); got != "off" {
		t.Fatalf("resolveTrainMode() = %q, want %q (lenient .env fallback on garbage value)", got, "off")
	}
}

// TestNormalizeTrainMode exercises the pure validation logic resolveTrainMode
// delegates to for E2E_TRAIN_MODE — including the "invalid value" case,
// which can't be tested through resolveTrainMode itself: resolveTrainMode
// calls t.Fatalf on invalid input, and a failing t.Run subtest propagates
// its failure to the parent test, so there is no way to observe "the fatal
// path fired as designed" without also failing the overall test run.
func TestNormalizeTrainMode(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"on", "on", false},
		{"ON", "on", false},
		{" on ", "on", false},
		{"off", "off", false},
		{"OFF", "off", false},
		{"sideways", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := normalizeTrainMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeTrainMode(%q) = %q, nil; want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeTrainMode(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("normalizeTrainMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
