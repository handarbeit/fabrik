package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/handarbeit/fabrik/warnings"
)

const maxWarningRows = 3
const maxDetailLines = 10

// WarningsPaneComponent renders the persistent pre-flight warnings panel.
type WarningsPaneComponent struct {
	entries        []warnings.Entry
	curIdx         int
	showDismissed  bool
	expandedDetail bool
	focused        bool
	lastMtime      time.Time
	width          int
	// availableH is the vertical space allocated by updateLayout.
	// -1 means SetLayout has not been called yet.
	// 0 means no space available (View returns "", Height returns 0).
	availableH int
}

// NewWarningsPaneComponent creates a WarningsPaneComponent loaded from disk.
func NewWarningsPaneComponent() WarningsPaneComponent {
	c := WarningsPaneComponent{availableH: -1}
	entries, err := warnings.Load()
	if err != nil {
		// Corrupt file — treat as empty, errors are logged by Load's caller.
		entries = nil
	}
	c.entries = entries
	if info, err := os.Stat(warnings.Path()); err == nil {
		c.lastMtime = info.ModTime()
	}
	return c
}

// Update handles messages for the warnings pane.
func (c WarningsPaneComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		info, err := os.Stat(warnings.Path())
		if err != nil {
			// File disappeared — treat as empty and reset mtime so a
			// recreated file with any timestamp is detected on next tick.
			if len(c.entries) > 0 {
				c.entries = nil
				c.curIdx = 0
			}
			c.lastMtime = time.Time{}
			break
		}
		if info.ModTime().After(c.lastMtime) {
			c.lastMtime = info.ModTime()
			entries, loadErr := warnings.Load()
			if loadErr == nil {
				c.entries = entries
			}
			// Clamp curIdx after reload.
			visible := c.visibleEntries()
			if c.curIdx >= len(visible) && c.curIdx > 0 {
				c.curIdx = max(len(visible)-1, 0)
			}
		}

	case tea.KeyMsg:
		if !c.focused {
			break
		}
		visible := c.visibleEntries()
		switch ev.String() {
		case "up", "k":
			if c.curIdx > 0 {
				c.curIdx--
				c.expandedDetail = false
			}
		case "down", "j":
			if c.curIdx < len(visible)-1 {
				c.curIdx++
				c.expandedDetail = false
			}
		case "d", "D":
			if entry := c.currentVisible(); entry != nil {
				if entry.Dismissed {
					_ = warnings.Undismiss(entry.Key)
				} else {
					_ = warnings.Dismiss(entry.Key)
				}
				// Reload to reflect the change.
				if entries, err := warnings.Load(); err == nil {
					c.entries = entries
				}
				visible = c.visibleEntries()
				if c.curIdx >= len(visible) && c.curIdx > 0 {
					c.curIdx = max(len(visible)-1, 0)
				}
			}
		case "s", "S":
			c.showDismissed = !c.showDismissed
			visible = c.visibleEntries()
			if c.curIdx >= len(visible) && c.curIdx > 0 {
				c.curIdx = max(len(visible)-1, 0)
			}
		case "enter":
			if len(visible) > 0 {
				c.expandedDetail = !c.expandedDetail
			}
		}
	}
	return c, nil
}

// visibleEntries returns entries to show based on showDismissed flag.
func (c WarningsPaneComponent) visibleEntries() []warnings.Entry {
	if c.showDismissed {
		return c.entries
	}
	out := make([]warnings.Entry, 0, len(c.entries))
	for _, e := range c.entries {
		if !e.Dismissed {
			out = append(out, e)
		}
	}
	return out
}

// currentVisible returns the currently selected visible entry, or nil.
func (c WarningsPaneComponent) currentVisible() *warnings.Entry {
	visible := c.visibleEntries()
	if len(visible) == 0 || c.curIdx >= len(visible) {
		return nil
	}
	return &visible[c.curIdx]
}

// SelectedEntry returns the currently selected entry, or nil if none.
func (c WarningsPaneComponent) SelectedEntry() *warnings.Entry {
	return c.currentVisible()
}

// SetFocused updates the focused state.
func (c *WarningsPaneComponent) SetFocused(f bool) {
	c.focused = f
}

// SetLayout updates the available width and height.
func (c *WarningsPaneComponent) SetLayout(width, availableH int) {
	c.width = width
	c.availableH = availableH
}

// Height returns the rendered height of the warnings panel.
func (c WarningsPaneComponent) Height() int {
	if c.availableH == 0 {
		return 0
	}
	visible := c.visibleEntries()
	if len(visible) == 0 {
		return 1
	}
	rows := min(len(visible), maxWarningRows)
	h := 2 + 1 + rows // border (2) + title line (1) + entry rows
	if c.expandedDetail {
		entry := c.currentVisible()
		if entry != nil {
			detailLines := strings.Count(entry.Detail, "\n") + 1
			if detailLines > maxDetailLines {
				detailLines = maxDetailLines
			}
			h += detailLines + 1 // +1 for separator line
		}
	}
	return h
}

// View renders the warnings panel.
func (c WarningsPaneComponent) View(width int) string {
	if c.availableH == 0 {
		return ""
	}
	if width == 0 {
		width = c.width
	}
	if width == 0 {
		width = 80
	}
	visible := c.visibleEntries()

	if len(visible) == 0 {
		// Count dismissed entries for the footer hint.
		dismissedCount := 0
		for _, e := range c.entries {
			if e.Dismissed {
				dismissedCount++
			}
		}
		msg := "  Warnings: none"
		if dismissedCount > 0 {
			msg = fmt.Sprintf("  Warnings: none (%d dismissed, press tab + S to show)", dismissedCount)
		}
		return dimStyle.Render(msg)
	}

	innerWidth := max(width-6, 20)
	focusIndicator := " "
	if c.focused {
		focusIndicator = "▸"
	}

	// Header colour based on non-dismissed count.
	nonDismissedCount := 0
	for _, e := range c.entries {
		if !e.Dismissed {
			nonDismissedCount++
		}
	}
	var headerStyle lipgloss.Style
	switch {
	case nonDismissedCount >= 3:
		headerStyle = failStyle
	case nonDismissedCount >= 1:
		headerStyle = activeStyle
	default:
		headerStyle = dimStyle
	}

	titleText := fmt.Sprintf("%s Warnings (%d)", focusIndicator, nonDismissedCount)
	dismissedHint := ""
	dismissedCount := 0
	for _, e := range c.entries {
		if e.Dismissed {
			dismissedCount++
		}
	}
	if dismissedCount > 0 && !c.showDismissed {
		dismissedHint = dimStyle.Render(fmt.Sprintf("  %d dismissed [S to show]", dismissedCount))
	} else if c.showDismissed && dismissedCount > 0 {
		dismissedHint = dimStyle.Render("  [S to hide dismissed]")
	}
	var focusHint string
	if c.focused {
		focusHint = dimStyle.Render("  [F]ix  [D]ismiss  [enter] detail")
	}
	title := headerStyle.Render(titleText) + dismissedHint + focusHint

	var lines []string
	lines = append(lines, title)

	displayRows := min(len(visible), maxWarningRows)
	for i := 0; i < displayRows; i++ {
		e := visible[i]
		prefix := "  "
		if c.focused && i == c.curIdx {
			prefix = "▸ "
		}
		titleText := e.Title
		// Truncate title before applying styles so ANSI codes don't bloat visible length.
		maxTitleRunes := innerWidth - len([]rune(prefix)) - 3
		if maxTitleRunes < 5 {
			maxTitleRunes = 5
		}
		if runes := []rune(titleText); len(runes) > maxTitleRunes {
			titleText = string(runes[:maxTitleRunes]) + "…"
		}
		var label string
		if e.Dismissed {
			label = dimStyle.Render(prefix + "✓ " + titleText + " (dismissed)")
		} else {
			line := prefix + titleText
			if c.focused && i == c.curIdx {
				label = selectedStyle.Render(line)
			} else {
				label = line
			}
		}
		lines = append(lines, label)
	}

	// Expanded detail for the selected entry.
	if c.expandedDetail {
		entry := c.currentVisible()
		if entry != nil {
			lines = append(lines, dimStyle.Render(strings.Repeat("─", innerWidth)))
			detailLines := strings.Split(entry.Detail, "\n")
			if len(detailLines) > maxDetailLines {
				detailLines = detailLines[:maxDetailLines]
			}
			for _, dl := range detailLines {
				lines = append(lines, dimStyle.Render("  "+dl))
			}
		}
	}

	content := strings.Join(lines, "\n")
	rendered := borderStyle.Width(width - 4).Render(content)
	if c.availableH > 0 {
		rLines := strings.Split(rendered, "\n")
		if len(rLines) > c.availableH {
			return strings.Join(rLines[:c.availableH], "\n")
		}
	}
	return rendered
}
