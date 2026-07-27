package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Styles duplicated from Fabrik's tui/styles.go — unexported there, so
// structural mirroring means duplicating this small set rather than
// importing it (see adrs/1114-pruefer-tui-architecture.md).
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))

	borderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)

	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	activeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("238"))
)

// fmtDuration formats a duration as MM:SS, mirroring tui/commands.go.
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

// fmtRateLimitCountdown returns a human-readable string describing how long
// until the rate limit resets relative to now, mirroring tui/commands.go.
func fmtRateLimitCountdown(reset time.Time, now time.Time) string {
	remaining := reset.Sub(now)
	if remaining <= 0 {
		return "soon"
	}
	secs := int(remaining.Seconds())
	if secs >= 3600 {
		return fmt.Sprintf("%dh", secs/3600)
	}
	if secs >= 60 {
		return fmt.Sprintf("%dm", secs/60)
	}
	return fmt.Sprintf("%ds", secs)
}
