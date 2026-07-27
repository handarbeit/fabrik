package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// maxHistoryEntries bounds HistoryPaneComponent's in-memory ring buffer.
// Deliberate divergence from Fabrik's tui/history.go (unbounded, persisted
// to .fabrik/history.json): Pruefer is a long-running daemon (weeks/months
// uptime), not an interactively-restarted CLI, so unbounded+persisted
// history would grow without limit and add disk I/O this package doesn't
// need — see adrs/1114-pruefer-tui-architecture.md.
const maxHistoryEntries = 200

// HistoryEntry records one completed ReviewPR outcome — reviewed, skipped,
// or errored, unified into a single shape (mirroring how Fabrik's own
// HistoryEntry unifies success/error/blocked-on-input) rather than a
// separate skip-only pane.
type HistoryEntry struct {
	Repo        string
	PRNumber    int
	Title       string
	Reviewed    bool
	Skipped     bool
	Reason      string // set iff Skipped; one of pruefer.SkipReason's values
	Err         string // set iff a genuine failure occurred; empty otherwise
	NumTurns    int
	CostUSD     float64
	Duration    time.Duration
	CompletedAt time.Time
}

// HistoryPaneComponent manages the completed-reviews pane: a bounded,
// in-memory-only ring buffer of the most recent maxHistoryEntries entries.
type HistoryPaneComponent struct {
	entries []HistoryEntry // oldest first
	idx     int            // selection index; 0 = most recent
	focused bool
}

func (h HistoryPaneComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case ReviewCompletedEvent:
		entry := HistoryEntry{
			Repo:        ev.Repo,
			PRNumber:    ev.PRNumber,
			Title:       ev.Title,
			Reviewed:    ev.Reviewed,
			Skipped:     ev.Skipped,
			Reason:      ev.Reason,
			Err:         ev.Err,
			NumTurns:    ev.NumTurns,
			CostUSD:     ev.CostUSD,
			Duration:    ev.Duration,
			CompletedAt: ev.CompletedAt,
		}
		h.entries = append(h.entries, entry)
		if len(h.entries) > maxHistoryEntries {
			h.entries = h.entries[len(h.entries)-maxHistoryEntries:]
		}

	case tea.KeyMsg:
		if !h.focused {
			return h, nil
		}
		switch ev.String() {
		case "up", "k":
			if h.idx > 0 {
				h.idx--
			}
		case "down", "j":
			if h.idx < len(h.entries)-1 {
				h.idx++
			}
		}
	}
	return h, nil
}

func (h HistoryPaneComponent) View(width int) string {
	focusIndicator := " "
	if h.focused {
		focusIndicator = "▸"
	}
	title := dimStyle.Render(fmt.Sprintf("%s Completed Reviews (%d)", focusIndicator, len(h.entries)))

	maxWidth := width - 6
	if maxWidth < 20 {
		maxWidth = 20
	}
	var lines []string
	for i := len(h.entries) - 1; i >= 0; i-- {
		e := h.entries[i]
		var status, extra string
		switch {
		case e.Err != "":
			status = failStyle.Render("✗")
			extra = dimStyle.Render("  " + e.Err)
		case e.Skipped:
			status = activeStyle.Render("○")
			extra = dimStyle.Render("  skipped: " + e.Reason)
		default:
			status = successStyle.Render("✓")
			parts := []string{}
			if e.NumTurns > 0 {
				parts = append(parts, fmt.Sprintf("%d turns", e.NumTurns))
			}
			if e.CostUSD > 0 {
				parts = append(parts, fmt.Sprintf("$%.4f", e.CostUSD))
			}
			if len(parts) > 0 {
				extra = dimStyle.Render("  " + strings.Join(parts, " "))
			}
		}
		ts := dimStyle.Render(e.CompletedAt.Format("15:04:05"))
		dur := fmtDuration(e.Duration)
		key := activeReviewKey(e.Repo, e.PRNumber)
		titleStr := ""
		if e.Title != "" {
			titleStr = "  " + dimStyle.Render(e.Title)
		}
		line := fmt.Sprintf("%s %-24s %s %s%s%s", status, key, ts, dur, extra, titleStr)
		displayIdx := len(h.entries) - 1 - i
		if h.focused && displayIdx == h.idx {
			line = selectedStyle.Render(line)
		}
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, dimStyle.Render("no completed reviews yet"))
	}

	// Window the display around the selection so Height() (which caps at
	// maxDisplayedHistoryRows) always matches what View() actually renders —
	// mirrors tui/active.go's windowing-with-"… N more" fallback.
	total := len(lines)
	if total > maxDisplayedHistoryRows {
		maxRows := maxDisplayedHistoryRows
		start := h.idx - maxRows/2
		if start < 0 {
			start = 0
		}
		if start+maxRows > total {
			start = total - maxRows
		}
		if start > 0 || start+maxRows < total {
			maxRows--
		}
		windowed := lines[start : start+maxRows]
		if start > 0 || start+maxRows < total {
			windowed = append(windowed, dimStyle.Render(fmt.Sprintf("  … %d more", total-maxRows)))
		}
		lines = windowed
	}

	content := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

// maxDisplayedHistoryRows caps the number of history rows rendered at once;
// older entries remain retained (up to maxHistoryEntries) and selectable,
// just scrolled out of view.
const maxDisplayedHistoryRows = 10

func (h HistoryPaneComponent) Height() int {
	n := len(h.entries)
	if n == 0 {
		n = 1
	}
	if n > maxDisplayedHistoryRows {
		n = maxDisplayedHistoryRows
	}
	return n + 3
}

// SetFocused updates the focused state.
func (h *HistoryPaneComponent) SetFocused(f bool) {
	h.focused = f
}

// Selected returns the currently selected history entry, or nil if none.
// Index 0 is the most recently completed review.
func (h HistoryPaneComponent) Selected() *HistoryEntry {
	if len(h.entries) == 0 {
		return nil
	}
	realIdx := len(h.entries) - 1 - h.idx
	if realIdx >= 0 && realIdx < len(h.entries) {
		return &h.entries[realIdx]
	}
	return nil
}

// Count returns the number of completed-review entries currently retained.
func (h HistoryPaneComponent) Count() int {
	return len(h.entries)
}

// Entries returns the retained history entries, oldest first.
func (h HistoryPaneComponent) Entries() []HistoryEntry {
	return h.entries
}
