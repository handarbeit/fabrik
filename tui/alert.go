package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// AlertBannerComponent renders a single high-visibility row when the GraphQL
// budget is exhausted or probe failures attributable to rate limiting have been
// observed. It implements the Component interface. Height() returns 1 when the
// banner is visible and 0 otherwise, so the layout budget in model.go is correct.
type AlertBannerComponent struct {
	bannerActive bool
	graphqlStats RateLimitStats
	now          time.Time
}

// isVisible returns true when the banner should be shown.
func (a AlertBannerComponent) isVisible() bool {
	if a.graphqlStats.Limit == 0 {
		return false
	}
	ratio := float64(a.graphqlStats.Remaining) / float64(a.graphqlStats.Limit)
	if ratio > 0.50 {
		return false
	}
	if a.graphqlStats.Remaining == 0 {
		return true
	}
	return a.bannerActive
}

// Update handles events relevant to the alert banner.
func (a AlertBannerComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case RateLimitAlertEvent:
		if ev.Exhausted {
			a.bannerActive = true
			if !ev.Reset.IsZero() {
				a.graphqlStats.Reset = ev.Reset
			}
		} else {
			a.bannerActive = false
		}
	case PollCompletedEvent:
		a.graphqlStats = ev.GraphQLStats
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

// View renders the alert banner at the given width.
func (a AlertBannerComponent) View(width int) string {
	if !a.isVisible() {
		return ""
	}
	text := "⚠ GraphQL rate limit exhausted — polling suspended."
	countdown := fmtBannerCountdown(a.graphqlStats.Reset, a.now)
	if countdown != "" {
		text += " " + countdown
	}
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("196")).
		Bold(true).
		Width(width)
	return style.Render(text)
}
