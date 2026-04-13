package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// HistoryPathOverride can be set by tests to redirect history I/O to a temp file.
var HistoryPathOverride string

// historyPath returns the path to the persistent history file.
// Uses .fabrik/history.json in the current working directory so each
// project has its own history.
func historyPath() string {
	if HistoryPathOverride != "" {
		return HistoryPathOverride
	}
	return filepath.Join(".fabrik", "history.json")
}

// historyDedupKey is the composite key used to identify duplicate history entries.
type historyDedupKey struct {
	IssueNumber int
	Repo        string
	StageName   string
	IsComment   bool
}

// deduplicateHistory collapses duplicate entries by (IssueNumber, Repo, StageName, IsComment),
// keeping the most recent entry by CompletedAt. Entries for different stages on the same
// issue are preserved — that is expected multi-stage pipeline behavior.
// The returned slice is sorted by CompletedAt ascending (oldest first).
func deduplicateHistory(entries []HistoryEntry) []HistoryEntry {
	best := make(map[historyDedupKey]HistoryEntry, len(entries))
	for _, e := range entries {
		k := historyDedupKey{
			IssueNumber: e.IssueNumber,
			Repo:        e.Repo,
			StageName:   e.StageName,
			IsComment:   e.IsComment,
		}
		if prev, exists := best[k]; !exists || e.CompletedAt.After(prev.CompletedAt) {
			best[k] = e
		}
	}
	seen := make(map[historyDedupKey]bool, len(best))
	out := make([]HistoryEntry, 0, len(best))
	for _, e := range entries {
		k := historyDedupKey{
			IssueNumber: e.IssueNumber,
			Repo:        e.Repo,
			StageName:   e.StageName,
			IsComment:   e.IsComment,
		}
		if !seen[k] && best[k].CompletedAt.Equal(e.CompletedAt) {
			out = append(out, e)
			seen[k] = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CompletedAt.Before(out[j].CompletedAt)
	})
	return out
}

// LoadHistory reads saved history entries from disk.
func LoadHistory() []HistoryEntry {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	var entries []HistoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return deduplicateHistory(entries)
}

// SaveHistory writes history entries to disk.
func SaveHistory(entries []HistoryEntry) {
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	dir := filepath.Dir(historyPath())
	os.MkdirAll(dir, 0700)
	os.WriteFile(historyPath(), data, 0600)
}

// HistoryPaneComponent manages the completed jobs history pane.
type HistoryPaneComponent struct {
	history      []HistoryEntry
	historyVP    viewport.Model
	histIdx      int
	focused      bool
	confirmClear bool
	// Layout state passed by root model via SetLayout for hint rendering.
	confirmQuit bool
	activeCount int
}

// NewHistoryPaneComponent creates a new HistoryPaneComponent with loaded history.
func NewHistoryPaneComponent() HistoryPaneComponent {
	vp := viewport.New(80, 10)
	return HistoryPaneComponent{
		history:   LoadHistory(),
		historyVP: vp,
	}
}

func (h HistoryPaneComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case JobCompletedEvent:
		entry := HistoryEntry{
			IssueNumber:    ev.IssueNumber,
			Repo:           ev.Repo,
			Title:          ev.Title,
			StageName:      ev.StageName,
			StageModel:     ev.StageModel,
			IsComment:      ev.IsComment,
			Success:        ev.Success,
			Completed:      ev.Completed,
			BlockedOnInput: ev.BlockedOnInput,
			Duration:       ev.Duration,
			CompletedAt:    ev.CompletedAt,
			TurnsUsed:      ev.TurnsUsed,
			MaxTurns:       ev.MaxTurns,
			CostUSD:        ev.CostUSD,
		}
		dupKey := historyDedupKey{
			IssueNumber: entry.IssueNumber,
			Repo:        entry.Repo,
			StageName:   entry.StageName,
			IsComment:   entry.IsComment,
		}
		filtered := make([]HistoryEntry, 0, len(h.history))
		for _, he := range h.history {
			k := historyDedupKey{
				IssueNumber: he.IssueNumber,
				Repo:        he.Repo,
				StageName:   he.StageName,
				IsComment:   he.IsComment,
			}
			if k != dupKey {
				filtered = append(filtered, he)
			}
		}
		h.history = append(filtered, entry)
		SaveHistory(h.history)

	case tea.KeyMsg:
		switch ev.String() {
		case "up", "k":
			if h.histIdx > 0 {
				h.histIdx--
			}
		case "down", "j":
			if h.histIdx < len(h.history)-1 {
				h.histIdx++
			}
		case "c":
			if len(h.history) > 0 {
				realIdx := len(h.history) - 1 - h.histIdx
				if realIdx >= 0 && realIdx < len(h.history) {
					h.history = append(h.history[:realIdx], h.history[realIdx+1:]...)
					SaveHistory(h.history)
					if h.histIdx >= len(h.history) && h.histIdx > 0 {
						h.histIdx--
					}
				}
			}
		case "C":
			if len(h.history) > 0 {
				if h.confirmClear {
					h.history = nil
					h.histIdx = 0
					h.confirmClear = false
					SaveHistory(h.history)
				} else {
					h.confirmClear = true
				}
			}
		case "n", "N":
			if h.confirmClear {
				h.confirmClear = false
			}
		case "l":
			if len(h.history) > 0 {
				realIdx := len(h.history) - 1 - h.histIdx
				if realIdx >= 0 && realIdx < len(h.history) {
					he := h.history[realIdx]
					return h, openWatchInlineCmd(he.IssueNumber, he.Repo)
				}
			}
		}

	}
	return h, nil
}

func (h HistoryPaneComponent) View(width int) string {
	innerWidth := max(width-6, 20)
	focusIndicator := " "
	if h.focused {
		focusIndicator = "▸"
	}
	title := dimStyle.Render(fmt.Sprintf("%s History (%d)", focusIndicator, len(h.history)))
	hint := h.historyHint(lipgloss.Width(title), innerWidth, h.confirmQuit, h.activeCount)
	content := title + hint + "\n" + h.historyVP.View()
	return borderStyle.Width(width - 4).Render(content)
}

func (h HistoryPaneComponent) Height() int {
	// Height is determined by SetLayout; return viewport height + overhead.
	return h.historyVP.Height + h.titleAndHintLines(0, false) + 2 // +2 for border
}

func (h *HistoryPaneComponent) HandleClick(x, y int) bool {
	// y is relative to the history pane's top (0-based).
	// border-top at y=0, title at y=1, viewport content starts at y=2.
	if y >= 2 {
		visibleRow := y - 2
		newHistIdx := visibleRow + h.historyVP.YOffset
		if newHistIdx >= 0 && newHistIdx < len(h.history) {
			h.histIdx = newHistIdx
			return true
		}
	}
	return y >= 0 && y <= 1
}

// SetLayout updates the viewport dimensions based on available space.
func (h *HistoryPaneComponent) SetLayout(width, availableHeight int, confirmQuit bool, activeCount int) {
	h.confirmQuit = confirmQuit
	h.activeCount = activeCount
	innerWidth := max(width-6, 20)

	// Rebuild viewport content.
	h.rebuildViewportContent(innerWidth)

	titleAndHintLines := h.titleAndHintLines(activeCount, confirmQuit)
	vpHeight := max(availableHeight-2-titleAndHintLines, 1) // -2 for border
	h.historyVP.Width = innerWidth
	h.historyVP.Height = vpHeight
}

// ScrollToTop scrolls the viewport to the top.
func (h *HistoryPaneComponent) ScrollToTop() {
	h.historyVP.GotoTop()
}

// ScrollToVisible ensures histIdx is visible within the viewport.
func (h *HistoryPaneComponent) ScrollToVisible() {
	if h.histIdx < h.historyVP.YOffset {
		h.historyVP.SetYOffset(h.histIdx)
	} else if h.histIdx > h.historyVP.YOffset+h.historyVP.Height-1 {
		h.historyVP.SetYOffset(h.histIdx - h.historyVP.Height + 1)
	}
}

func (h *HistoryPaneComponent) rebuildViewportContent(innerWidth int) {
	var lines []string
	for i := len(h.history) - 1; i >= 0; i-- {
		he := h.history[i]
		var status, result string
		if !he.Success {
			status = failStyle.Render("✗")
			result = dimStyle.Render("  (error)")
		} else if he.BlockedOnInput {
			status = activeStyle.Render("?")
			result = dimStyle.Render("  (input needed)")
		} else if !he.Completed {
			status = dimStyle.Render("↻")
			result = dimStyle.Render("  (retry)")
		} else {
			status = successStyle.Render("✓")
		}
		ts := dimStyle.Render(he.CompletedAt.Format("2006-01-02 15:04"))
		dur := fmtDuration(he.Duration)
		stats := ""
		if he.TurnsUsed > 0 || he.CostUSD > 0 {
			parts := []string{}
			if he.MaxTurns > 0 {
				parts = append(parts, fmt.Sprintf("%d/%d turns", he.TurnsUsed, he.MaxTurns))
			} else if he.TurnsUsed > 0 {
				parts = append(parts, fmt.Sprintf("%d turns", he.TurnsUsed))
			}
			if he.CostUSD > 0 {
				parts = append(parts, fmt.Sprintf("$%.2f", he.CostUSD))
			}
			stats = dimStyle.Render("  " + strings.Join(parts, " "))
		}
		titleStr := ""
		if he.Title != "" {
			maxTitle := max(innerWidth-60, 10)
			t := he.Title
			if runes := []rune(t); len(runes) > maxTitle {
				t = string(runes[:maxTitle]) + "…"
			}
			titleStr = "  " + dimStyle.Render(t)
		}
		stageDisplay := he.StageName
		if he.IsComment {
			stageDisplay += " 💬"
		}
		stagePad := 12 - lipgloss.Width(stageDisplay)
		if stagePad > 0 {
			stageDisplay += strings.Repeat(" ", stagePad)
		}
		line := fmt.Sprintf("#%-5d %s %s %s  %s%s%s%s",
			he.IssueNumber, stageDisplay, status, dur, ts, stats, result, titleStr)
		displayIdx := len(h.history) - 1 - i
		if h.focused && displayIdx == h.histIdx {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}
	h.historyVP.SetContent(strings.Join(lines, "\n"))
}

func (h HistoryPaneComponent) titleAndHintLines(activeCount int, confirmQuit bool) int {
	innerWidth := max(h.historyVP.Width, 20)
	focusIndicator := " "
	if h.focused {
		focusIndicator = "▸"
	}
	vpTitle := dimStyle.Render(fmt.Sprintf("%s History (%d)", focusIndicator, len(h.history)))
	vpHint := h.historyHint(lipgloss.Width(vpTitle), innerWidth, confirmQuit, activeCount)
	if lipgloss.Width(vpTitle+vpHint) > innerWidth {
		return 2
	}
	return 1
}

func (h HistoryPaneComponent) historyHint(titleDisplayWidth, innerWidth int, confirmQuit bool, activeCount int) string {
	maxHintWidth := max(innerWidth-titleDisplayWidth, 0)
	if maxHintWidth == 0 {
		return ""
	}

	var plainText string
	var style lipgloss.Style
	if h.confirmClear {
		plainText = "  Clear all history? [C]onfirm / [n]o"
		style = failStyle
	} else if confirmQuit {
		plainText = fmt.Sprintf("  Quit Fabrik? %d jobs still in progress \u2014 they will be interrupted.  [q] Quit anyway   [n/Escape] Cancel", activeCount)
		style = failStyle
	} else if h.focused && len(h.history) > 0 {
		plainText = "  [r]esume  [l] watch  [enter] details  [c]lear  [C]lear all  [tab] in-progress"
		style = dimStyle
	} else {
		return ""
	}

	rendered := style.Render(plainText)
	if lipgloss.Width(rendered) <= maxHintWidth {
		return rendered
	}

	runes := []rune(plainText)
	for len(runes) > 0 && lipgloss.Width(style.Render(string(runes)+"…")) > maxHintWidth {
		runes = runes[:len(runes)-1]
	}
	if len(runes) == 0 {
		return ""
	}
	return style.Render(string(runes) + "…")
}

// SetFocused updates the focused state.
func (h *HistoryPaneComponent) SetFocused(f bool) {
	h.focused = f
}

// History returns the history entries.
func (h HistoryPaneComponent) History() []HistoryEntry {
	return h.history
}

// HistIdx returns the current history selection index.
func (h HistoryPaneComponent) HistIdx() int {
	return h.histIdx
}

// ConfirmClear returns the confirmClear state.
func (h HistoryPaneComponent) ConfirmClear() bool {
	return h.confirmClear
}

// SetConfirmClear sets the confirmClear state.
func (h *HistoryPaneComponent) SetConfirmClear(v bool) {
	h.confirmClear = v
}

// SelectedEntry returns the currently selected history entry, or nil if none.
func (h HistoryPaneComponent) SelectedEntry() *HistoryEntry {
	if len(h.history) == 0 {
		return nil
	}
	realIdx := len(h.history) - 1 - h.histIdx
	if realIdx >= 0 && realIdx < len(h.history) {
		return &h.history[realIdx]
	}
	return nil
}

// ForwardMouseEvent forwards a mouse event to the history viewport for scroll handling.
func (h *HistoryPaneComponent) ForwardMouseEvent(ev tea.MouseMsg) tea.Cmd {
	var cmd tea.Cmd
	h.historyVP, cmd = h.historyVP.Update(ev)
	return cmd
}

// YOffset returns the current viewport Y offset.
func (h HistoryPaneComponent) YOffset() int {
	return h.historyVP.YOffset
}

// VPHeight returns the viewport height.
func (h HistoryPaneComponent) VPHeight() int {
	return h.historyVP.Height
}

// SetHistIdx sets the history selection index.
func (h *HistoryPaneComponent) SetHistIdx(idx int) {
	h.histIdx = idx
}
