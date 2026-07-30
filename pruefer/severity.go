package pruefer

// Severity is a per-finding severity tier, reported by Claude in the fenced
// JSON findings block (see ReviewFinding) and, separately, configured as
// Config.RequestChangesThreshold. Deliberately minimal — four ordinal tiers,
// just enough for a threshold comparison — not a multi-dimensional risk
// score (that's #1051/#1177's separate, out-of-scope concept).
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// severityRank returns s's ordinal position, low (1) to critical (4). Any
// unrecognized or empty value — including a missing "severity" field on a
// finding Claude produced, or a typo'd tier name — ranks 0, below every real
// tier. This is a deliberate fail-closed-toward-COMMENT default: an
// untrusted per-finding value must never be trusted to escalate toward the
// disruptive REQUEST_CHANGES outcome. Contrast Config.RequestChangesThreshold
// validation in config.go, which fails loud (an error) instead — a
// mistyped *config* value is a one-time operator error worth catching at
// startup, not a value to silently rank as "meets every threshold".
func severityRank(s Severity) int {
	switch s {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

// validSeverity reports whether s is one of the four recognized tiers.
// Used by LoadConfig to fail loud on a mistyped RequestChangesThreshold.
func validSeverity(s Severity) bool {
	switch s {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}
