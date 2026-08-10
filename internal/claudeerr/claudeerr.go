// Package claudeerr holds the error types the engine raises to classify a
// Claude Code invocation's failure mode. It is a leaf package — it imports
// nothing from this repository — so that both engine (which raises and
// classifies these errors via errors.As) and tests/sim (which must construct
// them from an external test package to script the corresponding failure
// shapes) can import it without an import cycle.
//
// This mirrors the precedent tests/sim/simgh/ghfault set for #1457, with one
// difference in kind: ghfault constructs synthetic GitHub-side faults for
// test fixtures, while this package holds the real error types engine/item.go
// classifies in production via errors.As. That is also why this package lives
// under internal/ rather than under tests/ — engine imports it unconditionally
// in production, not under a test build tag, so it cannot live in the test
// tree. See ADR-1449.
//
// engine/claude.go keeps unexported type aliases (e.g.
// "type claudeUsageLimitError = claudeerr.UsageLimitError") for every type
// here, so every existing construction site and errors.As assertion inside
// engine's own test files keeps compiling and behaving unchanged — this
// extraction moved the types' location, not their name or behavior.
package claudeerr

import "fmt"

// UsageLimitError signals that a Claude invocation exited because the
// account's usage limit (session or weekly) was exhausted, not because the
// stage genuinely failed. The stage never ran to completion, so this
// condition must be excluded from max_retries — see handleUsageLimitExit in
// engine/item.go, which is the sole consumer (via errors.As).
type UsageLimitError struct {
	// Message describes the structural field that triggered detection (see
	// engine's classifyUsageLimitExit), for logging.
	Message string
	// ResetTime is always "" — the structural detector never parses a reset
	// time from prose. Kept so computeUsageLimitResetDeadline's existing
	// fallback-when-empty path (claudeUsageLimitFallbackBackoff) is exercised
	// unconditionally; do not populate this from matched text (#1183).
	ResetTime string
}

func (e *UsageLimitError) Error() string {
	if e.ResetTime != "" {
		return fmt.Sprintf("claude usage limit hit: %s (resets %s)", e.Message, e.ResetTime)
	}
	return fmt.Sprintf("claude usage limit hit: %s", e.Message)
}

// TurnLimitError signals that a Claude invocation exited because it
// exhausted its configured turn budget (CLI-reported subtype
// "error_max_turns"), not because the stage genuinely failed. Unlike
// UsageLimitError, this does NOT short-circuit finalizeStageOutcome /
// processComments with an early return: the existing retry/escalation
// machinery (StageAttempted, commitWIP, StageRetryIncremented, MaxRetries)
// must still run exactly as it does today for a turn-cap exit — that
// behavior is pre-existing and deliberate (see #1081/#448), and out of scope
// for #1178. This sentinel only changes what feeds the InvocationRecorded
// write, so history/TUI can render the run as incomplete-but-resumable
// rather than as a genuine fault.
type TurnLimitError struct {
	// TerminalReason is the CLI's own terminal_reason field, if present, for logging.
	TerminalReason string
	// NumTurns is the CLI-reported turn count at exit, for logging.
	NumTurns int
}

func (e *TurnLimitError) Error() string {
	return fmt.Sprintf("claude exited: turn limit reached (num_turns=%d)", e.NumTurns)
}

// APIErrorExit signals that a Claude invocation exited because of a
// transient Anthropic-side API error, not because the stage genuinely
// failed. The stage never ran, so this condition must be excluded from
// max_retries — see handleAPIErrorExit in engine/item.go, which is the sole
// consumer (via errors.As).
//
// Deliberately a distinct type from UsageLimitError, not a second value
// recognized by classifyUsageLimitExit itself: UsageLimitError is also the
// trigger, by Go type, for two behaviors that must NOT apply here —
// activateClaudeSuspension (engine/item.go, engine/comments.go, forbidden by
// R3 of #1458, since an api_error is per-invocation, not account-wide) and
// the comment-processing circuit breaker bypass (engine/comments.go,
// forbidden by R6's spirit, since exempting api_error from the breaker would
// leave the comment-triggered dispatch path with no bound at all). Being a
// distinct type means neither errors.As(&UsageLimitError{}) check ever
// matches a *APIErrorExit, so both call sites need no changes. See ADR-1458.
type APIErrorExit struct {
	// TerminalReason is the CLI's own terminal_reason field, for logging.
	TerminalReason string
	// NumTurns is the CLI-reported turn count at exit, for logging.
	NumTurns int
	// CostUSD is the CLI-reported cost at exit, for logging.
	CostUSD float64
}

func (e *APIErrorExit) Error() string {
	return fmt.Sprintf("claude exited: transient api_error (terminal_reason=%q, num_turns=%d, cost=$%.4f)", e.TerminalReason, e.NumTurns, e.CostUSD)
}

// ResumeFailureError signals that a Claude invocation which resumed an
// existing session (--resume) failed for a reason none of the more specific
// classifiers above caught. Unlike UsageLimitError/APIErrorExit, this does
// NOT imply the stage never ran — a resumed session can fail after real work
// happened — so it must NOT short-circuit finalizeStageOutcome the way the
// did-not-run family does; it follows TurnLimitError's non-short-circuiting
// shape instead (commitWIP, push, InvocationRecorded all still run). It is
// exempted from StageRetryIncremented (mirroring the did-not-run family's
// max_retries exemption) precisely so the mechanism this type exists to
// enable — a guaranteed cold-start attempt once ConsecutiveFailures reaches
// Threshold — cannot itself be starved by the failures it is counting. See
// #1414, ADR-1414.
type ResumeFailureError struct {
	// Cause is the underlying error the CLI exited with.
	Cause error
	// SessionID is the --resume session ID this invocation attempted to
	// resume, for logging.
	SessionID string
	// ConsecutiveFailures is the count of consecutive resume failures for
	// this (issue, stage) session, including this one.
	ConsecutiveFailures int
	// Threshold is the configured MaxResumeFailures value in effect for this
	// invocation, for logging.
	Threshold int
	// Abandoned reports whether this invocation's failure pushed the
	// consecutive count to (or past) Threshold, causing the session pointer
	// to be discarded so the next invocation cold-starts.
	Abandoned bool
}

func (e *ResumeFailureError) Error() string {
	return fmt.Sprintf("claude exited with error while resuming session %s (consecutive failures=%d/%d, abandoned=%t): %v", e.SessionID, e.ConsecutiveFailures, e.Threshold, e.Abandoned, e.Cause)
}

func (e *ResumeFailureError) Unwrap() error {
	return e.Cause
}
