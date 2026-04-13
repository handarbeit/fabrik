package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "00:00"},
		{-5 * time.Second, "00:00"}, // negative clamped to 0
		{30 * time.Second, "00:30"},
		{90 * time.Second, "01:30"},
		{61 * time.Second, "01:01"},
		{10*time.Minute + 5*time.Second, "10:05"},
		{500 * time.Millisecond, "00:01"}, // rounds to nearest second
		{499 * time.Millisecond, "00:00"}, // rounds down
	}
	for _, tt := range tests {
		got := fmtDuration(tt.d)
		if got != tt.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestFmtRateLimitCountdown verifies the countdown formatting helper.
func TestFmtRateLimitCountdown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		delta time.Duration
		want  string
	}{
		{"soon (past)", -5 * time.Second, "soon"},
		{"soon (zero)", 0, "soon"},
		{"seconds", 45 * time.Second, "45s"},
		{"minutes", 3*time.Minute + 30*time.Second, "3m"},
		{"hours", 2*time.Hour + 30*time.Minute, "2h"},
		{"exactly 1 minute", 60 * time.Second, "1m"},
		{"exactly 1 hour", 3600 * time.Second, "1h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fmtRateLimitCountdown(now.Add(tc.delta), now)
			if got != tc.want {
				t.Errorf("fmtRateLimitCountdown delta=%v: got %q, want %q", tc.delta, got, tc.want)
			}
		})
	}
}

// TestTuiReadSessionID_NotFound verifies empty string for a missing session file.
func TestTuiReadSessionID_NotFound(t *testing.T) {
	id := tuiReadSessionID("", 99999, "SomeStageThatDoesNotExist")
	if id != "" {
		t.Errorf("expected empty string for missing session, got %q", id)
	}
}

// TestUpdate_LKey_Returns_WatchCmd verifies the l key returns a non-nil tea.ExecProcess cmd.
func TestUpdate_LKey_Returns_WatchCmd(t *testing.T) {
	m := New(30, ProjectInfo{}, "")
	m.focusPane = paneActive
	m.active.active[activeJobKey("", 7)] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.active.activeNumToKey[7] = activeJobKey("", 7)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if cmd == nil {
		t.Error("expected non-nil cmd (tea.ExecProcess) from l key with active job")
	}
}
