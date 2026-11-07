package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/config"
)

func TestCollectLegacyHookdeck_NothingPresent(t *testing.T) {
	scrub, ignored := collectLegacyHookdeck("", "", map[string]bool{}, []string{"PATH=/bin", "FABRIK_TOKEN=x"}, config.LegacyHookdeck{})
	if len(scrub) != 0 || len(ignored) != 0 {
		t.Fatalf("scrub=%v ignored=%v, want both empty", scrub, ignored)
	}
	if n := legacyHookdeckNotice(ignored); n != "" {
		t.Errorf("notice = %q, want none", n)
	}
}

// Every route contributes: the removed flag, the FABRIK_HOOKDECK_* variables
// and config.yaml. The same setting in two places is listed once, and the
// variable names each points at are all scrubbed (not only the winner).
func TestCollectLegacyHookdeck_AllRoutesOnceAndScrubNames(t *testing.T) {
	environ := []string{
		"FABRIK_HOOKDECK_API_KEY_ENV=ENV_KEY",
		"FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV=ENV_SECRET",
		"FABRIK_HOOKDECK_API_KEY=leftover-secret-value",
		"PATH=/bin",
	}
	yaml := config.LegacyHookdeck{
		APIKeyEnv:        "YAML_KEY",
		WebhookSecretEnv: "ENV_SECRET", // duplicate of the env route
		Present:          []string{"hookdeck_api_key_env", "hookdeck_webhook_secret_env"},
	}
	scrub, ignored := collectLegacyHookdeck("FLAG_KEY", "", map[string]bool{"hookdeck-api-key-env": true}, environ, yaml)

	wantScrub := []string{"FLAG_KEY", "ENV_KEY", "ENV_SECRET", "YAML_KEY"}
	if !reflect.DeepEqual(scrub, wantScrub) {
		t.Errorf("scrub = %v, want %v", scrub, wantScrub)
	}
	wantIgnored := []string{
		"--hookdeck-api-key-env", "FABRIK_HOOKDECK_API_KEY", "FABRIK_HOOKDECK_API_KEY_ENV",
		"FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV", "hookdeck_api_key_env", "hookdeck_webhook_secret_env",
	}
	if !reflect.DeepEqual(ignored, wantIgnored) {
		t.Errorf("ignored = %v, want %v", ignored, wantIgnored)
	}

	n := legacyHookdeckNotice(ignored)
	if strings.Count(n, "\n") != 1 || !strings.HasPrefix(n, "[startup] notice:") {
		t.Errorf("notice must be exactly one [startup] notice line, got %q", n)
	}
	for _, leaked := range []string{"leftover-secret-value", "ENV_KEY", "YAML_KEY", "FLAG_KEY"} {
		if strings.Contains(n, leaked) {
			t.Errorf("notice leaks %q: %q", leaked, n)
		}
	}
	if !strings.Contains(n, "Migrating from Hookdeck") {
		t.Errorf("notice lacks the migration pointer: %q", n)
	}
}

func TestCollectLegacyHookdeck_FlagSetEmptyStillReported(t *testing.T) {
	scrub, ignored := collectLegacyHookdeck("", "", map[string]bool{"hookdeck-webhook-secret-env": true}, nil, config.LegacyHookdeck{})
	if len(scrub) != 0 || !reflect.DeepEqual(ignored, []string{"--hookdeck-webhook-secret-env"}) {
		t.Errorf("scrub=%v ignored=%v", scrub, ignored)
	}
}
