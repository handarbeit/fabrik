package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFmtDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0:00"},
		{-5 * time.Second, "0:00"}, // negative clamped to 0
		{30 * time.Second, "0:30"},
		{90 * time.Second, "1:30"},
		{61 * time.Second, "1:01"},
		{10*time.Minute + 5*time.Second, "10:05"},
		{500 * time.Millisecond, "0:01"}, // rounds to nearest second
		{499 * time.Millisecond, "0:00"}, // rounds down
	}
	for _, tt := range tests {
		got := fmtDuration(tt.d)
		if got != tt.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestHeaderHeight(t *testing.T) {
	h := headerHeight()
	if h != 4 {
		t.Errorf("headerHeight() = %d, want 4", h)
	}
}

func TestActiveHeight(t *testing.T) {
	tests := []struct {
		n    int
		want int
	}{
		{0, 4}, // min 2 lines + 2 border
		{1, 4}, // 1 job + title + 2 border
		{2, 5}, // 2 jobs + title + 2 border
		{5, 8},
	}
	for _, tt := range tests {
		got := activeHeight(tt.n)
		if got != tt.want {
			t.Errorf("activeHeight(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

func TestNew(t *testing.T) {
	m := New(30)
	if m.pollInterval != 30*time.Second {
		t.Errorf("pollInterval = %v, want 30s", m.pollInterval)
	}
	if m.active == nil {
		t.Error("active map should be initialized")
	}
	if len(m.spinnerFrames) == 0 {
		t.Error("spinnerFrames should be non-empty")
	}
	if m.now.IsZero() {
		t.Error("now should be set")
	}
}

func TestUpdate_TickEvent(t *testing.T) {
	m := New(30)
	m.width = 80
	m.height = 24
	initial := m.spinnerIdx
	at := time.Now()

	next, cmd := m.Update(TickEvent{At: at})
	nm := next.(Model)

	if nm.now != at {
		t.Error("now not updated from TickEvent")
	}
	if nm.spinnerIdx != (initial+1)%len(m.spinnerFrames) {
		t.Errorf("spinnerIdx = %d, want %d", nm.spinnerIdx, (initial+1)%len(m.spinnerFrames))
	}
	if cmd == nil {
		t.Error("expected non-nil cmd (next tick) from TickEvent")
	}
}

func TestUpdate_JobStartedAndCompleted(t *testing.T) {
	m := New(30)
	m.history = nil // clear any persisted history
	start := time.Now()

	// JobStartedEvent adds to active
	next, _ := m.Update(JobStartedEvent{IssueNumber: 42, StageName: "Implement", StartedAt: start})
	nm := next.(Model)
	if _, ok := nm.active[42]; !ok {
		t.Fatal("expected issue 42 in active after JobStartedEvent")
	}
	if nm.active[42].StageName != "Implement" {
		t.Errorf("StageName = %q", nm.active[42].StageName)
	}

	// JobCompletedEvent removes from active and adds to history
	end := start.Add(2 * time.Minute)
	next2, _ := nm.Update(JobCompletedEvent{
		IssueNumber: 42,
		StageName:   "Implement",
		Success:     true,
		Duration:    2 * time.Minute,
		CompletedAt: end,
	})
	nm2 := next2.(Model)

	if _, ok := nm2.active[42]; ok {
		t.Error("expected issue 42 removed from active after JobCompletedEvent")
	}
	if len(nm2.history) != 1 {
		t.Fatalf("history len = %d, want 1", len(nm2.history))
	}
	h := nm2.history[0]
	if h.IssueNumber != 42 || h.StageName != "Implement" || !h.Success {
		t.Errorf("history entry = %+v", h)
	}
	if h.Duration != 2*time.Minute {
		t.Errorf("duration = %v, want 2m", h.Duration)
	}
}

func TestUpdate_LogEvent_UpdatesActiveJob(t *testing.T) {
	m := New(30)
	m.active[7] = &activeJob{StageName: "Research", StartedAt: time.Now()}

	next, _ := m.Update(LogEvent{IssueNumber: 7, Tag: "claude", Message: "running prompt\n"})
	nm := next.(Model)

	job, ok := nm.active[7]
	if !ok {
		t.Fatal("issue 7 missing from active")
	}
	if job.LastTag != "claude" {
		t.Errorf("LastTag = %q, want claude", job.LastTag)
	}
	if job.LastLine != "running prompt" {
		t.Errorf("LastLine = %q, want 'running prompt' (trailing newline stripped)", job.LastLine)
	}
}

func TestUpdate_LogEvent_UnknownIssue(t *testing.T) {
	// LogEvent for an issue not in active map should not panic
	m := New(30)
	next, _ := m.Update(LogEvent{IssueNumber: 999, Tag: "warn", Message: "something\n"})
	nm := next.(Model)
	if _, ok := nm.active[999]; ok {
		t.Error("unknown issue should not be added to active via LogEvent")
	}
}

func TestUpdate_PollStartedAndCompleted(t *testing.T) {
	m := New(30)
	before := time.Now()

	next, _ := m.Update(PollStartedEvent{Owner: "o", Repo: "r", Project: 1})
	nm := next.(Model)
	if !nm.nextPollAt.After(before) {
		t.Error("nextPollAt should be in the future after PollStartedEvent")
	}

	next2, _ := nm.Update(PollCompletedEvent{ItemCount: 5, Dispatched: 2})
	nm2 := next2.(Model)
	if nm2.pollCount != 1 {
		t.Errorf("pollCount = %d, want 1", nm2.pollCount)
	}
}

func TestUpdate_QuitKey(t *testing.T) {
	m := New(30)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Error("expected quit cmd from 'q' key")
	}
	// Execute the command and check it's tea.Quit
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestView_BeforeWindowSize(t *testing.T) {
	m := New(30)
	// Before width is set, View should return a loading placeholder without panicking
	v := m.View()
	if !strings.Contains(v, "Loading") {
		t.Errorf("expected 'Loading...' placeholder before window size, got %q", v)
	}
}

func TestView_AfterWindowSize(t *testing.T) {
	m := New(30)
	m.width = 80
	m.height = 24
	m.nextPollAt = time.Now().Add(30 * time.Second)

	v := m.View()
	if strings.Contains(v, "Loading") {
		t.Error("should not show loading after window size is set")
	}
	if !strings.Contains(v, "fabrik") {
		t.Error("header should contain 'fabrik'")
	}
	if !strings.Contains(v, "In Progress") {
		t.Error("should show In Progress pane")
	}
	if !strings.Contains(v, "History") {
		t.Error("should show History pane")
	}
}
