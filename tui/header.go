package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HeaderComponent renders the top status bar: title, status line, and poll timer.
type HeaderComponent struct {
	pollInterval  time.Duration
	nextPollAt    time.Time
	now           time.Time
	fabrikVersion string
	statusLine    string
	statusMsg     string
}

func (h HeaderComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		h.now = ev.At
		h.statusMsg = ""
	case PollStartedEvent:
		h.nextPollAt = time.Now().Add(h.pollInterval)
	case PollCompletedEvent:
		h.nextPollAt = time.Now().Add(h.pollInterval)
	case LogEvent:
		if ev.IssueNumber == 0 {
			h.statusLine = fmt.Sprintf("[%s] %s", ev.Tag, strings.TrimRight(ev.Message, "\n"))
		}
	}
	return h, nil
}

func (h HeaderComponent) View(width int) string {
	var timer string
	remaining := time.Until(h.nextPollAt)
	if remaining <= 0 {
		timer = "polling..."
	} else {
		timer = fmt.Sprintf("poll in %s", fmtDuration(remaining))
	}

	title := titleStyle.Render("fabrik")
	if h.fabrikVersion != "" {
		title = title + " " + dimStyle.Render(h.fabrikVersion)
	}
	timerStr := dimStyle.Render(timer)

	displayStatus := h.statusMsg
	if displayStatus == "" {
		displayStatus = h.statusLine
	}

	status := ""
	if displayStatus != "" {
		status = "  " + dimStyle.Render(displayStatus)
	}

	left := title + status
	leftWidth := lipgloss.Width(left)
	timerWidth := lipgloss.Width(timerStr)
	available := width - 4
	if leftWidth+timerWidth > available {
		maxStatus := max(available-lipgloss.Width(title)-timerWidth-3, 0)
		if maxStatus > 0 && displayStatus != "" {
			s := displayStatus
			for lipgloss.Width(s) > maxStatus {
				runes := []rune(s)
				if len(runes) == 0 {
					break
				}
				s = string(runes[:len(runes)-1])
			}
			s += "…"
			status = "  " + dimStyle.Render(s)
		} else {
			status = ""
		}
		left = title + status
		leftWidth = lipgloss.Width(left)
	}
	gap := max(width-4-leftWidth-timerWidth, 0)
	return " " + left + strings.Repeat(" ", gap) + timerStr
}

func (h HeaderComponent) Height() int {
	return 1
}

func (h HeaderComponent) HandleClick(x, y int) bool {
	return false
}

// SetStatusMsg sets a transient status message shown in the header.
func (h *HeaderComponent) SetStatusMsg(msg string) {
	h.statusMsg = msg
}
