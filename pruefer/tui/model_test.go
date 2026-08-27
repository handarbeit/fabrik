package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestModel_QuitOnQAndCtrlC(t *testing.T) {
	m := New([]string{"handarbeit/fabrik"}, time.Now())
	for _, key := range []string{"q", "ctrl+c"} {
		_, cmd := m.Update(keyMsg(key))
		if cmd == nil {
			t.Errorf("Update(%q) returned nil cmd, want tea.Quit", key)
		}
	}
}

func TestModel_TabCyclesFocus(t *testing.T) {
	m := New(nil, time.Now())
	if got := m.FocusedPaneName(); got != "repos" {
		t.Fatalf("initial focus = %q, want repos", got)
	}
	next, _ := m.Update(keyMsg("tab"))
	m = next.(Model)
	if got := m.FocusedPaneName(); got != "active" {
		t.Fatalf("after 1 tab, focus = %q, want active", got)
	}
	next, _ = m.Update(keyMsg("tab"))
	m = next.(Model)
	if got := m.FocusedPaneName(); got != "history" {
		t.Fatalf("after 2 tabs, focus = %q, want history", got)
	}
	next, _ = m.Update(keyMsg("tab"))
	m = next.(Model)
	if got := m.FocusedPaneName(); got != "repos" {
		t.Fatalf("after 3 tabs, focus = %q, want repos (wrapped)", got)
	}
}

func TestModel_EnterTogglesDetailPanel(t *testing.T) {
	m := New(nil, time.Now())
	if m.DetailVisible() {
		t.Fatal("detail panel visible before any enter press")
	}
	next, _ := m.Update(keyMsg("enter"))
	m = next.(Model)
	if !m.DetailVisible() {
		t.Fatal("detail panel not visible after enter")
	}
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)
	if m.DetailVisible() {
		t.Fatal("detail panel still visible after second enter")
	}
}

func TestModel_DetailPanelShowsSelectedActiveReview(t *testing.T) {
	m := New(nil, time.Now())
	start := time.Now()
	next, _ := m.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 1, Title: "fix bug", StartedAt: start})
	m = next.(Model)
	// Focus the active pane, then open detail.
	next, _ = m.Update(keyMsg("tab"))
	m = next.(Model)
	next, _ = m.Update(keyMsg("enter"))
	m = next.(Model)

	if m.detail.item == nil {
		t.Fatal("detail.item is nil, want the selected active review")
	}
	if m.detail.item.PRNumber != 1 || !m.detail.item.IsActive {
		t.Errorf("detail.item = %+v, want active PR #1", m.detail.item)
	}
}

func TestModel_RepoPollEventRoutesToRepoPane(t *testing.T) {
	m := New([]string{"handarbeit/fabrik"}, time.Now())
	next, _ := m.Update(RepoPollEvent{Repo: "handarbeit/fabrik", At: time.Now(), PRCount: 3})
	m = next.(Model)
	if m.repos.repos["handarbeit/fabrik"].PRCount != 3 {
		t.Errorf("repos pane PRCount = %d, want 3", m.repos.repos["handarbeit/fabrik"].PRCount)
	}
}

// TestModel_DerivedRepoSetEventRoutesToRepoPane covers #1641/R4's TUI
// plumbing: a DerivedRepoSetEvent must reach the repos pane through the
// same Model.Update dispatch every other daemon event uses.
func TestModel_DerivedRepoSetEventRoutesToRepoPane(t *testing.T) {
	m := New(nil, time.Now())
	next, _ := m.Update(DerivedRepoSetEvent{
		Repos: []DerivedRepoEntry{{Repo: "handarbeit/fabrik", InstallationID: 111}},
	})
	m = next.(Model)
	if _, ok := m.repos.repos["handarbeit/fabrik"]; !ok {
		t.Fatalf("repos pane = %+v, want handarbeit/fabrik present after DerivedRepoSetEvent", m.repos.repos)
	}
}

func TestModel_ReviewLifecycleRoutesToActiveHistoryAndFooter(t *testing.T) {
	m := New(nil, time.Now())
	start := time.Now()
	next, _ := m.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 9, StartedAt: start})
	m = next.(Model)
	if m.active.Count() != 1 {
		t.Fatalf("active.Count() = %d, want 1 after ReviewStartedEvent", m.active.Count())
	}

	next, _ = m.Update(ReviewCompletedEvent{
		Repo: "o/r", PRNumber: 9, Reviewed: true, NumTurns: 4, CostUSD: 0.02,
		Duration: 30 * time.Second, CompletedAt: time.Now(),
	})
	m = next.(Model)
	if m.active.Count() != 0 {
		t.Errorf("active.Count() = %d, want 0 after ReviewCompletedEvent", m.active.Count())
	}
	if m.history.Count() != 1 {
		t.Errorf("history.Count() = %d, want 1 after ReviewCompletedEvent", m.history.Count())
	}
	if m.footer.ReviewedCount() != 1 {
		t.Errorf("footer.ReviewedCount() = %d, want 1", m.footer.ReviewedCount())
	}
}

func TestModel_RateLimitSnapshotRoutesToFooter(t *testing.T) {
	m := New(nil, time.Now())
	next, _ := m.Update(RateLimitSnapshotEvent{})
	m = next.(Model)
	_ = m.footer.View(100) // no panic; wiring reaches the footer component
}

func TestModel_DropEventRoutesToFooter(t *testing.T) {
	m := New(nil, time.Now())
	next, _ := m.Update(DropEvent{Reason: "dedupe", Total: 3})
	m = next.(Model)
	if got := m.footer.DropCount("dedupe"); got != 3 {
		t.Errorf("footer.DropCount(\"dedupe\") = %d, want 3", got)
	}
}

func TestModel_SignatureDriftRoutesToFooter(t *testing.T) {
	m := New(nil, time.Now())
	next, _ := m.Update(SignatureDriftEvent{Active: true})
	m = next.(Model)
	if !m.footer.SignatureDriftActive() {
		t.Error("footer.SignatureDriftActive() = false after routing SignatureDriftEvent{Active: true}")
	}
}

func TestModel_TickAdvancesHeaderClock(t *testing.T) {
	start := time.Now()
	m := New(nil, start)
	next, _ := m.Update(TickEvent{At: start.Add(5 * time.Minute)})
	m = next.(Model)
	if m.header.now.Before(start.Add(4 * time.Minute)) {
		t.Errorf("header.now did not advance with TickEvent")
	}
}

func TestModel_ViewDoesNotPanicBeforeWindowSize(t *testing.T) {
	m := New([]string{"handarbeit/fabrik"}, time.Now())
	if got := m.View(); got != "Loading..." {
		t.Errorf("View() before WindowSizeMsg = %q, want %q", got, "Loading...")
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = next.(Model)
	_ = m.View() // no panic
}
