package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ClaudeUsageLimitBannerComponent renders a single high-visibility row while
// Claude dispatch is suspended account-wide after a usage-limit hit
// (ADR-1120). It implements the Component interface and mirrors
// AlertBannerComponent's shape, but is fully independent — the GitHub
// rate-limit banner and this one track unrelated conditions and can both be
// visible at once. Height() returns 1 when the banner is visible and 0
// otherwise, so the layout budget in model.go is correct.
type ClaudeUsageLimitBannerComponent struct {
	active bool
	reset  time.Time
	now    time.Time
}

// isVisible returns true when the banner should be shown: a suspension is
// active and (when a reset time is known) now hasn't reached it yet. This
// lets the banner self-clear off the per-second TickEvent fan-out even if no
// explicit ClaudeUsageLimitAlertEvent{Suspended: false} ever arrives (e.g. no
// item was up for dispatch during the window) — see ADR-1120 on why this is
// a harmless cosmetic-only lag relative to the engine's own lazy check.
func (c ClaudeUsageLimitBannerComponent) isVisible() bool {
	if !c.active {
		return false
	}
	if !c.reset.IsZero() && !c.now.Before(c.reset) {
		return false
	}
	return true
}

// Update handles events relevant to the usage-limit banner.
func (c ClaudeUsageLimitBannerComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case ClaudeUsageLimitAlertEvent:
		if ev.Suspended {
			c.active = true
			c.reset = ev.Reset
		} else {
			c.active = false
			c.reset = time.Time{}
		}
	case TickEvent:
		c.now = ev.At
	}
	return c, nil
}

// Height returns 1 when the banner is visible, 0 otherwise.
func (c ClaudeUsageLimitBannerComponent) Height() int {
	if c.isVisible() {
		return 1
	}
	return 0
}

// View renders the usage-limit banner at the given width.
func (c ClaudeUsageLimitBannerComponent) View(width int) string {
	if !c.isVisible() {
		return ""
	}
	text := "⚠ Claude usage limit hit — dispatch suspended."
	countdown := fmtBannerCountdown(c.reset, c.now)
	if countdown != "" {
		text += " " + countdown
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
