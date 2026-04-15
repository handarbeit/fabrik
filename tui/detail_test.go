package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel verifies enter in history pane toggles the detail panel.
func TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
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
	m := New(30, ProjectInfo{}, "", nil)
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

// TestUpdate_EnterKey_ActivePane_TogglesDetailPanel verifies enter in active pane toggles the detail panel.
func TestUpdate_EnterKey_ActivePane_TogglesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
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
