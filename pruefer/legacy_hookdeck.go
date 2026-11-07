package pruefer

import (
	"fmt"
	"sort"
	"strings"
)

// Removed Hookdeck support (ADR 2477). Pruefer cannot import the engine
// (ADR 1113), so the refusal text and the leftover-settings notice are
// implemented here, mirroring engine.HookdeckMigrationMessage and cmd's
// legacy_hookdeck.go. Names only — never a value.

const (
	removedEventSourceHookdeck = "hookdeck"
	legacyHookdeckEnvPrefix    = "PRUEFER_HOOKDECK_"
)

// hookdeckMigrationMessage names the replacements and the documentation
// section; it is the whole error for event_source: hookdeck.
const hookdeckMigrationMessage = "event_source: hookdeck was removed in v0.0.92 (ADR 2477) — " +
	"use event_source: tailscale (embedded Tailscale Funnel receiver) or event_source: direct " +
	"(your own tunnel or proxy), repoint the GitHub App webhook URL and secret, and delete the " +
	"hookdeck settings, or use event_source: poll. See \"Migrating from Hookdeck\" in cmd/pruefer/README.md " +
	"and docs/USER_GUIDE.md"

// noteLegacyHookdeck records one removed setting, keeping the list unique and sorted.
func (c *Config) noteLegacyHookdeck(name string) {
	for _, n := range c.LegacyHookdeck {
		if n == name {
			return
		}
	}
	c.LegacyHookdeck = append(c.LegacyHookdeck, name)
	sort.Strings(c.LegacyHookdeck)
}

// legacyHookdeckNotice is the single startup line reporting ignored Hookdeck
// settings, or "" when there are none.
func legacyHookdeckNotice(ignored []string) string {
	if len(ignored) == 0 {
		return ""
	}
	return fmt.Sprintf("notice: ignoring removed Hookdeck setting(s): %s — Hookdeck was removed in v0.0.92; "+
		"remove them (see \"Migrating from Hookdeck\" in cmd/pruefer/README.md)\n", strings.Join(ignored, ", "))
}
