package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel verifies enter in history pane toggles the detail panel.
func TestUpdate_EnterKey_HistoryPane_TogglesDetailPanel(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.width = 80
	m.height = 24
	m.focusPane = paneHistory
	m.history.history = []HistoryEntry{
		{IssueNumber: 99999, StageName: "Research"},
	}
	// enter toggles detail panel — no subprocess launched.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("enter key should return nil cmd (no subprocess)")
	}
	nm := next.(Model)
	if !nm.detailPanel {
		t.Error("expected detailPanel=true after enter in history pane")
	}
}

// TestUpdate_EscapeKey_ClosesDetailPanel verifies escape closes the detail panel.
func TestUpdate_EscapeKey_ClosesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.detailPanel = true

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("escape key should return nil cmd")
	}
	nm := next.(Model)
	if nm.detailPanel {
		t.Error("expected detailPanel=false after escape key")
	}
}

// TestDetailPanel_TurnLimitStatus verifies the detail panel's "Status" line renders
// "incomplete (turn limit)" for a turn-capped entry, distinct from a plain "incomplete"
// retry — part of the issue #1178 rendering fix (also applied in tui/history.go).
func TestDetailPanel_TurnLimitStatus(t *testing.T) {
	var d DetailPanelComponent
	d.SetVisible(true)
	d.SetWidth(80)

	d.SetItem(&DetailItem{IssueNumber: 1, StageName: "Implement", Success: true, TurnLimited: true, Completed: false})
	view := d.View(80)
	if !strings.Contains(view, "incomplete (turn limit)") {
		t.Errorf("expected %q in detail panel view for turn-capped entry, got: %q", "incomplete (turn limit)", view)
	}

	d.SetItem(&DetailItem{IssueNumber: 2, StageName: "Implement", Success: true, TurnLimited: false, Completed: false})
	view = d.View(80)
	if !strings.Contains(view, "Status:   incomplete") || strings.Contains(view, "turn limit") {
		t.Errorf("expected plain %q (no turn limit) in detail panel view for retry entry, got: %q", "incomplete", view)
	}
}

// TestDetailPanel_CommentPassStatus is the detail-panel half of the comment-pass
// fix (the history-pane half is TestViewHistory_TurnLimitClassification's
// comment cases). The two are separate implementations rather than shared code,
// so each needs its own coverage — a regression in one would not be caught by
// the other.
//
// Comment processing never emits FABRIK_STAGE_COMPLETE: every fabrik-*-comment
// skill forbids it ("Do NOT output FABRIK_STAGE_COMPLETE ... returns control to
// the engine without advancing the pipeline"). Completed is therefore false for
// every comment invocation by design, and reporting that as "incomplete"
// mislabels all of them.
func TestDetailPanel_CommentPassStatus(t *testing.T) {
	var d DetailPanelComponent
	d.SetVisible(true)
	d.SetWidth(80)

	// A comment pass that succeeded, was not blocked, and did not hit the turn
	// cap is complete — not "incomplete".
	d.SetItem(&DetailItem{IssueNumber: 1, StageName: "Review", IsComment: true, Success: true, TurnLimited: false, Completed: false})
	view := d.View(80)
	if !strings.Contains(view, "Status:   success") {
		t.Errorf("expected %q for a completed comment pass, got: %q", "Status:   success", view)
	}
	if strings.Contains(view, "incomplete") {
		t.Errorf("comment pass must not render as incomplete, got: %q", view)
	}

	// A comment pass CAN genuinely exhaust its turn budget — the comment skills
	// carry their own "If You Hit the Turn Limit" section — so that must still
	// be reported rather than swallowed by the branch above.
	d.SetItem(&DetailItem{IssueNumber: 2, StageName: "Review", IsComment: true, Success: true, TurnLimited: true, Completed: false})
	view = d.View(80)
	if !strings.Contains(view, "incomplete (turn limit)") {
		t.Errorf("expected %q for a turn-capped comment pass, got: %q", "incomplete (turn limit)", view)
	}

	// The stage-run path is unchanged: a stage that ends without the marker
	// really will be re-dispatched.
	d.SetItem(&DetailItem{IssueNumber: 3, StageName: "Implement", IsComment: false, Success: true, TurnLimited: false, Completed: false})
	view = d.View(80)
	if !strings.Contains(view, "Status:   incomplete") || strings.Contains(view, "turn limit") {
		t.Errorf("stage run without the marker must still render as incomplete, got: %q", view)
	}
}

// TestUpdate_EnterKey_ActivePane_TogglesDetailPanel verifies enter in active pane toggles the detail panel.
func TestUpdate_EnterKey_ActivePane_TogglesDetailPanel(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.width = 80
	m.height = 24
	m.focusPane = paneActive
	m.active.active[activeJobKey("", 99999)] = &activeJob{StageName: "Research", StartedAt: time.Now()}

	// First enter: panel opens.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(Model)
	if cmd != nil {
		t.Error("enter key should return nil cmd (no subprocess)")
	}
	if !nm.detailPanel {
		t.Error("expected detailPanel=true after first enter")
	}

	// Second enter: panel closes.
	next2, _ := nm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	nm2 := next2.(Model)
	if nm2.detailPanel {
		t.Error("expected detailPanel=false after second enter")
	}
}
