package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// FooterComponent renders the bottom status bar: project info and rate limit stats.
type FooterComponent struct {
	projectInfo  ProjectInfo
	graphqlStats RateLimitStats
	now          time.Time
}

func (f FooterComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		f.now = ev.At
	case PollCompletedEvent:
		if ev.GraphQLStats.Limit > 0 {
			f.graphqlStats = ev.GraphQLStats
		}
	case ProjectMetaEvent:
		f.projectInfo.BoardTitle = ev.BoardTitle
		f.projectInfo.BoardURL = ev.BoardURL
	}
	return f, nil
}

// supportsOSC8 returns true when the terminal is known to support OSC 8 hyperlinks.
// Detection uses environment variables because no universal runtime query exists.
func supportsOSC8() bool {
	prog := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	switch prog {
	case "iterm.app", "ghostty", "wezterm", "kitty":
		return true
	}
	if strings.Contains(strings.ToLower(os.Getenv("TERM")), "kitty") {
		return true
	}
	return false
}

// applyOSC8 wraps the first occurrence of title in plain with an OSC 8 hyperlink
// pointing to boardURL. If title or boardURL is empty, or if the terminal does not
// support OSC 8, plain is returned unchanged.
func applyOSC8(plain, title, boardURL string) string {
	if title == "" || boardURL == "" || !supportsOSC8() {
		return plain
	}
	return strings.Replace(plain, title, termenv.Hyperlink(boardURL, title), 1)
}

func (f FooterComponent) View(width int) string {
	// Assemble left side: CWD [· BoardTitle] [· version]
	parts := []string{}
	if f.projectInfo.CWD != "" {
		parts = append(parts, f.projectInfo.CWD)
	}
	if f.projectInfo.BoardTitle != "" {
		parts = append(parts, f.projectInfo.BoardTitle)
	}
	if f.projectInfo.Version != "" {
		parts = append(parts, f.projectInfo.Version)
	}
	leftPlain := strings.Join(parts, " · ")

	var rightStr string
	if f.graphqlStats.Limit > 0 {
		countdown := fmtRateLimitCountdown(f.graphqlStats.Reset, f.now)
		plain := fmt.Sprintf("%d/%d  %s", f.graphqlStats.Remaining, f.graphqlStats.Limit, countdown)
		pct := float64(f.graphqlStats.Remaining) / float64(f.graphqlStats.Limit)
		var style lipgloss.Style
		switch {
		case pct > 0.5:
			style = successStyle
		case pct > 0.2:
			style = activeStyle
		default:
			style = failStyle
		}
		rightStr = style.Render(plain)
	}

	maxWidth := width - 2 // leave 1 char margin on each side
	if maxWidth < 1 {
		maxWidth = 1
	}

	if rightStr == "" {
		footer := dimStyle.Render(applyOSC8(leftPlain, f.projectInfo.BoardTitle, f.projectInfo.BoardURL))
		if lipgloss.Width(footer) > maxWidth {
			runes := []rune(leftPlain)
			for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > maxWidth {
				runes = runes[:len(runes)-1]
			}
			footer = dimStyle.Render(applyOSC8(string(runes)+"…", f.projectInfo.BoardTitle, f.projectInfo.BoardURL))
		}
		return footer
	}

	rightWidth := lipgloss.Width(rightStr)
	leftRendered := dimStyle.Render(applyOSC8(leftPlain, f.projectInfo.BoardTitle, f.projectInfo.BoardURL))
	gap := maxWidth - lipgloss.Width(leftRendered) - rightWidth
	if gap < 1 {
		availLeft := maxWidth - rightWidth - 1
		if availLeft < 0 {
			availLeft = 0
		}
		runes := []rune(leftPlain)
		if availLeft == 0 {
			leftRendered = ""
		} else {
			for len(runes) > 0 && lipgloss.Width(dimStyle.Render(string(runes)+"…")) > availLeft {
				runes = runes[:len(runes)-1]
			}
			if len(runes) == 0 {
				leftRendered = ""
			} else {
				leftRendered = dimStyle.Render(applyOSC8(string(runes)+"…", f.projectInfo.BoardTitle, f.projectInfo.BoardURL))
			}
		}
		gap = maxWidth - lipgloss.Width(leftRendered) - rightWidth
		if gap < 0 {
			gap = 0
		}
	}
	return leftRendered + strings.Repeat(" ", gap) + rightStr
}

func (f FooterComponent) Height() int {
	return 1
}

func (f FooterComponent) HandleClick(x, y int) bool {
	return false
}
