package watch

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
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

// TestWrapContent verifies the wrapContent helper wraps long lines at the given width.
func TestWrapContent(t *testing.T) {
	// Zero / negative width: return unchanged.
	in := "hello world this is a very long line that should not be wrapped"
	if got := wrapContent(in, 0); got != in {
		t.Errorf("wrapContent(width=0) should return input unchanged; got %q", got)
	}

	// Width >= len(content): no wrap needed.
	short := "hello world"
	if got := wrapContent(short, 80); got != short {
		t.Errorf("wrapContent(short, 80) = %q; want %q", got, short)
	}

	// Width < len(content): result must contain a newline.
	wrapped := wrapContent("hello world foo bar", 10)
	if !strings.Contains(wrapped, "\n") {
		t.Errorf("wrapContent should insert newline for long line; got %q", wrapped)
	}

	// Each resulting line must be <= width runes.
	for _, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
		if len([]rune(line)) > 10 {
			t.Errorf("wrapped line %q exceeds width 10", line)
		}
	}
}

// TestUpdate_WindowSizeMsg_RewrapsContent verifies that a WindowSizeMsg re-wraps
// live content at the new viewport width.
func TestUpdate_WindowSizeMsg_RewrapsContent(t *testing.T) {
	m := newTestModel()
	m.vp = viewport.New(80, 20)
	// Add a long line to the live buffer.
	longLine := strings.Repeat("word ", 30) + "\n"
	m.lines = []string{longLine}
	m.vp.SetContent(longLine)

	// Shrink the terminal width to 40.
	model, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	wm := model.(WatchModel)
	content := wm.vp.View()
	for _, line := range strings.Split(content, "\n") {
		visible := len([]rune(line))
		if visible > 42 { // viewport adds a couple chars of padding; allow slack
			t.Errorf("after resize to 40, line %q has %d runes (expected <= 42)", line, visible)
		}
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
