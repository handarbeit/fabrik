package pruefer

import (
	"fmt"
	"strings"
)

// Startup validation for the direct and tailscale event sources (#2476,
// ADR 2476). The engine has equivalent refusals bound to its own Config
// (engine/github_app_auth.go); Pruefer cannot import engine (ADR 1113), so the
// rules are restated here. Pruefer is always App-authenticated, so the
// engine's "needs App auth" rule has no counterpart.
//
// Every message names a variable or a flag, never a value: a secret or auth key
// must not reach a log line or an error through here. envNameRE (review_env.go)
// refuses a value where the *name* of a variable belongs, catching the commonest
// mistake — pasting the secret itself — before it can be echoed anywhere.

// validEventSource reports whether s is a recognised event_source value.
func validEventSource(s string) bool {
	switch s {
	case EventSourcePoll, EventSourceHookdeck, EventSourceDirect, EventSourceTailscale:
		return true
	}
	return false
}

// isAppWebhookSource reports whether s is one of the two receivers built on
// internal/appwebhook.
func isAppWebhookSource(s string) bool {
	return s == EventSourceDirect || s == EventSourceTailscale
}

// validateAppSourceStatic is the part of the refusal rules that needs no
// environment: it runs at config load, so a bad config fails before any
// network or filesystem work. A poll or hookdeck config is never touched.
func validateAppSourceStatic(cfg Config) error {
	if !isAppWebhookSource(cfg.EventSource) {
		return nil
	}
	switch cfg.EventSource {
	case EventSourceDirect:
		if strings.TrimSpace(cfg.DirectListen) == "" {
			return fmt.Errorf("event_source: direct requires direct_listen (--direct-listen / PRUEFER_DIRECT_LISTEN) — " +
				"the host:port the daemon listens on, e.g. 127.0.0.1:8787; point your tunnel or proxy at it")
		}
	case EventSourceTailscale:
		if strings.TrimSpace(cfg.TailscaleHostname) == "" {
			return fmt.Errorf("event_source: tailscale requires tailscale_hostname (--tailscale-hostname / PRUEFER_TAILSCALE_HOSTNAME) — " +
				"the tailnet machine name for this daemon, e.g. pruefer-acme; use a different name for each daemon")
		}
		if strings.TrimSpace(cfg.TailscaleStateDir) == "" {
			return fmt.Errorf("tailscale_state_dir (--tailscale-state-dir / PRUEFER_TAILSCALE_STATE_DIR) must not be empty — " +
				"it holds the node identity that lets a restart skip the auth key")
		}
		if !envNameRE.MatchString(cfg.TailscaleAuthKeyEnv) {
			return fmt.Errorf("tailscale_auth_key_env (--tailscale-auth-key-env / PRUEFER_TAILSCALE_AUTH_KEY_ENV) must be the NAME " +
				"of an environment variable (letters, digits, underscore), not the key itself")
		}
	}
	if p := cfg.AppWebhookPath; p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("app_webhook_path (--app-webhook-path / PRUEFER_APP_WEBHOOK_PATH) must be an absolute URL path starting with \"/\", e.g. %q", DefaultAppWebhookPath)
	}
	if !envNameRE.MatchString(cfg.AppWebhookSecretEnv) {
		return fmt.Errorf("app_webhook_secret_env (--app-webhook-secret-env / PRUEFER_APP_WEBHOOK_SECRET_ENV) must be the NAME " +
			"of an environment variable (letters, digits, underscore), not the secret itself")
	}
	return nil
}

// RefuseAppSourceMisconfigured is the runtime half: the checks that need the
// resolved environment. secretValue and authKeyValue are tested for emptiness
// only. stateExists is whether the Tailscale state directory already holds a
// logged-in node (appwebhook.StateExists), in which case the auth key is not
// needed. A non-App event source is never refused here.
func RefuseAppSourceMisconfigured(cfg Config, secretValue, authKeyValue string, stateExists bool) error {
	if !isAppWebhookSource(cfg.EventSource) {
		return nil
	}
	if err := validateAppSourceStatic(cfg); err != nil {
		return err
	}
	if secretValue == "" {
		return fmt.Errorf("event_source: %s requires the App's webhook secret in $%s — set that variable "+
			"(or name another with app_webhook_secret_env / --app-webhook-secret-env) to the secret configured "+
			"on the GitHub App", cfg.EventSource, cfg.AppWebhookSecretEnv)
	}
	if cfg.EventSource == EventSourceTailscale && !stateExists && authKeyValue == "" {
		return fmt.Errorf("event_source: tailscale requires a tagged, pre-approved auth key in $%s on the first start "+
			"(the state directory %s holds no logged-in node yet) — set that variable (or name another with "+
			"tailscale_auth_key_env / --tailscale-auth-key-env). It is not needed once the node has joined",
			cfg.TailscaleAuthKeyEnv, cfg.TailscaleStateDir)
	}
	return nil
}
