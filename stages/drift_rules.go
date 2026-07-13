package stages

import "time"

// claudeKillGraceDefault mirrors the engine's compile-time kill-grace fallback
// (engine/engine.go sets claudeKillGraceSigInt/SigTerm to 10s each when unset).
// It is duplicated here as a literal because the stages package cannot import
// engine (import cycle), and `fabrik refresh-stages` runs as a standalone CLI
// invocation with no access to a running daemon's resolved config. Known
// limitation: an operator who globally overrides --kill-grace-sigint /
// FABRIK_KILL_GRACE_SIGINT away from 10s and also omits kill_grace from a
// stage file will not be warned that their effective config now differs from
// this literal default.
const claudeKillGraceDefault = 10 * time.Second

// noOpKeyRules maps a top-level YAML key to a predicate reporting whether the
// embedded default's value for that key is behaviorally identical to omitting
// the key entirely. Only keys registered here are eligible for suppression in
// FilterNoOpKeys; every other key is always reported as missing verbatim, so a
// future default key with no rule can't accidentally warn less than it does
// today.
var noOpKeyRules = map[string]func(*Stage) bool{
	"kill_grace": isKillGraceNoOp,
	"completion": isCompletionNoOp,
}

// isKillGraceNoOp reports whether defaultStage's kill_grace value resolves to
// the same behavior as omitting kill_grace entirely. It consults the raw YAML
// strings (SigIntRaw/SigTermRaw), not the parsed durations: an empty raw
// string and an explicit "10s" both mean "inherit the engine default", but an
// explicit "0s" means "skip the SIGINT step" (see ADR 054 and
// engine/item.go's sentinel translation) even though "" and "0s" both parse
// to a zero time.Duration. Judging equivalence from the parsed duration alone
// would wrongly treat "0s" as a no-op.
func isKillGraceNoOp(defaultStage *Stage) bool {
	return isKillGraceFieldNoOp(defaultStage.KillGrace.SigIntRaw) &&
		isKillGraceFieldNoOp(defaultStage.KillGrace.SigTermRaw)
}

func isKillGraceFieldNoOp(raw string) bool {
	if raw == "" {
		return true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return false
	}
	return d == claudeKillGraceDefault
}

// isCompletionNoOp reports whether defaultStage's completion.type value
// resolves to the same behavior as omitting completion entirely (loadOne
// defaults Completion.Type to "claude" when the field is absent).
func isCompletionNoOp(defaultStage *Stage) bool {
	return defaultStage.Completion.Type == "" || defaultStage.Completion.Type == "claude"
}

// FilterNoOpKeys returns missing with any key removed whose embedded-default
// value in defaultStage would be behaviorally equivalent to omission (per
// noOpKeyRules). Keys without a registered rule are never filtered. A nil
// defaultStage returns missing unchanged, so callers that failed to parse a
// typed default degrade to today's over-warning behavior rather than
// silently under-warning.
func FilterNoOpKeys(missing []string, defaultStage *Stage) []string {
	if defaultStage == nil {
		return missing
	}
	filtered := missing[:0]
	for _, key := range missing {
		if rule, ok := noOpKeyRules[key]; ok && rule(defaultStage) {
			continue
		}
		filtered = append(filtered, key)
	}
	return filtered
}
