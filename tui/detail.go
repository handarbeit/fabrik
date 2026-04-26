package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DetailItem is a union type holding fields needed for detail panel rendering.
// Constructed by the root model from whichever pane is focused.
type DetailItem struct {
	IssueNumber    int
	Title          string
	StageName      string
	StageModel     string
	IsActive       bool // true for in-flight jobs, false for history entries
	Elapsed        time.Duration
	Duration       time.Duration
	Success        bool
	Completed      bool
	BlockedOnInput bool
	TurnsUsed      int
	MaxTurns       int
	CostUSD        float64
	CompletedAt    time.Time
}

// DetailPanelComponent renders metadata for a selected item.
// It is a pure view component — Update is a no-op.
type DetailPanelComponent struct {
	item      *DetailItem
	visible   bool
	lastWidth int
}

func (d DetailPanelComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	return d, nil
}

func (d DetailPanelComponent) View(width int) string {
	if !d.visible || d.item == nil {
		return ""
	}

	var lines []string
	item := d.item

	if item.IsActive {
		lines = append(lines, fmt.Sprintf("Issue:   #%d", item.IssueNumber))
		if item.Title != "" {
			lines = append(lines, fmt.Sprintf("Title:   %s", item.Title))
		}
		lines = append(lines, fmt.Sprintf("Stage:   %s", item.StageName))
		lines = append(lines, fmt.Sprintf("Elapsed: %s", fmtDuration(item.Elapsed)))
		if item.TurnsUsed > 0 {
			if item.MaxTurns > 0 {
				lines = append(lines, fmt.Sprintf("Turns:   %d/%d", item.TurnsUsed, item.MaxTurns))
			} else {
				lines = append(lines, fmt.Sprintf("Turns:   %d", item.TurnsUsed))
			}
		}
	} else {
		statusStr := "success"
		if !item.Success {
			statusStr = "error"
		} else if item.BlockedOnInput {
			statusStr = "awaiting input"
		} else if !item.Completed {
			statusStr = "incomplete"
		}
		lines = append(lines, fmt.Sprintf("Issue:    #%d", item.IssueNumber))
		if item.Title != "" {
			lines = append(lines, fmt.Sprintf("Title:    %s", item.Title))
		}
		lines = append(lines, fmt.Sprintf("Stage:    %s", item.StageName))
		if item.StageModel != "" {
			lines = append(lines, fmt.Sprintf("Model:    %s", item.StageModel))
		}
		lines = append(lines, fmt.Sprintf("Status:   %s", statusStr))
		lines = append(lines, fmt.Sprintf("Duration: %s", fmtDuration(item.Duration)))
		if item.TurnsUsed > 0 {
			if item.MaxTurns > 0 {
				lines = append(lines, fmt.Sprintf("Turns:    %d/%d", item.TurnsUsed, item.MaxTurns))
			} else {
				lines = append(lines, fmt.Sprintf("Turns:    %d", item.TurnsUsed))
			}
		}
		if item.CostUSD > 0 {
			lines = append(lines, fmt.Sprintf("Cost:     $%.4f", item.CostUSD))
		}
		if !item.CompletedAt.IsZero() {
			lines = append(lines, fmt.Sprintf("At:       %s", item.CompletedAt.Format("2006-01-02 15:04:05")))
		}
	}

	if len(lines) == 0 {
		return ""
	}

	titleLine := dimStyle.Render("Detail") + "  " + dimStyle.Render("[esc] close")
	content := titleLine + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

func (d DetailPanelComponent) Height() int {
	if !d.visible || d.item == nil {
		return 0
	}
	w := d.lastWidth
	if w == 0 {
		w = 80
	}
	view := d.View(w)
	if view == "" {
		return 0
	}
	return strings.Count(view, "\n") + 1
}

func (d DetailPanelComponent) HandleClick(x, y int) bool {
	return false
}

// SetItem updates the item displayed in the detail panel.
func (d *DetailPanelComponent) SetItem(item *DetailItem) {
	d.item = item
}

// SetWidth records the terminal width for use in Height() calculations.
func (d *DetailPanelComponent) SetWidth(w int) {
	d.lastWidth = w
}

// SetVisible controls whether the detail panel is shown.
func (d *DetailPanelComponent) SetVisible(v bool) {
	d.visible = v
}

// IsVisible returns whether the detail panel is shown.
func (d DetailPanelComponent) IsVisible() bool {
	return d.visible
}
