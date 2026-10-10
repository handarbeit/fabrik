package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func newTrainActive() ActivePaneComponent {
	return ActivePaneComponent{
		active:         make(map[string]*activeJob),
		activeNumToKey: make(map[int]string),
		blocked:        make(map[string]*blockedIssue),
		spinnerFrames:  []string{"⠋"},
	}
}

func startTrainRow(a ActivePaneComponent, now time.Time) ActivePaneComponent {
	c, _ := a.Update(JobStartedEvent{Repo: "o/r", Title: "5 of 5: #1 #2 #3 #4 #5", StageName: "Merge Train", StartedAt: now})
	a = c.(ActivePaneComponent)
	c, _ = a.Update(TickEvent{At: now})
	return c.(ActivePaneComponent)
}

func TestTrainRowEvent_UpdatesTitleAndPhase(t *testing.T) {
	now := time.Now()
	a := startTrainRow(newTrainActive(), now)

	c, _ := a.Update(TrainRowEvent{Repo: "o/r", Title: "3 of 5: #2 #3 #5 (ejected #1 #4)", Phase: "trial CI #4012", PhaseStartedAt: now})
	a = c.(ActivePaneComponent)
	job := a.active[activeJobKey("o/r", 0)]
	if job.Title != "3 of 5: #2 #3 #5 (ejected #1 #4)" || job.Phase != "trial CI #4012" {
		t.Fatalf("row not updated: %+v", job)
	}
	if !job.StartedAt.Equal(now) {
		t.Errorf("StartedAt must be untouched by TrainRowEvent")
	}
}

func TestTrainRowEvent_IgnoredWhenRowGone(t *testing.T) {
	a := newTrainActive()
	c, _ := a.Update(TrainRowEvent{Repo: "o/r", Title: "x", Phase: "landing"})
	a = c.(ActivePaneComponent)
	if len(a.active) != 0 {
		t.Fatalf("TrainRowEvent must not create a row, got %d", len(a.active))
	}
}

func TestTrainRow_PhaseElapsedRestartsAndGrows(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a := startTrainRow(newTrainActive(), t0)

	c, _ := a.Update(TrainRowEvent{Repo: "o/r", Title: "5 of 5: #1 #2", Phase: "assembling", PhaseStartedAt: t0})
	a = c.(ActivePaneComponent)
	c, _ = a.Update(TickEvent{At: t0.Add(10 * time.Second)})
	a = c.(ActivePaneComponent)
	view := a.View(200)
	if !strings.Contains(view, "assembling (00:10)") {
		t.Fatalf("expected 'assembling (00:10)' in view:\n%s", view)
	}

	// New phase at t0+10s: elapsed restarts from zero.
	c, _ = a.Update(TrainRowEvent{Repo: "o/r", Title: "5 of 5: #1 #2", Phase: "trial CI #77", PhaseStartedAt: t0.Add(10 * time.Second)})
	a = c.(ActivePaneComponent)
	view = a.View(200)
	if !strings.Contains(view, "trial CI #77 (00:00)") {
		t.Fatalf("expected elapsed to restart: 'trial CI #77 (00:00)' in view:\n%s", view)
	}

	// A wedged phase shows a visibly larger elapsed.
	c, _ = a.Update(TickEvent{At: t0.Add(10*time.Second + 61*time.Minute)})
	a = c.(ActivePaneComponent)
	view = a.View(200)
	if !strings.Contains(view, "trial CI #77 (61:00)") {
		t.Fatalf("expected a 61-minute phase elapsed in view:\n%s", view)
	}
}

func TestTrainRow_NarrowKeepsPhaseAndCount(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a := startTrainRow(newTrainActive(), t0)
	c, _ := a.Update(TrainRowEvent{Repo: "o/r", Title: "3 of 5: #1555 #1562 #1576 (ejected #1549 #1560 #1561 #1570)", Phase: "bisecting (step 2)", PhaseStartedAt: t0})
	a = c.(ActivePaneComponent)
	c, _ = a.Update(LogEvent{Repo: "o/r", Tag: "merge-train", Message: "some long unrelated log line that should be dropped first"})
	a = c.(ActivePaneComponent)
	c, _ = a.Update(TickEvent{At: t0.Add(5 * time.Second)})
	a = c.(ActivePaneComponent)
	view := a.View(70)
	if !strings.Contains(view, "bisecting (step 2) (00:05)") {
		t.Fatalf("phase must survive narrow layout:\n%s", view)
	}
	if !strings.Contains(view, "3 of") {
		t.Fatalf("leading count must survive narrow layout:\n%s", view)
	}
	if strings.Contains(view, "unrelated log line") {
		t.Fatalf("last log line should be dropped first:\n%s", view)
	}
}

func TestTrainRow_NoPhaseRendersLikeBefore(t *testing.T) {
	t0 := time.Now()
	a := startTrainRow(newTrainActive(), t0)
	view := a.View(120)
	if !strings.Contains(view, "5 of 5: #1 #2 #3 #4 #5") || strings.Contains(view, " · ") {
		t.Fatalf("unexpected view before any phase:\n%s", view)
	}
}

func TestHistory_TrainOutcomeEntry(t *testing.T) {
	redirectHistory(t)
	h := NewHistoryPaneComponent("o/r")
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	c, _ := h.Update(JobCompletedEvent{Repo: "o/r", StageName: "Merge Train", Title: "3 of 3: #1 #2 #3",
		Success: true, Completed: true, Outcome: "landed", Detail: "#1 #2 #3 via PR #50", CompletedAt: at, Duration: 5 * time.Minute})
	h = c.(HistoryPaneComponent)
	c, _ = h.Update(JobCompletedEvent{Repo: "o/r", StageName: "Merge Train", Title: "2 of 2: #4 #5",
		Success: false, Completed: true, Outcome: "abandoned", Detail: "trial CI never started", CompletedAt: at.Add(time.Hour)})
	h = c.(HistoryPaneComponent)
	// Skipped completions still never reach History.
	c, _ = h.Update(JobCompletedEvent{Repo: "o/r", StageName: "Merge Train", Skipped: true})
	h = c.(HistoryPaneComponent)

	if len(h.history) != 2 {
		t.Fatalf("history len = %d, want 2", len(h.history))
	}
	if h.history[0].Outcome != "landed" || h.history[1].Outcome != "abandoned" {
		t.Fatalf("outcomes not carried: %+v", h.history)
	}
	h.historyVP.Height = 5
	h.rebuildViewportContent(200)
	out := h.historyVP.View()
	for _, want := range []string{"landed", "abandoned", "Merge Train", "3 of 3: #1 #2 #3", "✓", "✗"} {
		if !strings.Contains(out, want) {
			t.Errorf("history view missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(retry)") || strings.Contains(out, "(error)") {
		t.Errorf("train entries must not render as retry/error:\n%s", out)
	}
}

func TestHistory_OldEntryWithoutOutcomeRendersAsBefore(t *testing.T) {
	redirectHistory(t)
	h := NewHistoryPaneComponent("o/r")
	h.history = []HistoryEntry{{IssueNumber: 4, StageName: "Plan", Success: true, Completed: false}}
	h.historyVP.Height = 5
	h.rebuildViewportContent(200)
	if out := h.historyVP.View(); !strings.Contains(out, "(retry)") {
		t.Fatalf("old entry must keep its (retry) rendering:\n%s", out)
	}
}

func TestHistory_TrainEntryGuards(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.width, m.height = 80, 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{{IssueNumber: 0, Repo: "o/r", StageName: "Merge Train", Success: true, Outcome: "landed"}}

	_, cmd := m.history.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if cmd != nil {
		t.Errorf("'l' on a train entry must not open a watch")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd != nil {
		t.Errorf("'r' on a train entry must not resume")
	}
	if got := next.(Model).header.statusMsg; !strings.Contains(got, "nothing to resume") {
		t.Errorf("status msg = %q", got)
	}
}

func TestDetail_TrainEntryOutcomeLine(t *testing.T) {
	d := DetailPanelComponent{}
	d.SetVisible(true)
	d.SetItem(&DetailItem{StageName: "Merge Train", Success: true, Completed: true, Outcome: "landed", OutcomeDetail: "#1 via PR #50"})
	v := d.View(80)
	if !strings.Contains(v, "Outcome:  landed") || !strings.Contains(v, "#1 via PR #50") {
		t.Fatalf("detail missing outcome:\n%s", v)
	}
}
