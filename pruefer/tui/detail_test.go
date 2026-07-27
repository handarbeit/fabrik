package tui

import (
	"strings"
	"testing"
	"time"
)

func TestDetailPanel_HiddenByDefault(t *testing.T) {
	var d DetailPanelComponent
	if d.View(80) != "" {
		t.Errorf("View() = %q, want empty when not visible", d.View(80))
	}
	if d.Height() != 0 {
		t.Errorf("Height() = %d, want 0 when not visible", d.Height())
	}
}

func TestDetailPanel_ActiveReviewShowsElapsed(t *testing.T) {
	var d DetailPanelComponent
	d.SetItem(&DetailItem{
		Repo: "handarbeit/fabrik", PRNumber: 5, Title: "fix bug",
		IsActive: true, Elapsed: 90 * time.Second,
	})
	d.SetVisible(true)
	d.SetWidth(100)

	view := d.View(100)
	if !strings.Contains(view, "handarbeit/fabrik#5") {
		t.Errorf("View() missing review key:\n%s", view)
	}
	if !strings.Contains(view, "fix bug") {
		t.Errorf("View() missing title:\n%s", view)
	}
	if !strings.Contains(view, "01:30") {
		t.Errorf("View() missing elapsed time:\n%s", view)
	}
	if d.Height() == 0 {
		t.Error("Height() = 0, want > 0 when visible with an item")
	}
}

func TestDetailPanel_CompletedReviewShowsTurnsCostDuration(t *testing.T) {
	var d DetailPanelComponent
	d.SetItem(&DetailItem{
		Repo: "o/r", PRNumber: 9, Reviewed: true,
		NumTurns: 8, CostUSD: 1.2345, Duration: 45 * time.Second,
		CompletedAt: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	})
	d.SetVisible(true)
	d.SetWidth(100)

	view := d.View(100)
	for _, want := range []string{"Turns:    8", "$1.2345", "00:45", "2026-07-27"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q:\n%s", want, view)
		}
	}
}

func TestDetailPanel_SkippedReviewShowsReason(t *testing.T) {
	var d DetailPanelComponent
	d.SetItem(&DetailItem{Repo: "o/r", PRNumber: 3, Skipped: true, Reason: "draft"})
	d.SetVisible(true)
	d.SetWidth(100)

	if !strings.Contains(d.View(100), "skipped: draft") {
		t.Errorf("View() missing skip reason:\n%s", d.View(100))
	}
}

func TestDetailPanel_ErroredReviewShowsError(t *testing.T) {
	var d DetailPanelComponent
	d.SetItem(&DetailItem{Repo: "o/r", PRNumber: 3, Err: "claude review invocation: boom"})
	d.SetVisible(true)
	d.SetWidth(100)

	if !strings.Contains(d.View(100), "error: claude review invocation: boom") {
		t.Errorf("View() missing error text:\n%s", d.View(100))
	}
}

func TestDetailPanel_NilItemHidesEvenWhenVisible(t *testing.T) {
	var d DetailPanelComponent
	d.SetVisible(true)
	d.SetItem(nil)
	if d.View(100) != "" {
		t.Errorf("View() = %q, want empty when item is nil", d.View(100))
	}
}

func TestDetailPanel_UpdateIsNoOp(t *testing.T) {
	var d DetailPanelComponent
	d.SetItem(&DetailItem{Repo: "o/r", PRNumber: 1})
	d.SetVisible(true)
	comp, cmd := d.Update(TickEvent{At: time.Now()})
	if cmd != nil {
		t.Error("Update() returned a non-nil cmd, want nil (pure view component)")
	}
	after := comp.(DetailPanelComponent)
	if after.item.PRNumber != 1 {
		t.Error("Update() mutated state, want no-op")
	}
}
