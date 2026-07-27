package tui

import (
	"strings"
	"testing"
	"time"
)

// allSkipReasons mirrors pruefer.SkipReason's seven values (as plain
// strings, since this package cannot import pruefer without creating an
// import cycle — pruefer imports pruefer/tui).
var allSkipReasons = []string{
	"draft",
	"self-authored: PR author is the review identity",
	"excluded author",
	"excluded label",
	"excluded path: every touched file matches an exclusion glob",
	"already reviewed at this head SHA",
	"diff exceeds max_diff_bytes",
}

func TestHistoryPane_AllSkipReasonsRenderInHistory(t *testing.T) {
	var h HistoryPaneComponent
	for i, reason := range allSkipReasons {
		comp, _ := h.Update(ReviewCompletedEvent{
			Repo: "handarbeit/fabrik", PRNumber: 100 + i, Skipped: true, Reason: reason,
			CompletedAt: time.Now(),
		})
		h = comp.(HistoryPaneComponent)
	}
	if h.Count() != len(allSkipReasons) {
		t.Fatalf("Count() = %d, want %d", h.Count(), len(allSkipReasons))
	}
	view := h.View(120)
	for _, reason := range allSkipReasons {
		if !strings.Contains(view, reason) {
			t.Errorf("View() does not contain skip reason %q:\n%s", reason, view)
		}
	}
}

func TestHistoryPane_ErroredEntryRendersError(t *testing.T) {
	var h HistoryPaneComponent
	comp, _ := h.Update(ReviewCompletedEvent{
		Repo: "handarbeit/fabrik", PRNumber: 42, Err: "cloning PR head: boom",
		CompletedAt: time.Now(),
	})
	h = comp.(HistoryPaneComponent)

	entry := h.Selected()
	if entry == nil {
		t.Fatal("Selected() = nil, want the errored entry")
	}
	if entry.Err != "cloning PR head: boom" {
		t.Errorf("Selected().Err = %q, want %q", entry.Err, "cloning PR head: boom")
	}
	if !strings.Contains(h.View(120), "boom") {
		t.Errorf("View() does not render the error text:\n%s", h.View(120))
	}
}

func TestHistoryPane_ReviewedEntryRendersTurnsAndCost(t *testing.T) {
	var h HistoryPaneComponent
	comp, _ := h.Update(ReviewCompletedEvent{
		Repo: "handarbeit/fabrik", PRNumber: 7, Reviewed: true,
		NumTurns: 12, CostUSD: 0.4321, Duration: 90 * time.Second,
		CompletedAt: time.Now(),
	})
	h = comp.(HistoryPaneComponent)

	view := h.View(120)
	if !strings.Contains(view, "12 turns") {
		t.Errorf("View() does not render turn count:\n%s", view)
	}
	if !strings.Contains(view, "$0.4321") {
		t.Errorf("View() does not render cost:\n%s", view)
	}
}

func TestHistoryPane_RingBufferEvictsOldestAtCapacity(t *testing.T) {
	var h HistoryPaneComponent
	for i := 0; i < maxHistoryEntries; i++ {
		comp, _ := h.Update(ReviewCompletedEvent{
			Repo: "o/r", PRNumber: i, Reviewed: true, CompletedAt: time.Now(),
		})
		h = comp.(HistoryPaneComponent)
	}
	if h.Count() != maxHistoryEntries {
		t.Fatalf("Count() = %d, want %d", h.Count(), maxHistoryEntries)
	}
	entries := h.Entries()
	if entries[0].PRNumber != 0 {
		t.Fatalf("oldest entry PRNumber = %d, want 0", entries[0].PRNumber)
	}

	// One more entry pushes total to maxHistoryEntries+1 — the oldest (PR #0)
	// must be evicted, and the buffer must stay at exactly maxHistoryEntries.
	comp, _ := h.Update(ReviewCompletedEvent{
		Repo: "o/r", PRNumber: maxHistoryEntries, Reviewed: true, CompletedAt: time.Now(),
	})
	h = comp.(HistoryPaneComponent)

	if h.Count() != maxHistoryEntries {
		t.Fatalf("after overflow, Count() = %d, want %d", h.Count(), maxHistoryEntries)
	}
	entries = h.Entries()
	if entries[0].PRNumber != 1 {
		t.Errorf("after overflow, oldest entry PRNumber = %d, want 1 (PR #0 evicted)", entries[0].PRNumber)
	}
	if entries[len(entries)-1].PRNumber != maxHistoryEntries {
		t.Errorf("after overflow, newest entry PRNumber = %d, want %d", entries[len(entries)-1].PRNumber, maxHistoryEntries)
	}
}

func TestHistoryPane_SelectedNavigatesWithKeys(t *testing.T) {
	h := HistoryPaneComponent{focused: true}
	for i := 0; i < 3; i++ {
		comp, _ := h.Update(ReviewCompletedEvent{Repo: "o/r", PRNumber: i, Reviewed: true, CompletedAt: time.Now()})
		h = comp.(HistoryPaneComponent)
	}
	// idx 0 = most recently completed (PR #2).
	if got := h.Selected(); got == nil || got.PRNumber != 2 {
		t.Fatalf("initial Selected() = %+v, want PR #2", got)
	}
	comp, _ := h.Update(keyMsg("down"))
	h = comp.(HistoryPaneComponent)
	if got := h.Selected(); got == nil || got.PRNumber != 1 {
		t.Fatalf("after down, Selected() = %+v, want PR #1", got)
	}
	comp, _ = h.Update(keyMsg("up"))
	h = comp.(HistoryPaneComponent)
	if got := h.Selected(); got == nil || got.PRNumber != 2 {
		t.Fatalf("after up, Selected() = %+v, want PR #2", got)
	}
}

func TestHistoryPane_UnfocusedIgnoresKeys(t *testing.T) {
	h := HistoryPaneComponent{focused: false}
	for i := 0; i < 2; i++ {
		comp, _ := h.Update(ReviewCompletedEvent{Repo: "o/r", PRNumber: i, Reviewed: true, CompletedAt: time.Now()})
		h = comp.(HistoryPaneComponent)
	}
	comp, _ := h.Update(keyMsg("down"))
	h = comp.(HistoryPaneComponent)
	if h.idx != 0 {
		t.Errorf("idx = %d, want 0 (unfocused pane must ignore key events)", h.idx)
	}
}

func TestHistoryPane_EmptyView(t *testing.T) {
	var h HistoryPaneComponent
	if !strings.Contains(h.View(80), "no completed reviews yet") {
		t.Errorf("View() = %q, want placeholder text", h.View(80))
	}
	if h.Height() != 4 { // 1 placeholder line + 3
		t.Errorf("Height() = %d, want 4", h.Height())
	}
}
