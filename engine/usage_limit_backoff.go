package engine

import (
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// claudeUsageLimitFallbackBackoff is the fixed conservative backoff applied
// when a usage-limit exit carries no usable structured reset instant (absent,
// zero, malformed, already past, or no exhausted window — see
// resolveUsageLimitDeadline, ADR-1815). An order of magnitude longer than the
// normal 5-minute dispatch cooldown so a hit still meaningfully reduces
// hammering even without a known reset, without over-committing to a
// duration that might strand the engine long after the real limit clears.
const claudeUsageLimitFallbackBackoff = 1 * time.Hour

// claudeSuspendedUntilTime reports whether Claude dispatch is currently
// suspended account-wide, and the deadline if so. Returns ok=false once now
// has reached the deadline, at which point the very next dispatch attempt
// proceeds normally — there is no separate "resume" step to run.
func (e *Engine) claudeSuspendedUntilTime(now time.Time) (deadline time.Time, ok bool) {
	e.claudeSuspendMu.Lock()
	deadline = e.claudeSuspendedUntil
	e.claudeSuspendMu.Unlock()
	if deadline.IsZero() || !now.Before(deadline) {
		return time.Time{}, false
	}
	return deadline, true
}

// activateClaudeSuspension records that a worker observed the account usage
// limit and suspends new Claude dispatch account-wide until the computed
// deadline. If a suspension is already active, the deadline is only extended
// (never shortened) — concurrent workers racing to report the same or
// different reset times converge on the latest one seen. limitErr may be nil
// (no structured reset; the fixed fallback applies). Logs and emits
// tui.ClaudeUsageLimitAlertEvent only when the suspension actually changes
// (activates or extends), mirroring the non-spamming idiom used for
// fabrik:claude-limit/fabrik:awaiting-ci.
func (e *Engine) activateClaudeSuspension(issueNumber int, limitErr *claudeUsageLimitError, now time.Time) {
	deadline, source, reason := resolveUsageLimitDeadline(limitErr, now)

	e.claudeSuspendMu.Lock()
	changed := e.claudeSuspendedUntil.IsZero() || deadline.After(e.claudeSuspendedUntil)
	if changed {
		e.claudeSuspendedUntil = deadline
	}
	e.claudeSuspendMu.Unlock()

	if !changed {
		return
	}

	switch source {
	case usageLimitSourceExact:
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; suspending Claude dispatch account-wide until %s (structured reset from CLI rate_limit_event)", deadline.Format(time.RFC3339))
	case usageLimitSourceClamped:
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; structured reset %s is beyond the %s ceiling, clamping — suspending Claude dispatch account-wide until %s", limitErr.ResetAt.Format(time.RFC3339), usageLimitMaxResetHorizon, deadline.Format(time.RFC3339))
	default:
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; no usable structured reset (reason=%s), falling back to a %s suspension of Claude dispatch account-wide until %s", reason, claudeUsageLimitFallbackBackoff, deadline.Format(time.RFC3339))
	}
	e.emitStructural(tui.ClaudeUsageLimitAlertEvent{Suspended: true, Reset: deadline})
}

// clearClaudeSuspension clears an active account-wide Claude suspension, if
// one is set. Called both when a successful invocation proves the limit has
// already cleared (early clear, ahead of the computed deadline) and lazily
// once now passes the deadline. Logs and emits
// tui.ClaudeUsageLimitAlertEvent only when a suspension was actually active.
func (e *Engine) clearClaudeSuspension(reason string) {
	e.claudeSuspendMu.Lock()
	wasActive := !e.claudeSuspendedUntil.IsZero()
	e.claudeSuspendedUntil = time.Time{}
	e.claudeSuspendMu.Unlock()

	if !wasActive {
		return
	}
	e.logf(0, "claude-limit", "clearing account-wide Claude dispatch suspension: %s", reason)
	e.emitStructural(tui.ClaudeUsageLimitAlertEvent{Suspended: false})
}
