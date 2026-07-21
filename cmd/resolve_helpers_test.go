package cmd

import "testing"

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
