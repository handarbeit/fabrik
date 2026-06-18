package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// addActiveJob is a helper to add an active job to the model's active pane.
func addActiveJob(m *Model, issueNumber int, repo, stageName string) {
	key := activeJobKey(repo, issueNumber)
	m.active.active[key] = &activeJob{
		IssueNumber: issueNumber,
		Repo:        repo,
		StageName:   stageName,
		StartedAt:   time.Now(),
	}
}

// TestSKey_ActivePane_NoSelectedJob verifies that s with no active jobs is a no-op.
func TestSKey_ActivePane_NoSelectedJob(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.focusPane = paneActive

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from s with no active jobs")
	}
	if nm.confirmStop {
		t.Error("expected confirmStop=false when no job is selected")
	}
	if nm.pendingStopRequest != nil {
		t.Error("expected pendingStopRequest=nil when no job is selected")
	}
}

// TestSKey_ActivePane_WithSelectedJob verifies that s with a selected job sets
// confirmStop=true and populates pendingStopRequest.
func TestSKey_ActivePane_WithSelectedJob(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.focusPane = paneActive
	addActiveJob(&m, 42, "owner/repo", "Implement")

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd from s key (just shows prompt)")
	}
	if !nm.confirmStop {
		t.Error("expected confirmStop=true after s with selected job")
	}
	if nm.pendingStopRequest == nil {
		t.Fatal("expected pendingStopRequest to be set")
	}
	if nm.pendingStopRequest.IssueNumber != 42 {
		t.Errorf("IssueNumber = %d, want 42", nm.pendingStopRequest.IssueNumber)
	}
	if nm.pendingStopRequest.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", nm.pendingStopRequest.Repo, "owner/repo")
	}
	if nm.pendingStopRequest.StageName != "Implement" {
		t.Errorf("StageName = %q, want %q", nm.pendingStopRequest.StageName, "Implement")
	}
	if !strings.Contains(nm.header.statusMsg, "42") {
		t.Errorf("statusMsg should contain issue number, got %q", nm.header.statusMsg)
	}
}

// TestSKey_ThenY_SendsStopRequest verifies that s then y sends a StopRequest on stopCh.
func TestSKey_ThenY_SendsStopRequest(t *testing.T) {
	stopCh := make(chan StopRequest, 1)
	m := New(30, ProjectInfo{}, "", nil, stopCh, 0, false)
	m.focusPane = paneActive
	addActiveJob(&m, 99, "owner/repo", "Research")

	// Press s to show prompt.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = next.(Model)
	if !m.confirmStop {
		t.Fatal("precondition: confirmStop should be true after s")
	}

	// Press y to confirm.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd after y confirms stop")
	}
	if nm.confirmStop {
		t.Error("expected confirmStop=false after y")
	}
	if nm.pendingStopRequest != nil {
		t.Error("expected pendingStopRequest=nil after y")
	}
	if !strings.Contains(nm.header.statusMsg, "99") {
		t.Errorf("statusMsg should reference issue number, got %q", nm.header.statusMsg)
	}

	// Channel should have received the stop request.
	select {
	case req := <-stopCh:
		if req.IssueNumber != 99 {
			t.Errorf("StopRequest.IssueNumber = %d, want 99", req.IssueNumber)
		}
		if req.Repo != "owner/repo" {
			t.Errorf("StopRequest.Repo = %q, want %q", req.Repo, "owner/repo")
		}
		if req.StageName != "Research" {
			t.Errorf("StopRequest.StageName = %q, want %q", req.StageName, "Research")
		}
	default:
		t.Error("expected StopRequest on stopCh after y confirmation")
	}
}

// TestSKey_ThenN_ClearsConfirm verifies that s then n clears confirmStop without sending.
func TestSKey_ThenN_ClearsConfirm(t *testing.T) {
	stopCh := make(chan StopRequest, 1)
	m := New(30, ProjectInfo{}, "", nil, stopCh, 0, false)
	m.focusPane = paneActive
	addActiveJob(&m, 42, "", "Plan")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = next.(Model)
	if !m.confirmStop {
		t.Fatal("precondition: confirmStop should be true after s")
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd after n cancels stop")
	}
	if nm.confirmStop {
		t.Error("expected confirmStop=false after n")
	}
	if nm.pendingStopRequest != nil {
		t.Error("expected pendingStopRequest=nil after n")
	}
	if nm.header.statusMsg != "" {
		t.Errorf("expected empty statusMsg after cancel, got %q", nm.header.statusMsg)
	}

	// Channel should NOT have received anything.
	select {
	case req := <-stopCh:
		t.Errorf("unexpected StopRequest on stopCh after n: %+v", req)
	default:
		// ok
	}
}

// TestSKey_ThenEsc_ClearsConfirm verifies that s then esc clears confirmStop without sending.
func TestSKey_ThenEsc_ClearsConfirm(t *testing.T) {
	stopCh := make(chan StopRequest, 1)
	m := New(30, ProjectInfo{}, "", nil, stopCh, 0, false)
	m.focusPane = paneActive
	addActiveJob(&m, 42, "", "Plan")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = next.(Model)
	if !m.confirmStop {
		t.Fatal("precondition: confirmStop should be true after s")
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	nm := next.(Model)

	if cmd != nil {
		t.Error("expected nil cmd after esc cancels stop")
	}
	if nm.confirmStop {
		t.Error("expected confirmStop=false after esc")
	}
	if nm.pendingStopRequest != nil {
		t.Error("expected pendingStopRequest=nil after esc")
	}

	select {
	case req := <-stopCh:
		t.Errorf("unexpected StopRequest on stopCh after esc: %+v", req)
	default:
		// ok
	}
}

// TestSKey_TickEvent_RepromptsPersists verifies that a TickEvent re-shows the stop
// prompt when confirmStop is active.
func TestSKey_TickEvent_PromptPersists(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.focusPane = paneActive
	addActiveJob(&m, 77, "", "Validate")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = next.(Model)
	if !m.confirmStop {
		t.Fatal("precondition: confirmStop should be true")
	}

	// TickEvent should re-show the prompt.
	next, _ = m.Update(TickEvent{At: time.Now()})
	nm := next.(Model)

	if !nm.confirmStop {
		t.Error("expected confirmStop still true after tick")
	}
	if nm.header.statusMsg == "" {
		t.Error("expected prompt to be re-shown after tick cleared statusMsg")
	}
	if !strings.Contains(nm.header.statusMsg, "77") {
		t.Errorf("re-shown prompt should reference issue number, got %q", nm.header.statusMsg)
	}
}

// TestSKey_ConfirmStopNotFiredWhenConfirmUpgradeActive verifies that y when both
// confirmStop and confirmUpgrade are true fires confirmStop (it has priority).
func TestSKey_ConfirmStopPriorityOverConfirmUpgrade(t *testing.T) {
	stopCh := make(chan StopRequest, 1)
	m := New(30, ProjectInfo{}, "", nil, stopCh, 2, false)
	m.focusPane = paneActive
	addActiveJob(&m, 55, "", "Implement")

	// Set both confirm states simultaneously.
	m.confirmUpgrade = true
	m.confirmStop = true
	m.pendingStopRequest = &StopRequest{IssueNumber: 55, StageName: "Implement"}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	nm := next.(Model)

	// confirmStop should have fired (it has priority).
	if nm.confirmStop {
		t.Error("expected confirmStop=false after y")
	}
	// confirmUpgrade should remain true (not consumed by this y).
	if !nm.confirmUpgrade {
		t.Error("expected confirmUpgrade=true (not consumed by this y press)")
	}
	// Channel should have received the request.
	select {
	case req := <-stopCh:
		if req.IssueNumber != 55 {
			t.Errorf("StopRequest.IssueNumber = %d, want 55", req.IssueNumber)
		}
	default:
		t.Error("expected StopRequest on stopCh when confirmStop fires before confirmUpgrade")
	}
}

// TestSKey_NilStopCh_NoOp verifies that s then y with a nil stopCh is a no-op
// (no panic, no send).
func TestSKey_NilStopCh_NoOp(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.focusPane = paneActive
	addActiveJob(&m, 42, "", "Implement")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = next.(Model)

	// y with nil stopCh should not panic.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	nm := next.(Model)

	if nm.confirmStop {
		t.Error("expected confirmStop=false after y")
	}
}

// TestSKey_Warnings_ForwardedToWarnings verifies that s in paneWarnings is still
// forwarded to the warnings pane (existing behavior preserved).
func TestSKey_Warnings_ForwardedToWarnings(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.focusPane = paneWarnings

	// s in paneWarnings should not affect confirmStop.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	nm := next.(Model)

	if nm.confirmStop {
		t.Error("expected confirmStop=false in paneWarnings; s should forward to warnings")
	}
}

// TestStopRequest_Fields verifies the StopRequest struct has the expected exported fields.
func TestStopRequest_Fields(t *testing.T) {
	req := StopRequest{}
	if req.IssueNumber != 0 {
		t.Errorf("IssueNumber zero value = %d, want 0", req.IssueNumber)
	}
	if req.Repo != "" {
		t.Errorf("Repo zero value = %q, want empty", req.Repo)
	}
	if req.StageName != "" {
		t.Errorf("StageName zero value = %q, want empty", req.StageName)
	}

	req2 := StopRequest{IssueNumber: 7, Repo: "a/b", StageName: "Plan"}
	if req2.IssueNumber != 7 || req2.Repo != "a/b" || req2.StageName != "Plan" {
		t.Errorf("unexpected field values: %+v", req2)
	}

	// Verify the struct compiles and is usable as described.
	_ = fmt.Sprintf("stop #%d (%s) at %s", req2.IssueNumber, req2.Repo, req2.StageName)
}
