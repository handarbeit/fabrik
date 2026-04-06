package watch

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// newTestModel creates a minimal WatchModel for key-handler tests.
// It avoids filesystem calls by using an empty logDir and no stagesDir.
func newTestModel() WatchModel {
	m := WatchModel{
		issueNumber: 1,
		done:        make(chan struct{}),
	}
	return m
}

// isDone returns true if the done channel is closed.
func isDone(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestUpdate_EscapeKey_Quits verifies that the Escape key quits the watch TUI.
func TestUpdate_EscapeKey_Quits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from Escape key")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
	if !isDone(m.done) {
		t.Error("expected done channel to be closed after Escape key")
	}
}

// TestUpdate_QKey_Quits verifies that q quits the watch TUI (unchanged behavior).
func TestUpdate_QKey_Quits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from q key")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

// TestUpdate_CtrlC_Quits verifies that ctrl+c quits the watch TUI (unchanged behavior).
func TestUpdate_CtrlC_Quits(t *testing.T) {
	m := newTestModel()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected non-nil cmd from ctrl+c key")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}
