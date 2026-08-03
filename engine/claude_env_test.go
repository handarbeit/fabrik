package engine

import (
	"strings"
	"testing"
)

// TestMergeEnv_RemovalSentinel covers R1: a bare "KEY" override entry (no
// "=") strips KEY from base without appending a replacement, while every
// existing "KEY=VALUE" add/shadow/last-wins behavior is unchanged.
func TestMergeEnv_RemovalSentinel(t *testing.T) {
	t.Run("bare key removes a matching base entry and appends nothing", func(t *testing.T) {
		base := []string{"FOO=bar", "ANTHROPIC_API_KEY=leaked", "BAZ=qux"}
		got := mergeEnv(base, []string{"ANTHROPIC_API_KEY"})
		for _, kv := range got {
			if strings.HasPrefix(kv, "ANTHROPIC_API_KEY") {
				t.Errorf("expected ANTHROPIC_API_KEY entirely absent, got %q in %v", kv, got)
			}
		}
		if !containsExact(got, "FOO=bar") || !containsExact(got, "BAZ=qux") {
			t.Errorf("expected unrelated base entries preserved, got %v", got)
		}
	})

	t.Run("bare key with no matching base entry is a no-op", func(t *testing.T) {
		base := []string{"FOO=bar"}
		got := mergeEnv(base, []string{"NEVER_SET"})
		if len(got) != 1 || got[0] != "FOO=bar" {
			t.Errorf("expected base unchanged, got %v", got)
		}
		for _, kv := range got {
			if kv == "NEVER_SET" {
				t.Errorf("expected the removal sentinel itself never appended, got %v", got)
			}
		}
	})

	t.Run("bare removal token coexisting with a normal override for the same key: only KEY=VALUE form appended", func(t *testing.T) {
		base := []string{"ANTHROPIC_API_KEY=leaked"}
		got := mergeEnv(base, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY=explicit"})
		count := 0
		for _, kv := range got {
			if kv == "ANTHROPIC_API_KEY=explicit" {
				count++
			}
			if kv == "ANTHROPIC_API_KEY=leaked" {
				t.Errorf("expected leaked base value stripped, got %v", got)
			}
		}
		if count != 1 {
			t.Errorf("expected exactly one ANTHROPIC_API_KEY=explicit entry, got %v", got)
		}
	})

	t.Run("pre-existing add/shadow/last-wins behavior unchanged", func(t *testing.T) {
		base := []string{"FABRIK_REPO=stale-ambient/repo", "UNRELATED=1"}
		got := mergeEnv(base, []string{"FABRIK_REPO="})
		if !containsExact(got, "FABRIK_REPO=") {
			t.Errorf("expected FABRIK_REPO= (empty, but present) in %v", got)
		}
		if containsPrefix(got, "FABRIK_REPO=stale-ambient") {
			t.Errorf("expected stale ambient value stripped, got %v", got)
		}
		if !containsExact(got, "UNRELATED=1") {
			t.Errorf("expected unrelated base entries preserved, got %v", got)
		}
	})

	t.Run("empty overrides returns base unchanged", func(t *testing.T) {
		base := []string{"FOO=bar"}
		got := mergeEnv(base, nil)
		if len(got) != 1 || got[0] != "FOO=bar" {
			t.Errorf("expected base returned unchanged, got %v", got)
		}
	})

	t.Run("add a brand-new key not present in base", func(t *testing.T) {
		base := []string{"FOO=bar"}
		got := mergeEnv(base, []string{"NEW=value"})
		if !containsExact(got, "FOO=bar") || !containsExact(got, "NEW=value") {
			t.Errorf("expected both entries present, got %v", got)
		}
	})
}

func containsExact(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func containsPrefix(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}
