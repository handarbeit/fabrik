package pruefer

import (
	"strings"
	"testing"
)

// FR-003 / SC-005: the variables the direct and tailscale event sources add —
// their config names, the variables those name, and the secret and auth-key
// values — never reach the review subprocess. The review environment is an
// allowlist, so this holds by default; the test pins it, under the default
// names and under custom ones an operator chose.
func TestReviewEnvExcludesAppSourceVariables(t *testing.T) {
	const (
		secretVal = "whsec-VALUE-must-not-reach-review"
		keyVal    = "tskey-auth-VALUE-must-not-reach-review"
	)
	names := map[string]string{
		// The config surface (flag > env > YAML; these are the env spellings).
		"PRUEFER_EVENT_SOURCE":             "tailscale",
		"PRUEFER_DIRECT_LISTEN":            "127.0.0.1:8787",
		"PRUEFER_APP_WEBHOOK_PATH":         "/webhook",
		"PRUEFER_APP_WEBHOOK_SECRET_ENV":   "MY_HOOK_SECRET",
		"PRUEFER_TAILSCALE_HOSTNAME":       "pruefer-acme",
		"PRUEFER_TAILSCALE_AUTH_KEY_ENV":   "MY_TS_KEY",
		"PRUEFER_TAILSCALE_STATE_DIR":      "/var/lib/pruefer/ts",
		"PRUEFER_REPLAY_FAILED_DELIVERIES": "true",
		// The variables those names point at, default and custom, with values.
		DefaultAppWebhookSecretEnv: secretVal,
		DefaultTailscaleAuthKeyEnv: keyVal,
		"MY_HOOK_SECRET":           secretVal,
		"MY_TS_KEY":                keyVal,
	}
	environ := []string{"PATH=/usr/bin", "HOME=/home/x", "LC_ALL=C"}
	for k, v := range names {
		environ = append(environ, k+"="+v)
	}

	env := reviewProcessEnv(environ, ReviewRequest{})
	if err := checkReviewEnvScrubbed(env, nil); err != nil {
		t.Fatalf("review environment is not scrubbed: %v", err)
	}
	joined := strings.Join(env, "\n")
	for name := range names {
		if strings.Contains(joined, name+"=") {
			t.Errorf("%s reached the review environment", name)
		}
	}
	for _, v := range []string{secretVal, keyVal} {
		if strings.Contains(joined, v) {
			t.Errorf("a secret value reached the review environment")
		}
	}
	// The scrub did not simply empty the environment.
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("allowlisted PATH was dropped: %v", env)
	}
}

// An operator cannot forward the new names through the passthrough hatch: the
// PRUEFER_ prefix is never forwarded, and a custom secret variable that carries
// no such prefix is not a name the daemon knows to protect, so it is refused by
// being absent from the allowlist unless explicitly passed through — which the
// operator then owns. This pins the prefix half.
func TestReviewEnvPassthroughRefusesAppSourceNames(t *testing.T) {
	for _, name := range []string{DefaultAppWebhookSecretEnv, DefaultTailscaleAuthKeyEnv, "PRUEFER_DIRECT_LISTEN"} {
		environ := []string{
			ReviewEnvPassthroughVar + "=" + name,
			name + "=value",
		}
		env := reviewProcessEnv(environ, ReviewRequest{})
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				t.Errorf("%s was forwarded through the passthrough hatch", name)
			}
		}
	}
}
