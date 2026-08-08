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

// TestBuildClaudeEnv_GHHost_EmittedWhenConfigured covers FR-6: when a GHES
// host is configured, buildClaudeEnv must emit GH_HOST so a stage worker's
// `gh` invocations (e.g. fabrik-validate's Pre-Completion Gate) target the
// same instance as the engine rather than silently hitting github.com.
func TestBuildClaudeEnv_GHHost_EmittedWhenConfigured(t *testing.T) {
	old := claudeGHHost
	claudeGHHost = "github.example.com"
	t.Cleanup(func() { claudeGHHost = old })

	got := constructedEnv(nil, InvokeOptions{})
	if !containsExact(got, "GH_HOST=github.example.com") {
		t.Errorf("expected GH_HOST=github.example.com in env, got %v", got)
	}
}

// TestBuildClaudeEnv_GHHost_OmittedWhenNotConfigured covers FR-6's other
// half: absent GHES configuration, GH_HOST must not appear at all —
// byte-identical to pre-GHES behavior, not merely empty-valued.
func TestBuildClaudeEnv_GHHost_OmittedWhenNotConfigured(t *testing.T) {
	old := claudeGHHost
	claudeGHHost = ""
	t.Cleanup(func() { claudeGHHost = old })

	got := constructedEnv(nil, InvokeOptions{})
	for _, kv := range got {
		if strings.HasPrefix(kv, "GH_HOST=") || kv == "GH_HOST" {
			t.Errorf("expected no GH_HOST entry when unconfigured, found %q in %v", kv, got)
		}
	}
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

	// Regression: the ordering-dependent collision case R7 explicitly permits
	// (a passthrough entry and the FABRIK_ANTHROPIC_API_KEY translation both
	// naming ANTHROPIC_API_KEY) was previously untested — buildClaudeEnv emits
	// the passthrough re-add before the translation, so overrides ends up with
	// two ANTHROPIC_API_KEY entries; mergeEnv does not deduplicate them (by
	// design — its own doc comment already relies on Cmd.Env's last-occurrence-
	// wins resolution rather than special-casing this), so the *last* one in
	// the constructed slice is what actually reaches the subprocess. This pins
	// that ordering: a future refactor that moves the translation block before
	// the passthrough loop (or vice versa) would silently invert which value
	// wins, and this test would catch it. See also
	// TestInvokeClaude_AnthropicAPIKeyOptInWinsOverPassthrough for the
	// subprocess-level confirmation that this is what os/exec actually does.
	t.Run("passthrough names ANTHROPIC_API_KEY too: translation still wins over the passthrough re-add (R7)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicAPIKey = "sk-fabrik-wins"
		claudeAnthropicEnvPassthrough = []string{"ANTHROPIC_API_KEY"}
		baseEnv := []string{"ANTHROPIC_API_KEY=sk-passthrough-loses"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if lastValue(got, "ANTHROPIC_API_KEY") != "sk-fabrik-wins" {
			t.Errorf("expected the last ANTHROPIC_API_KEY entry (what os/exec honors) to be the FABRIK_-prefixed value, got %v", got)
		}
	})
}

// lastValue returns the value of the *last* "KEY=VALUE" entry in env matching
// key, mirroring Go's os/exec duplicate-Cmd.Env-key resolution (last
// occurrence wins) — see mergeEnv's doc comment. mergeEnv deliberately does
// not deduplicate its output, so a caller-level check for "the value" must
// replicate that same last-occurrence semantics rather than assume uniqueness.
func lastValue(env []string, key string) string {
	prefix := key + "="
	var val string
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
		}
	}
	return val
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

	// Regression: naming a Fabrik-internal control variable *inside* its own
	// passthrough list must not forward that variable's ambient value. mergeEnv's
	// bare removal sentinel (emitted unconditionally by buildClaudeEnv for both
	// control vars) only strips a key from base — it cannot retract an earlier
	// "KEY=VALUE" entry already present in the same overrides slice, so without an
	// explicit skip in the passthrough re-add loop, this self-referential case
	// would leak the control variable's own value straight through, violating R8/R17.
	t.Run("naming FABRIK_ANTHROPIC_API_KEY inside its own passthrough list does not leak it (R8)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"FABRIK_ANTHROPIC_API_KEY"}
		baseEnv := []string{"FABRIK_ANTHROPIC_API_KEY=leaked-secret"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if containsPrefix(got, "FABRIK_ANTHROPIC_API_KEY=") {
			t.Errorf("expected FABRIK_ANTHROPIC_API_KEY never forwarded even when self-named in passthrough, got %v", got)
		}
	})

	t.Run("naming FABRIK_ANTHROPIC_ENV_PASSTHROUGH inside its own passthrough list does not leak it (R17)", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		claudeAnthropicEnvPassthrough = []string{"FABRIK_ANTHROPIC_ENV_PASSTHROUGH"}
		baseEnv := []string{"FABRIK_ANTHROPIC_ENV_PASSTHROUGH=FABRIK_ANTHROPIC_ENV_PASSTHROUGH"}
		got := constructedEnv(baseEnv, InvokeOptions{})
		if containsPrefix(got, "FABRIK_ANTHROPIC_ENV_PASSTHROUGH=") {
			t.Errorf("expected FABRIK_ANTHROPIC_ENV_PASSTHROUGH never forwarded even when self-named in passthrough, got %v", got)
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

	// Regression (Validate-stage review finding): naming one of Fabrik's own
	// computed override keys in FABRIK_ANTHROPIC_ENV_PASSTHROUGH must not let
	// an ambient value silently win. mergeEnv appends every "KEY=VALUE"
	// override in order and never dedupes within a single overrides slice, so
	// a second, later entry for an already-emitted key (e.g. GH_TOKEN) would
	// have won per os/exec's last-occurrence-wins semantics, letting a stale
	// or attacker-controlled ambient value silently override Fabrik's own
	// computed token/effort-level/invocation-fact value.
	t.Run("naming a Fabrik-owned override key does not let the ambient value override it", func(t *testing.T) {
		resetAnthropicEnvVars(t)
		old := claudeGHToken
		claudeGHToken = "fabrik-computed-token"
		t.Cleanup(func() { claudeGHToken = old })
		claudeAnthropicEnvPassthrough = []string{"GH_TOKEN", "CLAUDE_CODE_EFFORT_LEVEL", "FABRIK_ISSUE"}
		baseEnv := []string{
			"GH_TOKEN=stale-ambient-token",
			"CLAUDE_CODE_EFFORT_LEVEL=low",
			"FABRIK_ISSUE=99999",
		}
		got := constructedEnv(baseEnv, InvokeOptions{})
		for _, want := range []string{"GH_TOKEN=fabrik-computed-token", "CLAUDE_CODE_EFFORT_LEVEL=high", "FABRIK_ISSUE=1346"} {
			if !containsExact(got, want) {
				t.Errorf("expected Fabrik's own value %q to survive, got %v", want, got)
			}
		}
		for _, unwanted := range []string{"GH_TOKEN=stale-ambient-token", "CLAUDE_CODE_EFFORT_LEVEL=low", "FABRIK_ISSUE=99999"} {
			if containsExact(got, unwanted) {
				t.Errorf("expected ambient value %q to never appear, got %v", unwanted, got)
			}
		}
		// Confirm there's exactly one entry per key — not just that Fabrik's
		// value happens to be found somewhere in a duplicated slice.
		for _, key := range []string{"GH_TOKEN=", "CLAUDE_CODE_EFFORT_LEVEL=", "FABRIK_ISSUE="} {
			count := 0
			for _, kv := range got {
				if strings.HasPrefix(kv, key) {
					count++
				}
			}
			if count != 1 {
				t.Errorf("expected exactly one %s entry, got %d in %v", key, count, got)
			}
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
