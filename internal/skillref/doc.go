// Package skillref discovers the plugins a Claude Code worker loads, resolves
// "plugin:skill" references against them and detects a skill that failed to
// load at runtime (ADR 2137, ADR 2138, ADR 2305).
//
// It is a leaf package shared by the engine and Pruefer. It may import the
// standard library, plugin (for the namespace separator) and internal/claudeerr,
// and nothing from engine, stages or github (ADR 1113: Pruefer must not import
// engine). What differs between consumers — the preferred base plugins for a
// bare name and the legacy-reference table — is supplied through Options, never
// hardcoded here.
package skillref
