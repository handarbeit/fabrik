package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// FooterComponent renders the bottom status bar, laid out like Fabrik's: who
// and where this daemon is (working directory and review identity) on the
// left, the Claude account it reviews with in the centre, and on the right
// the GitHub REST API rate-limit
// gauge (github.Client.RateLimitStats()'s "rest" return value — Pruefer
// issues no GraphQL requests, so "graphql" would always read zero), a
// per-category event-drop breakdown, and a signature-drift banner (both
// ADR-1563 — see DropEvent/SignatureDriftEvent).
type FooterComponent struct {
	info      Info
	restStats rateLimitStats
	now       time.Time

	dropCounts     map[string]int
	skipCounts     map[string]int // saving skips by reason (SkipEvent, #2230)
	sigDriftActive bool

	// unrecognizedAccounts/unrecognizedCount mirror the most recently
	// received UnrecognizedInstallationsEvent (#1722, R5) — a level, not a
	// delta, so the banner clears the moment a re-derivation reports the
	// count back to zero.
	unrecognizedAccounts []string
	unrecognizedCount    int
}

// Drop-reason category strings mirroring events.DropReason's constants
// (internal/events) — duplicated as plain strings rather than importing
// that package, matching DropEvent.Reason's own "plain string, no
// internal/events dependency" convention.
const (
	dropReasonSignatureMissing = "signature_missing"
	dropReasonSignatureInvalid = "signature_invalid"
	dropReasonUnwatchedOwner   = "unwatched_owner"
	dropReasonUnwatchedRepo    = "unwatched_repo"
	dropReasonDedupe           = "dedupe"
)

// rateLimitStats mirrors github.RateLimitStats's fields needed for display,
// avoiding importing the whole github package into this file (events.go
// already does, for RateLimitSnapshotEvent — this local type just narrows
// what footer.go itself reads from it).
type rateLimitStats struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

func (f FooterComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		f.now = ev.At
	case RateLimitSnapshotEvent:
		f.restStats = rateLimitStats{Limit: ev.Stats.Limit, Remaining: ev.Stats.Remaining, Reset: ev.Stats.Reset}
	case DropEvent:
		if f.dropCounts == nil {
			f.dropCounts = make(map[string]int)
		}
		// Overwrites, not increments: ev.Total is already the daemon's
		// cumulative count for this reason (see Daemon.recordDrop), so
		// incrementing here would double-count.
		f.dropCounts[ev.Reason] = ev.Total
	case SkipEvent:
		if f.skipCounts == nil {
			f.skipCounts = make(map[string]int)
		}
		f.skipCounts[ev.Reason] = ev.Total // cumulative: overwrite, never add
	case SignatureDriftEvent:
		f.sigDriftActive = ev.Active
	case UnrecognizedInstallationsEvent:
		f.unrecognizedAccounts = append([]string(nil), ev.Accounts...)
		f.unrecognizedCount = ev.Count
	}
	return f, nil
}

// dropTotals sums f.dropCounts into the four display buckets the footer's
// breakdown segment shows: sig (signature_missing + signature_invalid —
// the only actionable category, per R3/R4), unwatched (unwatched_owner +
// unwatched_repo), dedupe, and other (every remaining reason —
// malformed_envelope, malformed_attempt, malformed_payload,
// review_in_flight, pr_not_open). total is the sum of all four.
func (f FooterComponent) dropTotals() (sig, unwatched, dedupe, other, total int) {
	for reason, n := range f.dropCounts {
		total += n
		switch reason {
		case dropReasonSignatureMissing, dropReasonSignatureInvalid:
			sig += n
		case dropReasonUnwatchedOwner, dropReasonUnwatchedRepo:
			unwatched += n
		case dropReasonDedupe:
			dedupe += n
		default:
			other += n
		}
	}
	return
}

// dropBreakdownSegment renders the " · dropped: N (sig S · unwatched U ·
// dedupe D · other O)" footer segment, or "" once no drop has ever been
// recorded (R2: nothing to show before the first drop, distinguishing
// "quiet" from "actively dropping"). Colored failStyle when any signature
// failure has occurred (the actionable category), dimStyle otherwise (a
// steady trickle of benign unwatched-repo/dedupe drops is expected, not a
// problem).
func (f FooterComponent) dropBreakdownSegment() string {
	sig, unwatched, dedupe, other, total := f.dropTotals()
	if total == 0 {
		return ""
	}
	plain := fmt.Sprintf("dropped: %d (sig %d · unwatched %d · dedupe %d · other %d)", total, sig, unwatched, dedupe, other)
	if sig > 0 {
		return failStyle.Render(plain)
	}
	return dimStyle.Render(plain)
}

// skipSegment renders the "saved: patch N · trial M · catch-up K" footer
// segment (#2230 R4) — the reviews not run because the change was already
// reviewed — or "" until the first saving skip.
func (f FooterComponent) skipSegment() string {
	p, t, c := f.skipCounts["patch_unchanged"], f.skipCounts["trial_equivalent"], f.skipCounts["catch_up"]
	if p+t+c == 0 {
		return ""
	}
	return dimStyle.Render(fmt.Sprintf("skipped: patch %d · trial %d · catch-up %d", p, t, c))
}

// SkipCount returns the cumulative saving-skip count for reason (a SkipEvent
// key), or 0. Exported for tests.
func (f FooterComponent) SkipCount(reason string) int { return f.skipCounts[reason] }

// driftBannerSegment renders the high-contrast signature-drift banner while
// sigDriftActive (R4), or "" otherwise.
func (f FooterComponent) driftBannerSegment() string {
	if !f.sigDriftActive {
		return ""
	}
	return failStyle.Bold(true).Render("⚠ SIGNATURE DRIFT — check webhook secret")
}

// unrecognizedInstallationsBannerSegment renders a high-contrast banner
// while any App installation is currently unrecognized (#1722, R5), naming
// the affected account(s) so an operator sees it without reading the log —
// mirroring driftBannerSegment's shape. "" when unrecognizedCount is 0.
func (f FooterComponent) unrecognizedInstallationsBannerSegment() string {
	if f.unrecognizedCount == 0 {
		return ""
	}
	plain := fmt.Sprintf("⚠ UNRECOGNIZED INSTALLATION: %s", strings.Join(f.unrecognizedAccounts, ", "))
	return failStyle.Bold(true).Render(plain)
}

func (f FooterComponent) View(width int) string {
	var leftParts []string
	for _, p := range []string{f.info.CWD, f.info.Identity} {
		if p != "" {
			leftParts = append(leftParts, p)
		}
	}
	left := dimStyle.Render(strings.Join(leftParts, " · "))

	var rightParts []string
	if drift := f.driftBannerSegment(); drift != "" {
		rightParts = append(rightParts, drift)
	}
	if unrecognized := f.unrecognizedInstallationsBannerSegment(); unrecognized != "" {
		rightParts = append(rightParts, unrecognized)
	}
	if breakdown := f.dropBreakdownSegment(); breakdown != "" {
		rightParts = append(rightParts, breakdown)
	}
	if skipped := f.skipSegment(); skipped != "" {
		rightParts = append(rightParts, skipped)
	}
	if f.restStats.Limit > 0 {
		countdown := fmtRateLimitCountdown(f.restStats.Reset, f.now)
		plain := fmt.Sprintf("rest %d/%d  %s", f.restStats.Remaining, f.restStats.Limit, countdown)
		pct := float64(f.restStats.Remaining) / float64(f.restStats.Limit)
		var style lipgloss.Style
		switch {
		case pct > 0.5:
			style = successStyle
		case pct > 0.2:
			style = activeStyle
		default:
			style = failStyle
		}
		rightParts = append(rightParts, style.Render(plain))
	}
	right := strings.Join(rightParts, "  ")

	maxWidth := width - 2
	if maxWidth < 1 {
		maxWidth = 1
	}
	rightWidth := lipgloss.Width(right)
	// The right side is operational state (banners, rate limit) and is never
	// shortened; the left side gives way to it, keeping at least one space.
	availLeft := maxWidth - rightWidth
	if right != "" {
		availLeft--
	}
	if availLeft < 0 {
		availLeft = 0
	}
	if lipgloss.Width(left) > availLeft {
		left = ansi.Truncate(left, availLeft, "…")
	}
	gap := maxWidth - lipgloss.Width(left) - rightWidth
	if gap < 0 {
		gap = 0
	}

	// Centre the Claude account in the gap when it fits with at least one
	// space either side. It is the first thing dropped as the terminal
	// narrows: it is standing context, while left and right are what an
	// operator scans for — the same priority as Fabrik's footer.
	if account := f.info.ClaudeAccount; account != "" {
		rendered := dimStyle.Render(account)
		if w := lipgloss.Width(rendered); gap >= w+2 {
			leftPad := (gap - w) / 2
			return left + strings.Repeat(" ", leftPad) + rendered + strings.Repeat(" ", gap-w-leftPad) + right
		}
	}
	return left + strings.Repeat(" ", gap) + right
}

func (f FooterComponent) Height() int {
	return 1
}

// DropCount returns the most recently reported cumulative count for reason
// (a events.DropReason value rendered as a string — see DropEvent), or 0 if
// none has been recorded.
func (f FooterComponent) DropCount(reason string) int {
	return f.dropCounts[reason]
}

// SignatureDriftActive reports whether the footer's signature-drift banner
// is currently shown.
func (f FooterComponent) SignatureDriftActive() bool {
	return f.sigDriftActive
}

// UnrecognizedInstallationsCount reports the most recently reported count of
// currently-unrecognized App installations (#1722, R5).
func (f FooterComponent) UnrecognizedInstallationsCount() int {
	return f.unrecognizedCount
}
