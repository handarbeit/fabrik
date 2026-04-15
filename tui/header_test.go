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
	m := New(30, ProjectInfo{}, "", nil)
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
			m := New(30, ProjectInfo{}, "", nil)
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
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.header.statusLine = "some status"
	header := m.header.View(m.width)
	if !strings.Contains(header, "some status") {
		t.Errorf("header missing statusLine, got: %q", header)
	}
}

// TestViewHeader_StatusLineTruncation verifies narrow headers truncate statusLine without panic.
func TestViewHeader_StatusLineTruncation(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 25
	m.header.statusLine = "a very very very very very very long status message"
	header := m.header.View(m.width)
	w := lipgloss.Width(header)
	if w > m.width+5 {
		t.Errorf("header too wide: %d > %d", w, m.width)
	}
}

// TestViewHeader_EffectiveInterval verifies that PollCompletedEvent with
// EffectiveInterval updates the header timer to show the effective interval.
func TestViewHeader_EffectiveInterval(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80

	// Send a PollCompletedEvent with 2-minute effective interval.
	next, _ := m.Update(PollCompletedEvent{
		ItemCount:         5,
		EffectiveInterval: 2 * time.Minute,
	})
	nm := next.(Model)

	// The header should show approximately "poll in 02:00" (not "00:30").
	// Allow 01:59 due to timing: time.Now() in Update vs View can differ by ~1s.
	header := nm.header.View(nm.width)
	if !strings.Contains(header, "02:") && !strings.Contains(header, "01:59") {
		t.Errorf("header should show ~02:xx for 2min effective interval, got: %q", header)
	}
	if strings.Contains(header, "00:3") {
		t.Errorf("header should NOT show the base 30s interval, got: %q", header)
	}
}

// TestViewHeader_PollStartedUsesEffectiveInterval verifies that after a
// PollCompletedEvent sets the effective interval, a subsequent PollStartedEvent
// uses that effective interval (not the configured one).
func TestViewHeader_PollStartedUsesEffectiveInterval(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80

	// Set effective interval via PollCompletedEvent.
	next, _ := m.Update(PollCompletedEvent{
		EffectiveInterval: 2 * time.Minute,
	})
	m = next.(Model)

	// PollStartedEvent should use stored effective interval.
	next, _ = m.Update(PollStartedEvent{Owner: "o", Repo: "r", Project: 1})
	nm := next.(Model)

	header := nm.header.View(nm.width)
	if !strings.Contains(header, "02:") && !strings.Contains(header, "01:59") {
		t.Errorf("PollStartedEvent should use effective interval of 2min, got: %q", header)
	}
}
