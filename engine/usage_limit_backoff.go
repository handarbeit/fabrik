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

// claudeUsageLimitedUntilTime reports whether Claude dispatch is currently
// usage limited account-wide, and the deadline if so. Returns ok=false once now
// has reached the deadline, at which point the very next dispatch attempt
// proceeds normally — there is no separate "resume" step to run.
func (e *Engine) claudeUsageLimitedUntilTime(now time.Time) (deadline time.Time, ok bool) {
	e.claudeUsageLimitMu.Lock()
	deadline = e.claudeUsageLimitedUntil
	e.claudeUsageLimitMu.Unlock()
	if deadline.IsZero() || !now.Before(deadline) {
		return time.Time{}, false
	}
	return deadline, true
}

// activateClaudeUsageLimit records that a worker observed the account usage
// limit and holds new Claude dispatch account-wide until the computed
// deadline. If a usage-limit hold is already active, the deadline is only extended
// (never shortened) — concurrent workers racing to report the same or
// different reset times converge on the latest one seen. limitErr may be nil
// (no structured reset; the fixed fallback applies). Logs and emits
// tui.ClaudeUsageLimitAlertEvent only when the usage-limit hold actually changes
// (activates or extends), mirroring the non-spamming idiom used for
// fabrik:claude-limit/fabrik:awaiting-ci.
func (e *Engine) activateClaudeUsageLimit(issueNumber int, limitErr *claudeUsageLimitError, now time.Time) {
	deadline, source, reason := resolveUsageLimitDeadline(limitErr, now)

	e.claudeUsageLimitMu.Lock()
	changed := e.claudeUsageLimitedUntil.IsZero() || deadline.After(e.claudeUsageLimitedUntil)
	if changed {
		e.claudeUsageLimitedUntil = deadline
	}
	e.claudeUsageLimitMu.Unlock()

	if !changed {
		return
	}
	e.persistUsageLimit() // #2349: the hold survives a restart

	switch source {
	case usageLimitSourceExact:
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; holding Claude dispatch account-wide until %s (structured reset from CLI rate_limit_event)", deadline.Format(time.RFC3339))
	case usageLimitSourceClamped:
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; structured reset %s is beyond the %s ceiling, clamping — holding Claude dispatch account-wide until %s", limitErr.ResetAt.Format(time.RFC3339), usageLimitMaxResetHorizon, deadline.Format(time.RFC3339))
	default:
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; no usable structured reset (reason=%s), falling back to a %s hold on Claude dispatch account-wide until %s", reason, claudeUsageLimitFallbackBackoff, deadline.Format(time.RFC3339))
	}
	e.emitStructural(tui.ClaudeUsageLimitAlertEvent{UsageLimited: true, Reset: deadline})
	e.channelNudge() // observation only (#1968): re-check account-wide state
}

// clearClaudeUsageLimit clears an active account-wide Claude usage-limit hold, if
// one is set. Called both when a successful invocation proves the limit has
// already cleared (early clear, ahead of the computed deadline) and lazily
// once now passes the deadline. Logs and emits
// tui.ClaudeUsageLimitAlertEvent only when a usage-limit hold was actually active.
func (e *Engine) clearClaudeUsageLimit(reason string) {
	e.claudeUsageLimitMu.Lock()
	wasActive := !e.claudeUsageLimitedUntil.IsZero()
	e.claudeUsageLimitedUntil = time.Time{}
	e.claudeUsageLimitMu.Unlock()

	// Every clear path deletes the persisted deadline (#2349), including a clear with no
	// active in-memory hold, so a stale file can never hold dispatch after a manual clear.
	e.persistUsageLimit()
	if !wasActive {
		return
	}
	e.logf(0, "claude-limit", "clearing account-wide Claude dispatch hold: %s", reason)
	e.emitStructural(tui.ClaudeUsageLimitAlertEvent{UsageLimited: false})
	e.channelNudge() // observation only (#1968)
}
