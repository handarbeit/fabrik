package engine

import (
	"strings"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// claudeUsageLimitFallbackBackoff is the fixed conservative backoff applied
// when a usage-limit reset time can't be parsed (empty ResetTime, malformed
// fragment, or unknown IANA zone). An order of magnitude longer than the
// normal 5-minute dispatch cooldown so a hit still meaningfully reduces
// hammering even without a known reset, without over-committing to a
// duration that might strand the engine long after the real limit clears.
const claudeUsageLimitFallbackBackoff = 1 * time.Hour

// usageLimitResetTimeLayout is the Go reference-time layout matching the
// 12-hour clock fragment Anthropic's CLI emits (e.g. "10:20pm").
const usageLimitResetTimeLayout = "3:04pm"

// parseUsageLimitResetTime parses a reset-time fragment of the form
// "3:04pm (Zone/City)" — a 12-hour wall-clock time plus an IANA zone name,
// with no date — into a concrete time.Time. The wall-clock time is resolved
// against now's date in the parsed zone; if the resulting time is not after
// now, it is rolled forward one day (the CLI always reports a future reset,
// so "already past" only happens because the fragment carries no date).
//
// Returns ok=false for any fragment that doesn't match the expected shape or
// whose zone doesn't load — callers fall back to a fixed backoff in that case.
func parseUsageLimitResetTime(resetTime string, now time.Time) (time.Time, bool) {
	clockStr, zoneStr, ok := splitUsageLimitResetFragment(resetTime)
	if !ok {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(zoneStr)
	if err != nil {
		return time.Time{}, false
	}
	clock, err := time.ParseInLocation(usageLimitResetTimeLayout, clockStr, loc)
	if err != nil {
		return time.Time{}, false
	}
	nowInLoc := now.In(loc)
	deadline := time.Date(
		nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(),
		clock.Hour(), clock.Minute(), 0, 0, loc,
	)
	if !deadline.After(nowInLoc) {
		deadline = deadline.AddDate(0, 0, 1)
	}
	return deadline, true
}

// splitUsageLimitResetFragment splits "3:04pm (Zone/City)" into its clock and
// zone parts. Returns ok=false if the fragment doesn't have the expected
// "<clock> (<zone>)" shape.
func splitUsageLimitResetFragment(resetTime string) (clockStr, zoneStr string, ok bool) {
	open := strings.IndexByte(resetTime, '(')
	closeIdx := strings.IndexByte(resetTime, ')')
	if open < 0 || closeIdx < 0 || closeIdx < open {
		return "", "", false
	}
	clockStr = strings.TrimSpace(resetTime[:open])
	zoneStr = strings.TrimSpace(resetTime[open+1 : closeIdx])
	if clockStr == "" || zoneStr == "" {
		return "", "", false
	}
	return clockStr, zoneStr, true
}

// computeUsageLimitResetDeadline computes the deadline a usage-limit
// suspension should run until. It wraps parseUsageLimitResetTime, falling
// back to a fixed conservative backoff (claudeUsageLimitFallbackBackoff) when
// resetTime is empty or doesn't parse. The second return value reports
// whether the deadline came from a successfully parsed reset time (true) or
// the fallback (false), so callers can log which path was taken.
func computeUsageLimitResetDeadline(resetTime string, now time.Time) (deadline time.Time, parsed bool) {
	if deadline, ok := parseUsageLimitResetTime(resetTime, now); ok {
		return deadline, true
	}
	return now.Add(claudeUsageLimitFallbackBackoff), false
}

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
// different reset times converge on the latest one seen. Logs and emits
// tui.ClaudeUsageLimitAlertEvent only when the suspension actually changes
// (activates or extends), mirroring the non-spamming idiom used for
// fabrik:claude-limit/fabrik:awaiting-ci.
func (e *Engine) activateClaudeSuspension(issueNumber int, resetTimeRaw string, now time.Time) {
	deadline, parsed := computeUsageLimitResetDeadline(resetTimeRaw, now)

	e.claudeSuspendMu.Lock()
	changed := e.claudeSuspendedUntil.IsZero() || deadline.After(e.claudeSuspendedUntil)
	if changed {
		e.claudeSuspendedUntil = deadline
	}
	e.claudeSuspendMu.Unlock()

	if !changed {
		return
	}

	if parsed {
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; suspending Claude dispatch account-wide until %s (parsed from CLI reset time)", deadline.Format(time.RFC3339))
	} else {
		e.logf(issueNumber, "claude-limit", "account usage-limit hit; reset time unparseable, falling back to a %s suspension of Claude dispatch account-wide until %s", claudeUsageLimitFallbackBackoff, deadline.Format(time.RFC3339))
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
