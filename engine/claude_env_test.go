package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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

func envTestStage() *stages.Stage {
	return &stages.Stage{Name: "Implement", Prompt: "do it", Completion: stages.CompletionCriteria{Type: "claude"}}
}

func envTestIssue() gh.ProjectItem {
	return gh.ProjectItem{Number: 1346, Repo: "acme/widgets", Title: "env scrub test"}
}

// resetAnthropicEnvVars resets the package-level resolved
// FABRIK_ANTHROPIC_API_KEY/FABRIK_ANTHROPIC_ENV_PASSTHROUGH state and
// restores it after the test, mirroring the existing claudeGHToken
// t.Cleanup precedent (invoke_claude_test.go).
func resetAnthropicEnvVars(t *testing.T) {
	t.Helper()
	prevKey, prevPassthrough := claudeAnthropicAPIKey, claudeAnthropicEnvPassthrough
	claudeAnthropicAPIKey = ""
	claudeAnthropicEnvPassthrough = nil
	t.Cleanup(func() {
		claudeAnthropicAPIKey = prevKey
		claudeAnthropicEnvPassthrough = prevPassthrough
	})
}

// constructedEnv returns the final subprocess environment mergeEnv would
// hand to cmd.Env for baseEnv + buildClaudeEnv's overrides — the same
// composition runClaude performs.
func constructedEnv(baseEnv []string, opts InvokeOptions) []string {
	overrides := buildClaudeEnv(envTestStage(), envTestIssue(), "/work", opts, baseEnv)
	return mergeEnv(baseEnv, overrides)
}

// TestBuildClaudeEnv_AnthropicNamespaceScrub covers R2-R5: every inherited
// ANTHROPIC_*-prefixed variable and every enumerated CLAUDE_CODE_* auth
// selector is scrubbed by default; a variable that merely contains (but does
// not start with) a scrubbed name survives untouched; Fabrik's own emitted
// variables are unaffected.
func TestBuildClaudeEnv_AnthropicNamespaceScrub(t *testing.T) {
	resetAnthropicEnvVars(t)

	scrubbed := append([]string{"ANTHROPIC_API_KEY"}, claudeCodeAuthSelectors...)
	scrubbed = append(scrubbed,
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_UNIX_SOCKET",
		"ANTHROPIC_AWS_API_KEY", "ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_MODEL", "ANTHROPIC_CONFIG_DIR", // non-auth ANTHROPIC_* vars, also scrubbed by design
	)

	var baseEnv []string
	for _, key := range scrubbed {
		baseEnv = append(baseEnv, key+"=should-be-removed")
	}
	baseEnv = append(baseEnv, "FANTASY_ANTHROPIC_API_KEY=survives", "PATH=/usr/bin")

	got := constructedEnv(baseEnv, InvokeOptions{})

	for _, key := range scrubbed {
		if containsPrefix(got, key+"=") {
			t.Errorf("expected %s scrubbed, got it present in %v", key, got)
		}
	}
	if !containsExact(got, "FANTASY_ANTHROPIC_API_KEY=survives") {
		t.Errorf("expected FANTASY_ANTHROPIC_API_KEY (contains but doesn't start with ANTHROPIC_) to survive untouched, got %v", got)
	}
	if !containsExact(got, "PATH=/usr/bin") {
		t.Errorf("expected unrelated PATH untouched, got %v", got)
	}

	// R4: Fabrik's own emitted variables are present with correct values.
	for _, want := range []string{
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1",
		"CLAUDE_CODE_EFFORT_LEVEL=high",
		"FABRIK_ISSUE=1346",
		"FABRIK_REPO=acme/widgets",
		"FABRIK_WORKTREE=/work",
	} {
		if !containsExact(got, want) {
			t.Errorf("expected %q present after scrub (R4), got %v", want, got)
		}
	}
}

// TestBuildClaudeEnv_AnthropicAPIKeyOptIn covers R6-R9.
func TestBuildClaudeEnv_AnthropicAPIKeyOptIn(t *testing.T) {
	t.Run("unset: no ANTHROPIC_API_KEY reaches the subprocess regardless of ambient", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		baseEnv := []string{"ANTHROPIC_API_KEY=ambient-leak"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if containsPrefix(got, "ANTHROPIC_API_KEY=") {
			t.Errorf("expected no ANTHROPIC_API_KEY in %v", got)
		}
	})

	t.Run("set: ANTHROPIC_API_KEY present with exactly that value, FABRIK_ANTHROPIC_API_KEY never forwarded", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicAPIKey = "sk-fabrik-value"
		got := constructedEnv(nil, InvokeOptions{})
		if !containsExact(got, "ANTHROPIC_API_KEY=sk-fabrik-value") {
			t.Errorf("expected ANTHROPIC_API_KEY=sk-fabrik-value in %v", got)
		}
		if containsPrefix(got, "FABRIK_ANTHROPIC_API_KEY=") {
			t.Errorf("expected FABRIK_ANTHROPIC_API_KEY never forwarded, got %v", got)
		}
	})

	t.Run("ambient and FABRIK_ANTHROPIC_API_KEY differ: the FABRIK_-prefixed value wins (translation, not passthrough)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicAPIKey = "sk-fabrik-wins"
		baseEnv := []string{"ANTHROPIC_API_KEY=sk-ambient-loses"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if !containsExact(got, "ANTHROPIC_API_KEY=sk-fabrik-wins") {
			t.Errorf("expected the FABRIK_-prefixed value to win, got %v", got)
		}
		if containsExact(got, "ANTHROPIC_API_KEY=sk-ambient-loses") {
			t.Errorf("expected the ambient value gone, got %v", got)
		}
	})
}

// TestBuildClaudeEnv_AnthropicEnvPassthrough covers R14-R19.
func TestBuildClaudeEnv_AnthropicEnvPassthrough(t *testing.T) {
	t.Run("named variable is inherited from baseEnv into the result (R15)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"CLAUDE_CODE_USE_BEDROCK"}
		baseEnv := []string{"CLAUDE_CODE_USE_BEDROCK=1"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if !containsExact(got, "CLAUDE_CODE_USE_BEDROCK=1") {
			t.Errorf("expected passthrough variable inherited, got %v", got)
		}
	})

	t.Run("variable not named in the list is still scrubbed even when present (R16)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"CLAUDE_CODE_USE_BEDROCK"}
		baseEnv := []string{"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if containsPrefix(got, "CLAUDE_CODE_USE_VERTEX=") {
			t.Errorf("expected non-passthrough-named CLAUDE_CODE_USE_VERTEX still scrubbed, got %v", got)
		}
	})

	t.Run("FABRIK_ANTHROPIC_ENV_PASSTHROUGH itself is never forwarded (R17)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"CLAUDE_CODE_USE_BEDROCK"}
		baseEnv := []string{"FABRIK_ANTHROPIC_ENV_PASSTHROUGH=CLAUDE_CODE_USE_BEDROCK"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if containsPrefix(got, "FABRIK_ANTHROPIC_ENV_PASSTHROUGH=") {
			t.Errorf("expected FABRIK_ANTHROPIC_ENV_PASSTHROUGH never forwarded, got %v", got)
		}
	})

	t.Run("named variable absent from ambient environment is a no-op, not an error (R15)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"CLAUDE_CODE_USE_BEDROCK"}
		got := constructedEnv(nil, InvokeOptions{})
		if containsPrefix(got, "CLAUDE_CODE_USE_BEDROCK=") {
			t.Errorf("expected no CLAUDE_CODE_USE_BEDROCK entry when absent from ambient, got %v", got)
		}
	})

	t.Run("named variable outside the scrubbed namespace is a true no-op (R19)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"PATH"}
		baseEnv := []string{"PATH=/usr/bin"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		count := 0
		for _, kv := range got {
			if kv == "PATH=/usr/bin" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly one PATH=/usr/bin entry (same value in, same value out), got %v in %v", count, got)
		}
	})
}

func TestParseAnthropicEnvPassthrough(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty string", "", nil},
		{"whitespace only", "   ", nil},
		{"single name", "CLAUDE_CODE_USE_BEDROCK", []string{"CLAUDE_CODE_USE_BEDROCK"}},
		{"multiple names", "CLAUDE_CODE_USE_BEDROCK,AWS_ACCESS_KEY_ID", []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID"}},
		{"whitespace trimmed", " CLAUDE_CODE_USE_BEDROCK , AWS_ACCESS_KEY_ID ", []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID"}},
		{"leading/trailing/doubled commas dropped", ",CLAUDE_CODE_USE_BEDROCK,,AWS_ACCESS_KEY_ID,", []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAnthropicEnvPassthrough(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
