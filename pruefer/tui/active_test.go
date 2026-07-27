package tui

import (
	"strings"
	"testing"
	"time"
)

func TestActivePane_ReviewStartedAddsEntry(t *testing.T) {
	a := newActivePaneComponent()
	start := time.Now()
	comp, _ := a.Update(ReviewStartedEvent{Repo: "handarbeit/fabrik", PRNumber: 5, Title: "fix bug", StartedAt: start})
	a = comp.(ActivePaneComponent)

	if a.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", a.Count())
	}
	got := a.Selected()
	if got == nil || got.PRNumber != 5 || got.Repo != "handarbeit/fabrik" {
		t.Fatalf("Selected() = %+v, want PR #5 on handarbeit/fabrik", got)
	}
}

func TestActivePane_ElapsedTimeAdvancesWithTick(t *testing.T) {
	a := newActivePaneComponent()
	start := time.Now()
	comp, _ := a.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 1, StartedAt: start})
	a = comp.(ActivePaneComponent)

	comp, _ = a.Update(TickEvent{At: start.Add(90 * time.Second)})
	a = comp.(ActivePaneComponent)

	view := a.View(120)
	if !strings.Contains(view, "01:30") {
		t.Errorf("View() does not show elapsed 01:30:\n%s", view)
	}
}

func TestActivePane_ReviewCompletedRemovesEntry(t *testing.T) {
	a := newActivePaneComponent()
	comp, _ := a.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 1, StartedAt: time.Now()})
	a = comp.(ActivePaneComponent)
	comp, _ = a.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 2, StartedAt: time.Now()})
	a = comp.(ActivePaneComponent)
	if a.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", a.Count())
	}

	comp, _ = a.Update(ReviewCompletedEvent{Repo: "o/r", PRNumber: 1})
	a = comp.(ActivePaneComponent)
	if a.Count() != 1 {
		t.Fatalf("after completion, Count() = %d, want 1", a.Count())
	}
	if got := a.Selected(); got == nil || got.PRNumber != 2 {
		t.Fatalf("remaining entry = %+v, want PR #2", got)
	}
}

func TestActivePane_KeyedByRepoAndPRNumber(t *testing.T) {
	a := newActivePaneComponent()
	// Same PR number, different repos — must not collide.
	comp, _ := a.Update(ReviewStartedEvent{Repo: "handarbeit/fabrik", PRNumber: 1, StartedAt: time.Now()})
	a = comp.(ActivePaneComponent)
	comp, _ = a.Update(ReviewStartedEvent{Repo: "handarbeit/other", PRNumber: 1, StartedAt: time.Now()})
	a = comp.(ActivePaneComponent)
	if a.Count() != 2 {
		t.Fatalf("Count() = %d, want 2 (distinct repos, same PR number)", a.Count())
	}
}

func TestActivePane_UnfocusedIgnoresKeys(t *testing.T) {
	a := newActivePaneComponent()
	a.focused = false
	comp, _ := a.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 1, StartedAt: time.Now()})
	a = comp.(ActivePaneComponent)
	comp, _ = a.Update(ReviewStartedEvent{Repo: "o/r", PRNumber: 2, StartedAt: time.Now()})
	a = comp.(ActivePaneComponent)

	comp, _ = a.Update(keyMsg("down"))
	a = comp.(ActivePaneComponent)
	if a.idx != 0 {
		t.Errorf("idx = %d, want 0 (unfocused pane must ignore key events)", a.idx)
	}
}

func TestActivePane_EmptyView(t *testing.T) {
	a := newActivePaneComponent()
	if !strings.Contains(a.View(80), "no reviews in flight") {
		t.Errorf("View() = %q, want placeholder text", a.View(80))
	}
}
