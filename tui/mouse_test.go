package tui

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// mouseLeftPress returns a tea.MouseMsg for a left button press at (x, y).
func mouseLeftPress(x, y int) tea.MouseMsg {
	return tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: x, Y: y}
}

// testActiveHeight returns the height of an ActivePaneComponent with n active jobs.
func testActiveHeight(n int) int {
	a := ActivePaneComponent{
		active:        make(map[string]*activeJob),
		blocked:       make(map[string]*blockedIssue),
		spinnerFrames: []string{"⠋"},
	}
	now := time.Now()
	for i := 0; i < n; i++ {
		a.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
	}
	return a.Height()
}

// TestHandleMouse_ClickActivePaneTitle verifies a click on the active pane title
// area (Y=2) switches focus to the active pane.
func TestHandleMouse_ClickActivePaneTitle(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory // start on history

	next, cmd := m.Update(mouseLeftPress(10, 2))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Errorf("focusPane = %v, want paneActive after clicking title row Y=2", nm.focusPane)
	}
	if cmd != nil {
		t.Error("expected nil cmd from clicking active pane title")
	}
}

// TestHandleMouse_ClickActivePaneBorderTop verifies a click on Y=1 (active pane
// border top) also switches focus to the active pane.
func TestHandleMouse_ClickActivePaneBorderTop(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory

	next, _ := m.Update(mouseLeftPress(5, 1))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Errorf("focusPane = %v, want paneActive after clicking Y=1", nm.focusPane)
	}
}

// TestHandleMouse_ClickActiveRow_SelectsRow verifies clicking on a row in the
// active pane content area selects that row and switches focus.
func TestHandleMouse_ClickActiveRow_SelectsRow(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	// Add two active jobs so row clicks are meaningful.
	m.active.active[activeJobKey("", 1)] = &activeJob{IssueNumber: 1, StageName: "Research", StartedAt: time.Now()}
	m.active.active[activeJobKey("", 2)] = &activeJob{IssueNumber: 2, StageName: "Plan", StartedAt: time.Now()}

	// With 2 active jobs, activeHeight(2)=5. Row 0 is at Y=3, row 1 is at Y=4.
	// Click Y=3 → select row 0.
	next, cmd := m.Update(mouseLeftPress(5, 3))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Errorf("focusPane = %v, want paneActive", nm.focusPane)
	}
	if nm.active.activeIdx != 0 {
		t.Errorf("activeIdx = %d, want 0 after clicking row Y=3", nm.active.activeIdx)
	}
	if cmd != nil {
		t.Error("expected nil cmd (single click, not double-click)")
	}

	// Click Y=4 → select row 1.
	next2, _ := m.Update(mouseLeftPress(5, 4))
	nm2 := next2.(Model)
	if nm2.active.activeIdx != 1 {
		t.Errorf("activeIdx = %d, want 1 after clicking row Y=4", nm2.active.activeIdx)
	}
}

// TestHandleMouse_DoubleClickActiveRow_OpensWatch verifies that a double-click on
// an active pane row returns a non-nil cmd (tea.ExecProcess for fabrik watch).
func TestHandleMouse_DoubleClickActiveRow_OpensWatch(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.active.active[activeJobKey("", 7)] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}

	// First click: single click, no cmd.
	next, cmd := m.Update(mouseLeftPress(5, 3))
	if cmd != nil {
		t.Error("first click should not open watch")
	}
	nm := next.(Model)

	// Second click at the same position within 300ms: double-click.
	nm.lastClickAt = time.Now() // ensure it's within the threshold
	next2, cmd2 := nm.Update(mouseLeftPress(5, 3))
	_ = next2
	if cmd2 == nil {
		t.Error("expected non-nil cmd (tea.ExecProcess) from double-click on active row")
	}
}

// TestHandleMouse_ClickHistoryTitle_SwitchesFocus verifies clicking the history
// pane title row switches focus to the history pane.
func TestHandleMouse_ClickHistoryTitle_SwitchesFocus(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive

	// With no active jobs, testActiveHeight(0)=4. Active pane: Y=1..4.
	// histTopY = 1 + 4 = 5. histTitle at Y=6.
	histTopY := 1 + testActiveHeight(0)
	histTitleY := histTopY + 1

	next, _ := m.Update(mouseLeftPress(5, histTitleY))
	nm := next.(Model)
	if nm.focusPane != paneHistory {
		t.Errorf("focusPane = %v, want paneHistory after clicking history title Y=%d", nm.focusPane, histTitleY)
	}
}

// TestHandleMouse_ClickHistoryRow_SelectsEntry verifies clicking on a history
// viewport row selects that entry.
func TestHandleMouse_ClickHistoryRow_SelectsEntry(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive
	m.history.history = []HistoryEntry{
		{IssueNumber: 1, StageName: "Research", Success: true},
		{IssueNumber: 2, StageName: "Plan", Success: true},
		{IssueNumber: 3, StageName: "Implement", Success: true},
	}
	m.updateLayout(false)

	// histTopY = 1 + testActiveHeight(0) = 5. Viewport content starts at Y=7.
	histTopY := 1 + testActiveHeight(0)
	histContentStart := histTopY + 2

	// Click the first viewport row → histIdx=0.
	next, cmd := m.Update(mouseLeftPress(10, histContentStart))
	nm := next.(Model)
	if nm.focusPane != paneHistory {
		t.Errorf("focusPane = %v, want paneHistory", nm.focusPane)
	}
	if nm.history.histIdx != 0 {
		t.Errorf("histIdx = %d, want 0 after clicking first history row", nm.history.histIdx)
	}
	if cmd != nil {
		t.Error("expected nil cmd from single history row click")
	}

	// Click the second viewport row → histIdx=1.
	next2, _ := m.Update(mouseLeftPress(10, histContentStart+1))
	nm2 := next2.(Model)
	if nm2.history.histIdx != 1 {
		t.Errorf("histIdx = %d, want 1 after clicking second history row", nm2.history.histIdx)
	}
}

// TestHandleMouse_DoubleClickHistoryRow_OpensWatch verifies that a double-click on
// a history row returns a non-nil cmd.
func TestHandleMouse_DoubleClickHistoryRow_OpensWatch(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.history.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", Success: true},
	}
	m.updateLayout(false)

	histTopY := 1 + testActiveHeight(0)
	histContentStart := histTopY + 2

	// First click at history row.
	next, cmd := m.Update(mouseLeftPress(10, histContentStart))
	if cmd != nil {
		t.Error("first click should not open watch")
	}
	nm := next.(Model)

	// Second click immediately (simulate double-click).
	nm.lastClickAt = time.Now()
	next2, cmd2 := nm.Update(mouseLeftPress(10, histContentStart))
	_ = next2
	if cmd2 == nil {
		t.Error("expected non-nil cmd from double-click on history row")
	}
}

// TestHandleMouse_NonLeftClick_NoStateChange verifies that non-left-click mouse
// events (e.g. right click, motion) do not change selection state.
func TestHandleMouse_NonLeftClick_NoStateChange(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24
	m.focusPane = paneActive

	// Right button press: should not change focus or selection.
	rightClick := tea.MouseMsg{Button: tea.MouseButtonRight, Action: tea.MouseActionPress, X: 5, Y: 3}
	next, _ := m.Update(rightClick)
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Error("right-click should not change focus pane")
	}

	// Release event: no state change.
	release := tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, X: 5, Y: 2}
	next2, _ := m.Update(release)
	nm2 := next2.(Model)
	if nm2.focusPane != paneActive {
		t.Error("mouse release should not change focus pane")
	}
}

// TestHandleMouse_ClickOutOfBounds_NoStateChange verifies clicks outside the
// known pane regions do not crash or change state unexpectedly.
func TestHandleMouse_ClickOutOfBounds_NoStateChange(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.height = 24

	// Click at Y=0 (header) should not crash or change focus.
	next, _ := m.Update(mouseLeftPress(5, 0))
	nm := next.(Model)
	if nm.focusPane != paneActive {
		t.Error("click on header should not change focus pane from initial paneActive")
	}
}
