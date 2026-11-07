package cmd

import (
	"fmt"
	"strings"

	"github.com/handarbeit/fabrik/config"
)

// Removed Hookdeck settings (ADR 2477). Hookdeck was removed in v0.0.92; a
// leftover flag, variable or config.yaml key must neither fail startup nor go
// unmentioned. This file collects them so cmd can (a) print one notice and
// (b) hand the variable names they point at to the engine, which keeps
// scrubbing those variables from worker environments. Names only — a value is
// never printed, because a value is a variable name an operator chose, and the
// variable it names holds the secret.

const (
	legacyHookdeckAPIKeyEnvVar        = "FABRIK_HOOKDECK_API_KEY_ENV"
	legacyHookdeckWebhookSecretEnvVar = "FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV"
	legacyHookdeckEnvPrefix           = "FABRIK_HOOKDECK_"
)

// collectLegacyHookdeck gathers, from every route (the removed flags, the
// environment, config.yaml), the settings present and the variable names they
// point at. ignored is de-duplicated and ordered; scrubNames is de-duplicated
// and includes the value from every route, not only the winning one, so no
// leftover secret variable escapes the worker-env scrub.
func collectLegacyHookdeck(flagAPIKeyEnv, flagSecretEnv string, flagsSet map[string]bool, environ []string, yaml config.LegacyHookdeck) (scrubNames, ignored []string) {
	seenIgnored := map[string]bool{}
	seenScrub := map[string]bool{}
	note := func(setting string) {
		if !seenIgnored[setting] {
			seenIgnored[setting] = true
			ignored = append(ignored, setting)
		}
	}
	scrub := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seenScrub[name] {
			seenScrub[name] = true
			scrubNames = append(scrubNames, name)
		}
	}

	if flagsSet["hookdeck-api-key-env"] || flagAPIKeyEnv != "" {
		note("--hookdeck-api-key-env")
		scrub(flagAPIKeyEnv)
	}
	if flagsSet["hookdeck-webhook-secret-env"] || flagSecretEnv != "" {
		note("--hookdeck-webhook-secret-env")
		scrub(flagSecretEnv)
	}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, legacyHookdeckEnvPrefix) {
			continue
		}
		note(k)
		if k == legacyHookdeckAPIKeyEnvVar || k == legacyHookdeckWebhookSecretEnvVar {
			scrub(v)
		}
	}
	for _, k := range yaml.Present {
		note(k)
	}
	scrub(yaml.APIKeyEnv)
	scrub(yaml.WebhookSecretEnv)
	sortStrings(ignored)
	return scrubNames, ignored
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// legacyHookdeckNotice is the single startup line reporting ignored settings,
// or "" when there are none.
func legacyHookdeckNotice(ignored []string) string {
	if len(ignored) == 0 {
		return ""
	}
	return fmt.Sprintf("[startup] notice: ignoring removed Hookdeck setting(s): %s — Hookdeck was removed in v0.0.92; "+
		"remove them (see \"Migrating from Hookdeck\" in docs/USER_GUIDE.md)\n", strings.Join(ignored, ", "))
}
