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
	pollInterval      time.Duration
	effectiveInterval time.Duration // last computed effective interval (includes idle/rate-limit backoff)
	nextPollAt        time.Time
	now               time.Time
	fabrikVersion     string
	statusLine        string
	statusMsg         string
	skillsStaleCount  int  // number of plugin skill files differing from embedded; 0 = up to date
	customWorkflow    bool // operator has local customizations in .fabrik/plugin/
}

func (h HeaderComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		h.now = ev.At
		h.statusMsg = ""
	case PollStartedEvent:
		interval := h.effectiveInterval
		if interval == 0 {
			interval = h.pollInterval
		}
		h.nextPollAt = time.Now().Add(interval)
	case PollCompletedEvent:
		if ev.EffectiveInterval > 0 {
			h.effectiveInterval = ev.EffectiveInterval
			h.nextPollAt = time.Now().Add(ev.EffectiveInterval)
		} else {
			h.nextPollAt = time.Now().Add(h.pollInterval)
		}
	case LogEvent:
		if ev.IssueNumber == 0 {
			h.statusLine = fmt.Sprintf("[%s] %s", ev.Tag, strings.TrimRight(ev.Message, "\n"))
		}
	case SkillsStaleEvent:
		h.skillsStaleCount = ev.Count
	case CustomWorkflowEvent:
		h.customWorkflow = true
		h.skillsStaleCount = 0
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

	// Pre-compute badge so its width is factored into the truncation budget.
	// customWorkflow takes priority over skillsStaleCount — they are mutually exclusive.
	badge := ""
	badgeWidth := 0
	if h.customWorkflow {
		badge = dimStyle.Render("  [u] custom workflow")
		badgeWidth = lipgloss.Width(badge)
	} else if h.skillsStaleCount > 0 {
		badge = dimStyle.Render("  [u] skills out of date")
		badgeWidth = lipgloss.Width(badge)
	}

	left := title + status
	leftWidth := lipgloss.Width(left)
	timerWidth := lipgloss.Width(timerStr)
	available := width - 4
	if leftWidth+timerWidth+badgeWidth > available {
		maxStatus := max(available-lipgloss.Width(title)-timerWidth-badgeWidth-3, 0)
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
	if badge != "" {
		left = left + badge
		leftWidth += badgeWidth
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

// SetSkillsStaleCount sets the number of plugin skill files that differ from
// the embedded versions. When n > 0, a persistent badge is shown in the header.
func (h *HeaderComponent) SetSkillsStaleCount(n int) {
	h.skillsStaleCount = n
}

// SetCustomWorkflow sets the custom workflow state. When true, a persistent
// [u] custom workflow badge is shown (with priority over skillsStaleCount).
func (h *HeaderComponent) SetCustomWorkflow(v bool) {
	h.customWorkflow = v
}
