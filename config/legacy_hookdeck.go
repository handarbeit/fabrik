package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Legacy Hookdeck settings (ADR 2477). Hookdeck was removed in v0.0.92, but a
// stale config.yaml may still carry its two keys. They are no longer fields of
// ProjectConfig; this file is the one place that still reads them, for two
// reasons only: so the engine can report them once as ignored, and so the
// variables they name can still be scrubbed from worker environments. It is
// deletable together with the retained workerenv scrub names.

const (
	// LegacyHookdeckAPIKeyEnvKey / LegacyHookdeckWebhookSecretEnvKey are the
	// removed config.yaml keys.
	LegacyHookdeckAPIKeyEnvKey        = "hookdeck_api_key_env"
	LegacyHookdeckWebhookSecretEnvKey = "hookdeck_webhook_secret_env"
)

// legacyHookdeckKeys is excluded from UnrecognizedTopLevelKeys so a leftover
// key produces the single "ignored" notice rather than a config_key panel entry.
var legacyHookdeckKeys = map[string]bool{
	LegacyHookdeckAPIKeyEnvKey:        true,
	LegacyHookdeckWebhookSecretEnvKey: true,
}

// LegacyHookdeck holds the raw values of the removed Hookdeck keys.
type LegacyHookdeck struct {
	// APIKeyEnv / WebhookSecretEnv are the values (variable names) of
	// hookdeck_api_key_env / hookdeck_webhook_secret_env; "" when absent.
	APIKeyEnv        string
	WebhookSecretEnv string
	// Present lists the removed keys found in the file, in a stable order.
	Present []string
}

// LoadLegacyHookdeck reads the removed Hookdeck keys from .fabrik/config.yaml.
// An absent file returns the zero value. It never affects LoadProjectConfig.
func LoadLegacyHookdeck() (LegacyHookdeck, error) {
	data, err := os.ReadFile(".fabrik/config.yaml")
	if os.IsNotExist(err) {
		return LegacyHookdeck{}, nil
	}
	if err != nil {
		return LegacyHookdeck{}, fmt.Errorf("reading .fabrik/config.yaml: %w", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return LegacyHookdeck{}, fmt.Errorf("parsing .fabrik/config.yaml: %w", err)
	}
	var l LegacyHookdeck
	for _, k := range []string{LegacyHookdeckAPIKeyEnvKey, LegacyHookdeckWebhookSecretEnvKey} {
		v, ok := raw[k]
		if !ok {
			continue
		}
		l.Present = append(l.Present, k)
		str, _ := v.(string)
		if k == LegacyHookdeckAPIKeyEnvKey {
			l.APIKeyEnv = str
		} else {
			l.WebhookSecretEnv = str
		}
	}
	return l, nil
}
