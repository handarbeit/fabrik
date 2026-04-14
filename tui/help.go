package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// helpContent holds all keybindings and labels documentation shown in the help panel.
// All lines must be ≤ 72 visible characters to avoid wrapping at standard terminal widths.
//
// NOTE: keep in sync with docs/USER_GUIDE.md §6 (Labels Reference) and §7 (TUI
// Dashboard Keyboard Shortcuts). Update both locations when adding or changing entries.
var helpContent = strings.TrimSpace(`
KEYBOARD SHORTCUTS

  Navigation
    tab        Switch focus: In Progress <-> History
    ↑/↓  k/j  Navigate items in the focused pane
    enter      Toggle inline detail panel
    esc        Close open panels; with none open, triggers quit confirmation
    n / N      Cancel quit or clear-all confirmation

  Active Jobs (In Progress pane)
    l          Open fabrik watch for selected issue (live output)
    r          (disabled) Stage in progress — use l to watch

  History pane
    l          View log for selected history entry
    r          Resume Claude session for selected history entry
    c          Delete selected history entry
    C          Clear all history (with confirmation)

  Global
    ?          Toggle this help panel
    q          Quit
    ctrl+c     Force quit

─────────────────────────────────────────────────────────

LABELS REFERENCE

  Engine-managed state
    fabrik:locked:<user>   Issue being processed by this user's instance
    fabrik:editing         Issue body being updated (comment processing)
    fabrik:paused          Processing paused (max retries exceeded or manual)
    fabrik:awaiting-input  Stage paused waiting for user input
    fabrik:awaiting-review Waiting for PR reviewers to submit
    fabrik:blocked         Waiting for blocking issues to close

  Stage state
    stage:<name>:in_progress  Stage actively running
    stage:<name>:complete     Stage completed successfully
    stage:<name>:failed       Stage hit max retries

  Automation modes
    fabrik:yolo    Auto-advance; auto-merge PR on Validate complete
    fabrik:cruise  Auto-advance through all stages, no auto-merge

  Per-issue overrides (user-set)
    model:<name>        Override Claude model (e.g. opus, sonnet)
    effort:<level>      Override thinking effort (low/medium/high/max)
    fabrik:paused       Manually pause (same label; add to pause, remove to resume)
    fabrik:unrestricted Skip permissions check (use with caution)
    base:<branch>       Override base branch (e.g. base:develop)

  Other
    fabrik:sub-issue  Plan-created sub-issues; never decomposed further
`) + "\n"

// HelpPanelComponent renders a scrollable help overlay with keybindings
// and label reference. It follows the DetailPanelComponent pattern:
// value-type component with pointer-receiver setters and Height() returning
// 0 when not visible.
type HelpPanelComponent struct {
	vp        viewport.Model
	visible   bool
	lastWidth int
}

// Update is a no-op. Scrolling is handled by forwarding key events from
// the root model directly to vp.
func (h HelpPanelComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	return h, nil
}

// View renders the help panel. Returns an empty string when not visible.
func (h HelpPanelComponent) View(width int) string {
	if !h.visible {
		return ""
	}
	titleLine := dimStyle.Render("Help") + "  " + dimStyle.Render("[?/esc] close")
	content := titleLine + "\n" + h.vp.View()
	return borderStyle.Width(width - 4).Render(content)
}

// Height returns the total rendered height by measuring the actual rendered view.
// Returns 0 when not visible. Uses the same approach as DetailPanelComponent to
// correctly account for any line wrapping inside the border.
func (h HelpPanelComponent) Height() int {
	if !h.visible {
		return 0
	}
	w := h.lastWidth
	if w == 0 {
		w = 80
	}
	view := h.View(w)
	if view == "" {
		return 0
	}
	return strings.Count(view, "\n") + 1
}

// SetVisible controls whether the help panel is shown.
func (h *HelpPanelComponent) SetVisible(v bool) {
	h.visible = v
}

// SetLayout sizes the internal viewport to fit within targetHeight terminal rows.
// The viewport height is targetHeight - 3 (subtracting 1 title and 2 border lines).
// On resize the existing scroll offset is preserved so an open panel stays in place.
func (h *HelpPanelComponent) SetLayout(width, targetHeight int) {
	vpH := max(targetHeight-3, 1)
	innerWidth := max(width-6, 20) // -6 for border+padding, matching other bordered components
	if h.vp.Width == 0 && h.vp.Height == 0 {
		h.vp = viewport.New(innerWidth, vpH)
		h.vp.SetContent(helpContent)
	} else {
		h.vp.Width = innerWidth
		h.vp.Height = vpH
	}
	h.lastWidth = width
}

// ScrollToTop scrolls the viewport to the top.
func (h *HelpPanelComponent) ScrollToTop() {
	h.vp.GotoTop()
}
