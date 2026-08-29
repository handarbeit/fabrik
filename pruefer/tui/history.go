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

	// maxRows is the row budget the layout has granted this pane, set by
	// Model.View from the terminal height (#1674 R1). Zero means "unset" and
	// falls back to defaultHistoryRows, so a zero-value component still
	// renders sensibly in tests and before the first WindowSizeMsg.
	maxRows int
}

// visibleEntries returns the entries the pane actually displays: completed
// reviews and failures, with plain skips filtered out (#1674 R2).
//
// A skip is the daemon working correctly — "already reviewed at this head SHA"
// is the steady state of a healthy daemon, and at 15 watched repos it is the
// overwhelming majority of outcomes. Left unfiltered it evicts the rows that
// carry information.
//
// An errored entry is never filtered, even if Skipped is somehow also set: a
// failed review is the most important row on this pane, and sweeping it away
// with the skips is the obvious way to get this wrong.
func (h HistoryPaneComponent) visibleEntries() []HistoryEntry {
	out := make([]HistoryEntry, 0, len(h.entries))
	for _, e := range h.entries {
		if e.Skipped && e.Err == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// skippedCount reports how many retained entries visibleEntries filtered out,
// so the pane can disclose them rather than hiding them silently.
func (h HistoryPaneComponent) skippedCount() int {
	return len(h.entries) - len(h.visibleEntries())
}

// rowBudget is the number of content rows this pane may render.
func (h HistoryPaneComponent) rowBudget() int {
	if h.maxRows > 0 {
		return h.maxRows
	}
	return defaultHistoryRows
}

// SetMaxRows sets the row budget granted by the layout (#1674 R1).
func (h *HistoryPaneComponent) SetMaxRows(n int) {
	if n < minPaneRows {
		n = minPaneRows
	}
	h.maxRows = n
}

// elideMiddle shortens s to at most max runes by removing from the middle,
// preserving both ends. Used for "owner/repo#N" keys, where the tail (the PR
// number) is the identifying part and a plain right-truncation destroys it
// (#1674 R3).
func elideMiddle(s string, max int) string {
	r := []rune(s)
	if max < 3 || len(r) <= max {
		return s
	}
	keep := max - 1
	head := (keep + 1) / 2
	tail := keep - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
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
			// Bound against the visible list, not h.entries: skips are
			// filtered out of the pane (#1674 R2), so h.idx is a position
			// within what View actually renders. Bounding on h.entries would
			// let the selection run past the last visible row.
			if h.idx < len(h.visibleEntries())-1 {
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
	entries := h.visibleEntries()
	titleText := fmt.Sprintf("%s Completed Reviews (%d)", focusIndicator, len(entries))
	if n := h.skippedCount(); n > 0 {
		// Disclose rather than hide: the skips are still counted, just not
		// occupying rows (#1674 R2).
		titleText += fmt.Sprintf(" · %d skipped", n)
	}
	title := dimStyle.Render(titleText)

	maxWidth := width - 6
	if maxWidth < 20 {
		maxWidth = 20
	}

	// Column widths are computed from the rendered set rather than fixed, so
	// entries of differing name length line up (#1674 R3). keyCol is capped so
	// one very long repo name cannot push every other column off-screen.
	keyCol := 0
	for _, e := range entries {
		if n := len([]rune(activeReviewKey(e.Repo, e.PRNumber))); n > keyCol {
			keyCol = n
		}
	}
	if keyCol > maxKeyColumn {
		keyCol = maxKeyColumn
	}

	var lines []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		var status, extra string
		switch {
		case e.Err != "":
			status = failStyle.Render("✗")
			extra = dimStyle.Render("  " + e.Err)
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
		key := elideMiddle(activeReviewKey(e.Repo, e.PRNumber), keyCol)
		// Pad on the rune count, not the byte count: an elided key contains a
		// multi-byte "…" and %-*s would under-pad it.
		key += strings.Repeat(" ", keyCol-len([]rune(key)))
		titleStr := ""
		if e.Title != "" {
			titleStr = "  " + dimStyle.Render(e.Title)
		}
		line := fmt.Sprintf("%s %s %s %5s%s%s", status, key, ts, dur, extra, titleStr)
		displayIdx := len(entries) - 1 - i
		if h.focused && displayIdx == h.idx {
			line = selectedStyle.Render(line)
		}
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		msg := "no completed reviews yet"
		if h.skippedCount() > 0 {
			// Distinguish "nothing has happened" from "everything so far was a
			// skip" — a bare empty pane would misrepresent an active daemon.
			msg = fmt.Sprintf("no completed reviews yet (%d skipped)", h.skippedCount())
		}
		lines = append(lines, dimStyle.Render(msg))
	}

	// Window the display around the selection so Height() always matches what
	// View() actually renders — mirrors tui/active.go's windowing-with-"… N
	// more" fallback.
	budget := h.rowBudget()
	total := len(lines)
	if total > budget {
		maxRows := budget
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
	// Stretch to the granted budget so the layout fills the terminal instead
	// of leaving dead space below it (#1674 R1). Only when a budget was
	// granted: an ungranted pane keeps its natural height.
	if h.maxRows > 0 {
		for len(lines) < budget {
			lines = append(lines, "")
		}
	}

	content := title + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

// defaultHistoryRows is the row budget used before the layout has granted one
// (no WindowSizeMsg yet, or a zero-value component in tests). Older entries
// remain retained (up to maxHistoryEntries) and selectable, just scrolled out
// of view.
const defaultHistoryRows = 10

// maxKeyColumn caps the computed repo#PR column so one very long name cannot
// push the remaining columns off-screen.
const maxKeyColumn = 34

func (h HistoryPaneComponent) Height() int {
	// With a granted budget, View pads to exactly that many rows, so the
	// height is the budget regardless of how many entries exist. Reporting
	// min(entries, budget) here would under-report the rendered height — e.g.
	// budget 20 with 3 visible entries renders 23 rows, not 6.
	if h.maxRows > 0 {
		return h.rowBudget() + 3
	}
	n := len(h.visibleEntries())
	if n == 0 {
		n = 1
	}
	if b := h.rowBudget(); n > b {
		n = b
	}
	return n + 3
}

// SetFocused updates the focused state.
func (h *HistoryPaneComponent) SetFocused(f bool) {
	h.focused = f
}

// Selected returns the currently selected history entry, or nil if none.
// Index 0 is the most recently completed review.
// Selected returns the entry the pane is highlighting, indexed within the
// *visible* entries. h.idx is a display position: View renders visibleEntries()
// newest-first and highlights the row at h.idx, so Selected must resolve
// against that same list or the highlighted row and the detail panel disagree
// as soon as a single skip is filtered out.
func (h HistoryPaneComponent) Selected() *HistoryEntry {
	visible := h.visibleEntries()
	if len(visible) == 0 {
		return nil
	}
	realIdx := len(visible) - 1 - h.idx
	if realIdx >= 0 && realIdx < len(visible) {
		return &visible[realIdx]
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
