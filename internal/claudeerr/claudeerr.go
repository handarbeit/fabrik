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

import (
	"fmt"
	"strings"
)

// UsageLimitError signals that a Claude invocation exited because the
// account's usage limit (session or weekly) was exhausted, not because the
// stage genuinely failed. The stage never ran to completion, so this
// condition must be excluded from max_retries — see handleUsageLimitExit in
// engine/item.go, which is the sole consumer (via errors.As).
type UsageLimitError struct {
	// Message describes the structural field(s) that triggered detection (see
	// engine's classifyUsageLimitExit), for logging. Two shapes are detected:
	// terminal_reason "blocking_limit", and terminal_reason "api_error" with
	// api_error_status 429 (ADR-1811).
	Message string
	// ResetTime is always "" — the structural detector never parses a reset
	// time from prose (including the "resets 3:30am (...)" text of a 429
	// api_error result). Kept so computeUsageLimitResetDeadline's existing
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
// failed. An api_error carrying api_error_status 429 is not this type: it is a
// session/usage limit and is classified as UsageLimitError instead (ADR-1811),
// so only non-429 statuses (5xx, absent, ...) reach APIErrorExit. The stage never ran, so this condition must be excluded from
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

// ToolsDeniedError signals that a Claude invocation was blocked from making
// progress because one or more mutating tool calls (Edit, Write, a
// write-capable Bash, etc.) were denied by the CLI's own permission layer —
// a local environment/configuration problem (a stray PreToolUse hook, an
// org/user-level "ask" permission rule with no interactive prompt available,
// etc.), not a genuine defect in the stage's work. See #1523.
//
// Each denial is scoped to the specific command that was denied, not to the
// tool for the rest of the session (confirmed by direct observation in
// #1741: a later, differently-shaped Bash command ran successfully in the
// same session after an earlier one was denied) — so a bound retry that
// reshapes the offending command, or re-runs the step as separate simpler
// commands, is often exactly what resolves it. See adrs/1775-*.md, which
// supersedes ADR-1523's "by strong inference" allowlist reasoning without
// rewriting that ADR.
//
// Structurally unlike UsageLimitError/APIErrorExit: the CLI exits cleanly
// (is_error=false, terminal_reason="completed") and real, committable work
// may have happened before the denial — so this does NOT short-circuit
// finalizeStageOutcome the way the did-not-run family does. It follows
// TurnLimitError/ResumeFailureError's non-short-circuiting shape instead:
// commitWIP, push, and markCommentsSeenByStage all still run, so a
// late-invocation denial never discards earlier valid edits. It IS exempted
// from StageRetryIncremented (mirroring the did-not-run family's max_retries
// exemption), bounded instead by its own independent counter — see
// handleToolsDeniedExit in engine/item.go, the sole consumer (via
// errors.As), and ADR-1523.
type ToolsDeniedError struct {
	// ToolNames lists the distinct tool names the CLI reported as denied
	// (resp.permission_denials[].tool_name), deduplicated, for the R4
	// explanatory comment and for logging. Never empty when this error is
	// constructed.
	ToolNames []string
	// Denials carries one entry per raw denial (not deduplicated), including
	// the command detail decoded from the CLI's tool_input where available
	// (Bash only — see engine's decodeToolCommand). May be nil for a caller
	// that only ever populated ToolNames (e.g. older test fixtures); consumers
	// must degrade to tool-name-only wording when empty rather than assume a
	// command is always present.
	Denials []ToolDenial
}

// ToolDenial is a single denied tool call: the tool name the CLI reported,
// and — when decodable — the specific command that was denied. Command is
// "" when the tool wasn't Bash, or the CLI's tool_input for this denial
// couldn't be decoded; callers must treat that as "no command available,"
// never as an empty command string worth displaying.
type ToolDenial struct {
	ToolName string
	Command  string
}

func (e *ToolsDeniedError) Error() string {
	return fmt.Sprintf("claude tool call(s) denied by permission configuration: %s", strings.Join(e.ToolNames, ", "))
}
