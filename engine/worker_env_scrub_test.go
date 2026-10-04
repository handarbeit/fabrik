package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/workerenv"
)

// daemonEnv is a worker-shaped base env: every R1 variable, the ADR-1846
// credential-helper entries, GH_TOKEN and an unrelated variable.
func daemonEnv() []string {
	return []string{
		"FABRIK_GITHUB_APP_PRIVATE_KEY_PATH=/keys/app.pem",
		"FABRIK_GITHUB_APP_ID=1",
		"FABRIK_GITHUB_APP_INSTALLATION_ID=2",
		"FABRIK_GITHUB_WEBHOOK_SECRET=whsec",
		"FABRIK_TOKEN=pat",
		"FABRIK_REVIEWER_TOKEN=reviewer",
		"FABRIK_WORKER_ENV_PASSTHROUGH=FABRIK_TOKEN",
		"FABRIK_POLL=30s",
		"HOOKDECK_API_KEY=hd",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_1=!helper",
		"GH_TOKEN=ambient-stale",
		"UNRELATED=keep",
	}
}

func resetWorkerEnv(t *testing.T) {
	t.Helper()
	resetAnthropicEnvVars(t) // also saves/restores claudeWorkerEnv
	prevTok, prevFn, prevHost := claudeGHToken, claudeGHTokenOverrideFn, claudeGHHost
	t.Cleanup(func() { claudeGHToken, claudeGHTokenOverrideFn, claudeGHHost = prevTok, prevFn, prevHost })
	claudeGHToken, claudeGHTokenOverrideFn, claudeGHHost = "", nil, ""
}

func hasKey(env []string, key string) bool {
	for _, kv := range env {
		if kv == key || strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

var r1Vars = []string{
	"FABRIK_GITHUB_APP_PRIVATE_KEY_PATH", "FABRIK_GITHUB_APP_ID", "FABRIK_GITHUB_APP_INSTALLATION_ID",
	"FABRIK_GITHUB_WEBHOOK_SECRET", "FABRIK_TOKEN", "FABRIK_REVIEWER_TOKEN",
}

// TestBuildClaudeEnv_FabrikSecretScrub_AppMode is the #2027 R1/R2 acceptance:
// none of the R1 variables reaches the worker, while the App token, the
// ADR-1846 helper entries and the engine's own worker facts do.
func TestBuildClaudeEnv_FabrikSecretScrub_AppMode(t *testing.T) {
	resetWorkerEnv(t)
	claudeGHTokenOverrideFn = func() string { return "installation-token" }
	claudeGHHost = "ghe.example.com"

	got := constructedEnv(daemonEnv(), InvokeOptions{FabrikRoot: "/root", PRNumber: 7})

	for _, k := range append(append([]string{}, r1Vars...), "FABRIK_WORKER_ENV_PASSTHROUGH", "FABRIK_POLL", "HOOKDECK_API_KEY") {
		if hasKey(got, k) {
			t.Errorf("%s reached the worker env", k)
		}
	}
	for _, want := range []string{
		"GH_TOKEN=installation-token", "GITHUB_TOKEN=installation-token", "GH_HOST=ghe.example.com",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.https://github.com.helper", "GIT_CONFIG_VALUE_1=!helper",
		"FABRIK_ISSUE=1346", "FABRIK_REPO=acme/widgets", "FABRIK_WORKTREE=/work", "FABRIK_ROOT=/root", "FABRIK_PR=7",
		"UNRELATED=keep",
	} {
		if !containsExact(got, want) {
			t.Errorf("worker env missing %q", want)
		}
	}
	if containsExact(got, "GH_TOKEN=ambient-stale") {
		t.Error("the engine's token must replace the ambient GH_TOKEN")
	}
}

// TestBuildClaudeEnv_FabrikSecretScrub_NonVacuous proves the assertions above
// bite: with the scrub's sentinels withheld the same base env leaks every R1
// variable through mergeEnv.
func TestBuildClaudeEnv_FabrikSecretScrub_NonVacuous(t *testing.T) {
	resetWorkerEnv(t)
	base := daemonEnv()
	overrides := buildClaudeEnv(envTestStage(), envTestIssue(), "/work", InvokeOptions{}, base)
	sentinel := map[string]bool{}
	for _, n := range workerenv.Sentinels(workerenv.Resolved{}) {
		sentinel[n] = true
	}
	var without []string
	for _, kv := range overrides {
		if !strings.Contains(kv, "=") && sentinel[kv] && !strings.HasPrefix(kv, "FABRIK_ANTHROPIC_") {
			continue
		}
		without = append(without, kv)
	}
	leaked := mergeEnv(base, without)
	for _, k := range r1Vars {
		if !hasKey(leaked, k) {
			t.Errorf("%s was not leaked with the scrub removed — the scrub test would be vacuous", k)
		}
	}
	clean := mergeEnv(base, overrides)
	for _, k := range r1Vars {
		if hasKey(clean, k) {
			t.Errorf("%s leaked with the scrub in place", k)
		}
	}
}

func TestBuildClaudeEnv_FabrikSecretScrub_PATMode(t *testing.T) {
	resetWorkerEnv(t)
	claudeGHToken = "configured-pat"

	got := constructedEnv(daemonEnv(), InvokeOptions{})
	if !containsExact(got, "GH_TOKEN=configured-pat") || !containsExact(got, "GITHUB_TOKEN=configured-pat") {
		t.Errorf("PAT mode must still deliver the configured token: %v", got)
	}
	if hasKey(got, "FABRIK_TOKEN") {
		t.Error("FABRIK_TOKEN must be scrubbed in PAT mode")
	}
}

func TestBuildClaudeEnv_FabrikSecretScrub_OptIn(t *testing.T) {
	resetWorkerEnv(t)
	claudeGHTokenOverrideFn = func() string { return "installation-token" }

	t.Run("not named: absent", func(t *testing.T) {
		if hasKey(constructedEnv(daemonEnv(), InvokeOptions{}), "FABRIK_REVIEWER_TOKEN") {
			t.Error("scrubbed variable reached the worker without the opt-in")
		}
	})

	t.Run("named: admitted, others still scrubbed, opt-in itself and GH_TOKEN untouched", func(t *testing.T) {
		claudeWorkerEnv = workerenv.Resolve(nil, "FABRIK_REVIEWER_TOKEN, GH_TOKEN, PATH, FABRIK_WORKER_ENV_PASSTHROUGH")
		got := constructedEnv(daemonEnv(), InvokeOptions{})
		if !containsExact(got, "FABRIK_REVIEWER_TOKEN=reviewer") {
			t.Errorf("named variable not admitted: %v", got)
		}
		if hasKey(got, "FABRIK_TOKEN") {
			t.Error("an unnamed scrubbed variable leaked")
		}
		if hasKey(got, "FABRIK_WORKER_ENV_PASSTHROUGH") {
			t.Error("the opt-in variable itself must never be forwarded")
		}
		if containsExact(got, "GH_TOKEN=ambient-stale") || !containsExact(got, "GH_TOKEN=installation-token") {
			t.Error("the opt-in must not be able to override the engine-set GH_TOKEN")
		}
	})
}

func TestBuildClaudeEnv_FabrikSecretScrub_HookdeckNames(t *testing.T) {
	resetWorkerEnv(t)
	base := append(daemonEnv(), "MY_HD_KEY=k", "MY_HD_SECRET=s", "OTHER=o")

	claudeWorkerEnv = workerenv.Resolve([]string{"MY_HD_KEY", "MY_HD_SECRET"}, "")
	got := constructedEnv(base, InvokeOptions{})
	for _, k := range []string{"MY_HD_KEY", "MY_HD_SECRET", "HOOKDECK_API_KEY", "FABRIK_GITHUB_WEBHOOK_SECRET"} {
		if hasKey(got, k) {
			t.Errorf("%s (Hookdeck secret) reached the worker", k)
		}
	}
	if !containsExact(got, "OTHER=o") {
		t.Error("unrelated variable must survive")
	}

	// An operator value naming a protected variable can never strip it.
	claudeWorkerEnv = workerenv.Resolve([]string{"PATH", "GH_TOKEN"}, "")
	got = constructedEnv(append(base, "PATH=/bin"), InvokeOptions{})
	if !containsExact(got, "PATH=/bin") {
		t.Error("a Hookdeck name of PATH must not scrub PATH")
	}
}

func TestLogWorkerEnvOptIn(t *testing.T) {
	var buf bytes.Buffer
	logWorkerEnvOptIn(workerenv.Resolve([]string{"PATH"}, "FABRIK_TOKEN,GH_TOKEN"), &buf)
	out := buf.String()
	for _, want := range []string{"passes FABRIK_TOKEN through", "ignored", "GH_TOKEN", "not scrubbed", "PATH"} {
		if !strings.Contains(out, want) {
			t.Errorf("notice missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	logWorkerEnvOptIn(workerenv.Resolved{}, &buf)
	if buf.Len() != 0 {
		t.Errorf("no opt-in must log nothing, got %q", buf.String())
	}
}
