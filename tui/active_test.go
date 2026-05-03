package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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
		a := ActivePaneComponent{
			active:        make(map[string]*activeJob),
			blocked:       make(map[string]*blockedIssue),
			spinnerFrames: []string{"⠋"},
		}
		now := time.Now()
		for i := 0; i < tt.n; i++ {
			a.active[fmt.Sprintf("issue-%d", i+1)] = &activeJob{StageName: "Research", StartedAt: now}
		}
		got := a.Height()
		if got != tt.want {
			t.Errorf("ActivePaneComponent.Height() with %d jobs = %d, want %d", tt.n, got, tt.want)
		}
	}
}

func TestUpdate_JobStartedAndCompleted(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
	start := time.Now()

	// JobStartedEvent adds to active
	next, _ := m.Update(JobStartedEvent{IssueNumber: 42, StageName: "Implement", StartedAt: start})
	nm := next.(Model)
	key42 := activeJobKey("", 42)
	if _, ok := nm.active.active[key42]; !ok {
		t.Fatal("expected issue 42 in active after JobStartedEvent")
	}
	if nm.active.active[key42].StageName != "Implement" {
		t.Errorf("StageName = %q", nm.active.active[key42].StageName)
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

	if _, ok := nm2.active.active[key42]; ok {
		t.Error("expected issue 42 removed from active after JobCompletedEvent")
	}
	if len(nm2.history.History()) != 1 {
		t.Fatalf("history len = %d, want 1", len(nm2.history.History()))
	}
	h := nm2.history.History()[0]
	if h.IssueNumber != 42 || h.StageName != "Implement" || !h.Success {
		t.Errorf("history entry = %+v", h)
	}
	if h.Duration != 2*time.Minute {
		t.Errorf("duration = %v, want 2m", h.Duration)
	}
}

func TestUpdate_LogEvent_UpdatesActiveJob(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	key7 := activeJobKey("", 7)
	m.active.active[key7] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.active.activeNumToKey[7] = key7

	next, _ := m.Update(LogEvent{IssueNumber: 7, Tag: "claude", Message: "running prompt\n"})
	nm := next.(Model)

	job, ok := nm.active.active[key7]
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
	m := New(30, ProjectInfo{}, "", nil)
	next, _ := m.Update(LogEvent{IssueNumber: 999, Tag: "warn", Message: "something\n"})
	nm := next.(Model)
	if _, ok := nm.active.active[activeJobKey("", 999)]; ok {
		t.Error("unknown issue should not be added to active via LogEvent")
	}
}

// TestUpdate_JK_ActivePane verifies j/k navigation in the active pane.
func TestUpdate_JK_ActivePane(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.active.active[activeJobKey("", 1)] = &activeJob{StageName: "Research", StartedAt: time.Now()}
	m.active.active[activeJobKey("", 2)] = &activeJob{StageName: "Plan", StartedAt: time.Now()}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	nm := next.(Model)
	if nm.active.activeIdx != 1 {
		t.Errorf("activeIdx = %d after j, want 1", nm.active.activeIdx)
	}
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	nm2 := next2.(Model)
	if nm2.active.activeIdx != 0 {
		t.Errorf("activeIdx = %d after k, want 0", nm2.active.activeIdx)
	}
}

// TestUpdate_RKey_ActivePane_SetsStatusMsg verifies r on an active pane item sets statusMsg.
func TestUpdate_RKey_ActivePane_SetsStatusMsg(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.focusPane = paneActive
	key7 := activeJobKey("", 7)
	m.active.active[key7] = &activeJob{IssueNumber: 7, StageName: "Research", StartedAt: time.Now()}
	m.active.activeNumToKey[7] = key7

	// r on an active pane item sets statusMsg and returns no cmd.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key in active pane")
	}
	nm := next.(Model)
	if nm.header.statusMsg == "" {
		t.Error("expected statusMsg to be set after r key in active pane")
	}
}

func TestUpdate_RKey_ActivePane_NoJobs_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.focusPane = paneActive
	// No active jobs: r is a no-op

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Error("expected nil cmd from r key with empty active pane")
	}
}

// TestViewActive_IsComment verifies the 💬 emoji appears for comment jobs.
func TestViewActive_IsComment(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 80
	m.height = 24
	m.active.active[activeJobKey("", 5)] = &activeJob{StageName: "Implement", IsComment: true, StartedAt: time.Now()}
	view := m.active.View(m.width)
	if !strings.Contains(view, "💬") {
		t.Errorf("expected 💬 in viewActive for IsComment job, got: %q", view)
	}
}

func TestUpdate_IssueBlockedEvent(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)

	// IssueBlockedEvent adds to blocked map
	next, _ := m.Update(IssueBlockedEvent{
		IssueNumber: 214,
		Title:       "fix auto-upgrade",
		StageName:   "Research",
		WaitingFor:  []string{"#213"},
	})
	nm := next.(Model)
	key214 := activeJobKey("", 214)
	b, ok := nm.active.blocked[key214]
	if !ok {
		t.Fatal("expected issue 214 in blocked map after IssueBlockedEvent")
	}
	if b.StageName != "Research" {
		t.Errorf("StageName = %q, want Research", b.StageName)
	}
	if len(b.WaitingFor) != 1 || b.WaitingFor[0] != "#213" {
		t.Errorf("WaitingFor = %v, want [#213]", b.WaitingFor)
	}

	// JobStartedEvent for the same issue removes it from blocked
	next2, _ := nm.Update(JobStartedEvent{
		IssueNumber: 214,
		StageName:   "Research",
		StartedAt:   time.Now(),
	})
	nm2 := next2.(Model)
	if _, ok := nm2.active.blocked[key214]; ok {
		t.Error("expected issue 214 removed from blocked after JobStartedEvent")
	}
}

func TestViewActive_BlockedIssue(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 120
	m.active.now = time.Now()

	// Add a blocked issue
	key := activeJobKey("", 215)
	m.active.blocked[key] = &blockedIssue{
		IssueNumber: 215,
		Title:       "cut release",
		StageName:   "Research",
		WaitingFor:  []string{"#214"},
	}

	view := m.active.View(m.width)
	if !strings.Contains(view, "🔒") {
		t.Errorf("expected 🔒 in viewActive for blocked issue, got: %q", view)
	}
	if !strings.Contains(view, "#215") {
		t.Errorf("expected #215 in viewActive, got: %q", view)
	}
	if !strings.Contains(view, "waiting for") {
		t.Errorf("expected 'waiting for' in viewActive, got: %q", view)
	}
	if !strings.Contains(view, "#214") {
		t.Errorf("expected #214 in waiting-for list, got: %q", view)
	}
}

func TestViewActive_BlockedCountIncludedInHeader(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil)
	m.width = 120
	m.active.now = time.Now()

	// 1 active + 1 blocked = 2 in header
	m.active.active[activeJobKey("", 10)] = &activeJob{IssueNumber: 10, StageName: "Implement", StartedAt: time.Now()}
	m.active.blocked[activeJobKey("", 20)] = &blockedIssue{IssueNumber: 20, StageName: "Research", WaitingFor: []string{"#10"}}

	view := m.active.View(m.width)
	if !strings.Contains(view, "In Progress (2)") {
		t.Errorf("expected 'In Progress (2)' in header, got: %q", view)
	}
}

func TestIssueBlockedEvent_tuiEvent(t *testing.T) {
	IssueBlockedEvent{}.tuiEvent() // satisfies interface
}

// TestTurnBadge verifies full/compact/omit rendering.
func TestTurnBadge(t *testing.T) {
	tests := []struct {
		turnsUsed int
		maxTurns  int
		available int
		want      string
	}{
		{5, 50, 100, "[5/50 turns]"},
		{5, 50, 12, "[5/50 turns]"},
		{5, 50, 11, "[5/50]"},
		{5, 50, 7, "[5/50]"},
		{5, 50, 6, "[5/50]"},
		{5, 50, 5, ""},
		{0, 50, 100, ""},         // no turns yet
		{5, 50, 0, ""},           // no space
		{5, 50, -1, ""},          // negative space
		{3, 0, 100, "[3 turns]"}, // unlimited
		{3, 0, 9, "[3 turns]"},
		{3, 0, 8, "[3]"},
		{3, 0, 2, ""},
	}
	for _, tt := range tests {
		got := turnBadge(tt.turnsUsed, tt.maxTurns, tt.available)
		if got != tt.want {
			t.Errorf("turnBadge(%d,%d,%d) = %q, want %q",
				tt.turnsUsed, tt.maxTurns, tt.available, got, tt.want)
		}
	}
}

// TestUpdate_TurnProgressEvent_UpdatesJob verifies that TurnProgressEvent updates the active job.
func TestUpdate_TurnProgressEvent_UpdatesJob(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	key42 := activeJobKey("", 42)
	m.active.active[key42] = &activeJob{IssueNumber: 42, StageName: "Research", StartedAt: time.Now()}
	m.active.activeNumToKey[42] = key42

	next, _ := m.Update(TurnProgressEvent{IssueNumber: 42, TurnsUsed: 7, MaxTurns: 50})
	nm := next.(Model)

	job, ok := nm.active.active[key42]
	if !ok {
		t.Fatal("issue 42 missing from active after TurnProgressEvent")
	}
	if job.TurnsUsed != 7 {
		t.Errorf("TurnsUsed = %d, want 7", job.TurnsUsed)
	}
	if job.MaxTurns != 50 {
		t.Errorf("MaxTurns = %d, want 50", job.MaxTurns)
	}
}

// TestUpdate_TurnProgressEvent_UnknownIssue verifies that an unknown issue number does not panic.
func TestUpdate_TurnProgressEvent_UnknownIssue(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	// Should not panic for an issue not in the active map.
	next, _ := m.Update(TurnProgressEvent{IssueNumber: 999, TurnsUsed: 5, MaxTurns: 30})
	nm := next.(Model)
	if _, ok := nm.active.active[activeJobKey("", 999)]; ok {
		t.Error("unknown issue should not be added to active via TurnProgressEvent")
	}
}

// TestUpdate_TurnProgressEvent_ResetOnJobStarted verifies that starting a new job resets turn state.
func TestUpdate_TurnProgressEvent_ResetOnJobStarted(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil)
	key5 := activeJobKey("", 5)
	m.active.active[key5] = &activeJob{IssueNumber: 5, StageName: "Research", StartedAt: time.Now(), TurnsUsed: 10, MaxTurns: 30}
	m.active.activeNumToKey[5] = key5

	// Simulate a new stage start that replaces the existing job.
	next, _ := m.Update(JobStartedEvent{IssueNumber: 5, StageName: "Plan", StartedAt: time.Now()})
	nm := next.(Model)

	job := nm.active.active[activeJobKey("", 5)]
	if job.TurnsUsed != 0 {
		t.Errorf("TurnsUsed = %d after new JobStartedEvent, want 0", job.TurnsUsed)
	}
	if job.MaxTurns != 0 {
		t.Errorf("MaxTurns = %d after new JobStartedEvent, want 0", job.MaxTurns)
	}
}
