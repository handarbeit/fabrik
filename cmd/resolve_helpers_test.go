package cmd

import (
	"testing"
	"time"
)

// Focused tests for the resolveBool/resolveInt/resolveDuration helpers
// extracted out of Execute (#1029). These call the helpers directly instead
// of driving all of Execute (flag parsing, stage loading, engine construction)
// per env-var precedence case, which is exactly the isolated testability the
// decomposition enables.

func TestResolveBool_EnvTruthyValues(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"garbage", false},
	}
	for _, c := range cases {
		t.Setenv("FABRIK_TEST_BOOL", c.env)
		if got := resolveBool("FABRIK_TEST_BOOL", false); got != c.want {
			t.Errorf("resolveBool(%q, false) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestResolveBool_FallsBackToProjectConfig(t *testing.T) {
	t.Setenv("FABRIK_TEST_BOOL_UNSET", "")
	if got := resolveBool("FABRIK_TEST_BOOL_UNSET", true); got != true {
		t.Errorf("resolveBool with unset env = %v, want fallback true", got)
	}
	if got := resolveBool("FABRIK_TEST_BOOL_UNSET", false); got != false {
		t.Errorf("resolveBool with unset env = %v, want fallback false", got)
	}
}

func TestResolveInt_ValidEnvOverridesCurrent(t *testing.T) {
	t.Setenv("FABRIK_TEST_INT", "42")
	if got := resolveInt(0, "FABRIK_TEST_INT", "", 5); got != 42 {
		t.Errorf("resolveInt = %d, want 42", got)
	}
}

func TestResolveInt_InvalidEnvKeepsCurrentAndWarns(t *testing.T) {
	t.Setenv("FABRIK_TEST_INT", "-1")
	if got := resolveInt(7, "FABRIK_TEST_INT", "", 5); got != 7 {
		t.Errorf("resolveInt with invalid env = %d, want unchanged current 7", got)
	}
	t.Setenv("FABRIK_TEST_INT", "not-a-number")
	if got := resolveInt(7, "FABRIK_TEST_INT", "", 5); got != 7 {
		t.Errorf("resolveInt with non-numeric env = %d, want unchanged current 7", got)
	}
}

func TestResolveInt_UnsetEnvKeepsCurrent(t *testing.T) {
	t.Setenv("FABRIK_TEST_INT_UNSET", "")
	if got := resolveInt(3, "FABRIK_TEST_INT_UNSET", "", 5); got != 3 {
		t.Errorf("resolveInt with unset env = %d, want unchanged current 3", got)
	}
}

func TestResolveDuration_EnvOverridesCurrent(t *testing.T) {
	t.Setenv("FABRIK_TEST_DURATION", "20s")
	if got := resolveDuration("10s", "FABRIK_TEST_DURATION"); got != "20s" {
		t.Errorf("resolveDuration = %q, want %q", got, "20s")
	}
}

func TestResolveDuration_UnsetEnvKeepsCurrent(t *testing.T) {
	t.Setenv("FABRIK_TEST_DURATION_UNSET", "")
	if got := resolveDuration("10s", "FABRIK_TEST_DURATION_UNSET"); got != "10s" {
		t.Errorf("resolveDuration with unset env = %q, want unchanged %q", got, "10s")
	}
}

// TestDrainDeadline mirrors the shape of killGraceSigInt/killGraceSigTerm's
// own (untested-directly, but identically shaped) validation logic —
// default, valid override, and invalid/negative falling back to the default
// with a warning (#1393/ADR-1393 R4). Unlike kill_grace, 0s is NOT a legal
// "skip" sentinel here: a clean stop has no unbounded-wait mode, so 0s also
// falls back to the default.
func TestDrainDeadline(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty uses default", "", 30 * time.Second},
		{"valid override", "45s", 45 * time.Second},
		{"valid override minutes", "2m", 2 * time.Minute},
		{"zero falls back to default", "0s", 30 * time.Second},
		{"negative falls back to default", "-5s", 30 * time.Second},
		{"invalid syntax falls back to default", "not-a-duration", 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := drainDeadline(c.in); got != c.want {
				t.Errorf("drainDeadline(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
