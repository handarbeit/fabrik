package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestHeaderHeight(t *testing.T) {
	h := HeaderComponent{}.Height()
	if h != 1 {
		t.Errorf("HeaderComponent.Height() = %d, want 1", h)
	}
}

// TestViewHeader_TimerAlwaysVisible verifies that when the status message is
// long enough to trigger truncation, the timer string still appears in the output.
func TestViewHeader_TimerAlwaysVisible(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 60
	m.header.nextPollAt = time.Now().Add(90 * time.Second)

	// Status long enough to overflow without truncation.
	m.header.statusLine = "this is a very long status message that should be truncated by the header renderer"

	header := m.header.View(m.width)

	// The timer prefix must be visible.
	if !strings.Contains(header, "poll in") {
		t.Errorf("viewHeader() does not contain timer; got: %q", header)
	}
}

// TestViewHeader_WidthNeverExceedsTerminal verifies that lipgloss.Width of
// viewHeader() never exceeds m.width for various status line lengths.
func TestViewHeader_WidthNeverExceedsTerminal(t *testing.T) {
	cases := []struct {
		name       string
		width      int
		statusLine string
	}{
		{"empty status", 80, ""},
		{"short status", 80, "syncing"},
		{"boundary status", 80, strings.Repeat("x", 60)},
		{"very long status", 80, strings.Repeat("x", 200)},
		{"narrow terminal empty", 30, ""},
		{"narrow terminal long", 30, strings.Repeat("x", 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(30, ProjectInfo{}, "")
			m.width = tc.width
			m.header.nextPollAt = time.Now().Add(90 * time.Second)
			m.header.statusLine = tc.statusLine

			header := m.header.View(m.width)
			w := lipgloss.Width(header)
			if w > tc.width {
				t.Errorf("viewHeader() width %d exceeds terminal width %d; status=%q",
					w, tc.width, tc.statusLine)
			}
		})
	}
}

// TestViewHeader_WithStatusLine verifies statusLine content appears in the header.
func TestViewHeader_WithStatusLine(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 80
	m.header.statusLine = "some status"
	header := m.header.View(m.width)
	if !strings.Contains(header, "some status") {
		t.Errorf("header missing statusLine, got: %q", header)
	}
}

// TestViewHeader_StatusLineTruncation verifies narrow headers truncate statusLine without panic.
func TestViewHeader_StatusLineTruncation(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.width = 25
	m.header.statusLine = "a very very very very very very long status message"
	header := m.header.View(m.width)
	w := lipgloss.Width(header)
	if w > m.width+5 {
		t.Errorf("header too wide: %d > %d", w, m.width)
	}
}
