package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// DetailItem is a union type holding fields needed for detail panel
// rendering, constructed by the root model from whichever pane is focused —
// mirrors tui/detail.go's DetailItem.
type DetailItem struct {
	Repo        string
	PRNumber    int
	Title       string
	IsActive    bool // true for an in-flight review, false for a completed one
	Elapsed     time.Duration
	Duration    time.Duration
	Reviewed    bool
	Skipped     bool
	Reason      string
	Err         string
	NumTurns    int
	CostUSD     float64
	CompletedAt time.Time
}

// DetailPanelComponent renders metadata for a selected review. It is a pure
// view component — Update is a no-op, mirroring tui/detail.go.
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

	item := d.item
	var lines []string
	lines = append(lines, fmt.Sprintf("Review:  %s", activeReviewKey(item.Repo, item.PRNumber)))
	if item.Title != "" {
		lines = append(lines, fmt.Sprintf("Title:   %s", item.Title))
	}

	if item.IsActive {
		lines = append(lines, fmt.Sprintf("Elapsed: %s", fmtDuration(item.Elapsed)))
	} else {
		status := "reviewed"
		switch {
		case item.Err != "":
			status = "error: " + item.Err
		case item.Skipped:
			status = "skipped: " + item.Reason
		}
		lines = append(lines, fmt.Sprintf("Status:   %s", status))
		lines = append(lines, fmt.Sprintf("Duration: %s", fmtDuration(item.Duration)))
		if item.NumTurns > 0 {
			lines = append(lines, fmt.Sprintf("Turns:    %d", item.NumTurns))
		}
		if item.CostUSD > 0 {
			lines = append(lines, fmt.Sprintf("Cost:     $%.4f", item.CostUSD))
		}
		if !item.CompletedAt.IsZero() {
			lines = append(lines, fmt.Sprintf("At:       %s", item.CompletedAt.Format("2006-01-02 15:04:05")))
		}
	}

	titleLine := dimStyle.Render("Detail") + "  " + dimStyle.Render("[enter] close")
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
