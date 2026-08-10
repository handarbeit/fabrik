package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// rateLimitAlertState tracks one bucket's (REST or GraphQL) exhaustion state
// independently. It mirrors ClaudeUsageLimitBannerComponent's active/reset
// model rather than AlertBannerComponent's former ratio-recheck: REST has no
// ratio-based clear concept of its own (its exhaustion/clear is purely
// event-driven), and GraphQL's ratio recheck was redundant with its own
// explicit clear event (both fire at the same 50% threshold, ADR-028) — see
// ADR 1482.
type rateLimitAlertState struct {
	active bool
	reset  time.Time
}

// isVisible returns true when this bucket's banner should be shown: an
// exhaustion is active and (when a reset time is known) now hasn't reached
// it yet. This lets the banner self-clear off the per-second TickEvent
// fan-out even if no explicit Exhausted=false event ever arrives, mirroring
// ClaudeUsageLimitBannerComponent.isVisible.
func (s rateLimitAlertState) isVisible(now time.Time) bool {
	if !s.active {
		return false
	}
	if !s.reset.IsZero() && !now.Before(s.reset) {
		return false
	}
	return true
}

// AlertBannerComponent renders a single high-visibility row when the REST
// and/or GraphQL rate-limit budget is exhausted. It implements the
// Component interface. REST and GraphQL are tracked as fully independent
// rateLimitAlertState values — neither bucket's visibility, label text, or
// reset time is ever derived from the other's state (#1482). Height()
// returns 1 when either banner is visible and 0 otherwise, so the layout
// budget in model.go is correct.
type AlertBannerComponent struct {
	rest    rateLimitAlertState
	graphql rateLimitAlertState
	now     time.Time
}

// isVisible returns true when either bucket's banner should be shown.
func (a AlertBannerComponent) isVisible() bool {
	return a.rest.isVisible(a.now) || a.graphql.isVisible(a.now)
}

// soonerReset returns whichever of the two reset times is earlier, treating
// a zero (unknown) reset as "no opinion" rather than as the earliest
// possible time — so a bucket with an unknown reset never wins the
// comparison against a bucket with a known one.
func soonerReset(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if b.Before(a) {
		return b
	}
	return a
}

// Update handles events relevant to the alert banner.
func (a AlertBannerComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case RateLimitAlertEvent:
		state := &a.graphql
		if ev.Bucket == RateLimitBucketREST {
			state = &a.rest
		}
		if ev.Exhausted {
			state.active = true
			if !ev.Reset.IsZero() {
				state.reset = ev.Reset
			}
		} else {
			state.active = false
			state.reset = time.Time{}
		}
	case TickEvent:
		a.now = ev.At
	}
	return a, nil
}

// Height returns 1 when the banner is visible, 0 otherwise.
func (a AlertBannerComponent) Height() int {
	if a.isVisible() {
		return 1
	}
	return 0
}

// View renders the alert banner at the given width. When only one bucket is
// exhausted, it names that bucket and quotes its own reset time. When both
// are exhausted simultaneously, it renders a single combined line naming
// both (R2's "or names both") and quotes whichever bucket's reset comes
// sooner — the operationally useful choice, since that's the earliest
// moment anything could change — while preserving the Height()==1 invariant.
func (a AlertBannerComponent) View(width int) string {
	restVisible := a.rest.isVisible(a.now)
	graphqlVisible := a.graphql.isVisible(a.now)
	if !restVisible && !graphqlVisible {
		return ""
	}

	var text string
	switch {
	case restVisible && graphqlVisible:
		text = "⚠ REST and GraphQL rate limits exhausted — polling suspended."
		if countdown := fmtBannerCountdown(soonerReset(a.rest.reset, a.graphql.reset), a.now); countdown != "" {
			text += " " + countdown
		}
	case restVisible:
		text = "⚠ REST rate limit exhausted — polling suspended."
		if countdown := fmtBannerCountdown(a.rest.reset, a.now); countdown != "" {
			text += " " + countdown
		}
	default:
		text = "⚠ GraphQL rate limit exhausted — polling suspended."
		if countdown := fmtBannerCountdown(a.graphql.reset, a.now); countdown != "" {
			text += " " + countdown
		}
	}

	// Truncate to a single line so Height() == 1 is always accurate on
	// narrow terminals where the text would otherwise wrap.
	if width > 0 && lipgloss.Width(text) > width {
		runes := []rune(text)
		for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > width {
			runes = runes[:len(runes)-1]
		}
		text = string(runes) + "…"
	}
	return failStyle.Bold(true).Width(width).Render(text)
}
