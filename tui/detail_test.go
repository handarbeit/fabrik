package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel verifies enter in history pane toggles the detail panel.
func TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 99999, StageName: "Research"},
	}
	// enter toggles detail panel — no subprocess launched.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("enter key should return nil cmd (no subprocess)")
	}
	nm := next.(Model)
	if !nm.detailPanel {
		t.Error("expected detailPanel=true after enter in history pane")
	}
}

// TestUpdate_EscapeKey_ClosesDetailPanel verifies escape closes the detail panel.
func TestUpdate_EscapeKey_ClosesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.detailPanel = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("escape key should return nil cmd")
	}
	nm := next.(Model)
	if nm.detailPanel {
		t.Error("expected detailPanel=false after escape key")
	}
}

// TestDetailPanel_TurnLimitStatus verifies the detail panel's "Status" line renders
// "incomplete (turn limit)" for a turn-capped entry, distinct from a plain "incomplete"
// retry — part of the issue #1178 rendering fix (also applied in tui/history.go).
func TestDetailPanel_TurnLimitStatus(t *testing.T) {
	var d DetailPanelComponent
	d.SetVisible(true)
	d.SetWidth(80)

	d.SetItem(&DetailItem{IssueNumber: 1, StageName: "Implement", Success: true, TurnLimited: true, Completed: false})
	view := d.View(80)
	if !strings.Contains(view, "incomplete (turn limit)") {
		t.Errorf("expected %q in detail panel view for turn-capped entry, got: %q", "incomplete (turn limit)", view)
	}

	d.SetItem(&DetailItem{IssueNumber: 2, StageName: "Implement", Success: true, TurnLimited: false, Completed: false})
	view = d.View(80)
	if !strings.Contains(view, "Status:   incomplete") || strings.Contains(view, "turn limit") {
		t.Errorf("expected plain %q (no turn limit) in detail panel view for retry entry, got: %q", "incomplete", view)
	}
}

// TestUpdate_EnterKey_ActivePane_TogglesDetailPanel verifies enter in active pane toggles the detail panel.
func TestUpdate_EnterKey_ActivePane_TogglesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.width = 80
	m.height = 24
	m.focusPane = paneActive
	m.active.active[activeJobKey("", 99999)] = &activeJob{StageName: "Research", StartedAt: time.Now()}

	// First enter: panel opens.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(Model)
	if cmd != nil {
		t.Error("enter key should return nil cmd (no subprocess)")
	}
	if !nm.detailPanel {
		t.Error("expected detailPanel=true after first enter")
	}

	// Second enter: panel closes.
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm2 := next2.(Model)
	if nm2.detailPanel {
		t.Error("expected detailPanel=false after second enter")
	}
}
