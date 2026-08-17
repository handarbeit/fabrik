package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// FooterComponent renders the bottom status bar: the running session-total
// cost/turns (accumulated in-memory across every reviewed PR, reset on
// daemon restart — matching Fabrik's own session-scoped TUI totals, see
// adrs/1114-pruefer-tui-architecture.md), the GitHub REST API rate-limit
// gauge (github.Client.RateLimitStats()'s "rest" return value — Pruefer
// issues no GraphQL requests, so "graphql" would always read zero), a
// per-category event-drop breakdown, and a signature-drift banner (both
// ADR-1563 — see DropEvent/SignatureDriftEvent).
type FooterComponent struct {
	reviewedCount int
	totalTurns    int
	totalCostUSD  float64
	restStats     rateLimitStats
	now           time.Time

	dropCounts     map[string]int
	sigDriftActive bool
}

// Drop-reason category strings mirroring events.DropReason's constants
// (pruefer/events) — duplicated as plain strings rather than importing
// that package, matching DropEvent.Reason's own "plain string, no
// pruefer/events dependency" convention.
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
	case ReviewCompletedEvent:
		if ev.Reviewed {
			f.reviewedCount++
			f.totalTurns += ev.NumTurns
			f.totalCostUSD += ev.CostUSD
		}
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
	case SignatureDriftEvent:
		f.sigDriftActive = ev.Active
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

// driftBannerSegment renders the high-contrast signature-drift banner while
// sigDriftActive (R4), or "" otherwise.
func (f FooterComponent) driftBannerSegment() string {
	if !f.sigDriftActive {
		return ""
	}
	return failStyle.Bold(true).Render("⚠ SIGNATURE DRIFT — check webhook secret")
}

func (f FooterComponent) View(width int) string {
	leftPlain := fmt.Sprintf("session: %d reviewed  ·  %d turns  ·  $%.4f", f.reviewedCount, f.totalTurns, f.totalCostUSD)
	left := dimStyle.Render(leftPlain)

	var rightParts []string
	if drift := f.driftBannerSegment(); drift != "" {
		rightParts = append(rightParts, drift)
	}
	if breakdown := f.dropBreakdownSegment(); breakdown != "" {
		rightParts = append(rightParts, breakdown)
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
	if right == "" {
		if lipgloss.Width(left) > maxWidth {
			runes := []rune(leftPlain)
			for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > maxWidth {
				runes = runes[:len(runes)-1]
			}
			left = dimStyle.Render(string(runes) + "…")
		}
		return left
	}

	rightWidth := lipgloss.Width(right)
	gap := maxWidth - lipgloss.Width(left) - rightWidth
	if gap < 1 {
		availLeft := maxWidth - rightWidth - 1
		if availLeft < 0 {
			availLeft = 0
		}
		runes := []rune(leftPlain)
		for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > availLeft {
			runes = runes[:len(runes)-1]
		}
		if len(runes) == 0 {
			left = ""
		} else {
			left = dimStyle.Render(string(runes) + "…")
		}
		gap = maxWidth - lipgloss.Width(left) - rightWidth
		if gap < 0 {
			gap = 0
		}
	}
	return left + strings.Repeat(" ", gap) + right
}

func (f FooterComponent) Height() int {
	return 1
}

// TotalCostUSD returns the accumulated session-total review cost.
func (f FooterComponent) TotalCostUSD() float64 {
	return f.totalCostUSD
}

// TotalTurns returns the accumulated session-total review turns.
func (f FooterComponent) TotalTurns() int {
	return f.totalTurns
}

// ReviewedCount returns the accumulated session-total number of reviewed PRs.
func (f FooterComponent) ReviewedCount() int {
	return f.reviewedCount
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
