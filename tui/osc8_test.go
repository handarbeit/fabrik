package tui

import (
	"strings"
	"testing"
	"time"
)

// TestInjectIssueLink_SupportedTerminal verifies that a hyperlink is injected when
// the terminal supports OSC 8, repo is non-empty, and issueNum is non-zero.
func TestInjectIssueLink_SupportedTerminal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	line := "#340  Research  ✓ 1m23s  2026-04-13 15:00"
	result := injectIssueLink(line, "acme/fabrik", 340)
	if !strings.Contains(result, "https://github.com/acme/fabrik/issues/340") {
		t.Errorf("expected OSC 8 URL in result; got: %q", result)
	}
	if !strings.Contains(result, "#340") {
		t.Errorf("issue text #340 should still appear in result; got: %q", result)
	}
}

// TestInjectIssueLink_UnsupportedTerminal verifies that line is returned unchanged
// when the terminal does not support OSC 8.
func TestInjectIssueLink_UnsupportedTerminal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TERM", "")
	line := "#340  Research  ✓ 1m23s  2026-04-13 15:00"
	result := injectIssueLink(line, "acme/fabrik", 340)
	if result != line {
		t.Errorf("expected unchanged line; got: %q", result)
	}
}

// TestInjectIssueLink_EmptyRepo verifies that line is returned unchanged when repo is empty.
func TestInjectIssueLink_EmptyRepo(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	line := "#340  Research  ✓ 1m23s"
	result := injectIssueLink(line, "", 340)
	if result != line {
		t.Errorf("expected unchanged line when repo is empty; got: %q", result)
	}
}

// TestInjectIssueLink_ZeroIssueNum verifies that line is returned unchanged when issueNum is zero.
func TestInjectIssueLink_ZeroIssueNum(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	line := "#0  Research  ✓ 1m23s"
	result := injectIssueLink(line, "acme/fabrik", 0)
	if result != line {
		t.Errorf("expected unchanged line when issueNum is zero; got: %q", result)
	}
}

// TestInjectIssueLink_OnlyFirstOccurrence verifies that only the first occurrence of
// "#NNN" is replaced (the issue number field, not any mention in the title).
func TestInjectIssueLink_OnlyFirstOccurrence(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	// Issue 340 with "#340" also appearing in the title
	line := "#340  Research  ✓ 1m23s  refs #340 in title"
	result := injectIssueLink(line, "acme/fabrik", 340)
	// Count occurrences of the URL — should be exactly 1
	count := strings.Count(result, "https://github.com/acme/fabrik/issues/340")
	if count != 1 {
		t.Errorf("expected 1 OSC 8 URL injection, got %d; result: %q", count, result)
	}
}

// TestHistoryRowOSC8_Present verifies that history rows contain an OSC 8 link when
// the terminal supports it and the component has a defaultRepo set.
func TestHistoryRowOSC8_Present(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	redirectHistory(t)

	h := NewHistoryPaneComponent("acme/fabrik")
	h.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", Success: true, CompletedAt: time.Now()},
	}
	h.SetLayout(120, 20, false, 0)

	// The viewport content should contain the OSC 8 URL for issue 42.
	content := h.historyVP.View()
	if !strings.Contains(content, "https://github.com/acme/fabrik/issues/42") {
		t.Errorf("expected OSC 8 URL for issue 42 in history viewport; got: %q", content)
	}
}

// TestHistoryRowOSC8_Absent verifies that history rows do not contain an OSC 8 link
// when the terminal does not support OSC 8.
func TestHistoryRowOSC8_Absent(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TERM", "")
	redirectHistory(t)

	h := NewHistoryPaneComponent("acme/fabrik")
	h.history = []HistoryEntry{
		{IssueNumber: 42, StageName: "Research", Success: true, CompletedAt: time.Now()},
	}
	h.SetLayout(120, 20, false, 0)

	content := h.historyVP.View()
	if strings.Contains(content, "https://github.com/acme/fabrik/issues/42") {
		t.Errorf("OSC 8 URL should not appear in non-OSC8 terminal; got: %q", content)
	}
	if !strings.Contains(content, "#42") {
		t.Errorf("issue number #42 should still appear as plain text; got: %q", content)
	}
}

// TestHistoryRowOSC8_PerEntryRepoTakesPrecedence verifies that when a HistoryEntry
// has its own Repo set, that takes precedence over defaultRepo.
func TestHistoryRowOSC8_PerEntryRepoTakesPrecedence(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	redirectHistory(t)

	h := NewHistoryPaneComponent("default/repo")
	h.history = []HistoryEntry{
		{IssueNumber: 99, Repo: "specific/repo", StageName: "Plan", Success: true, CompletedAt: time.Now()},
	}
	h.SetLayout(120, 20, false, 0)

	content := h.historyVP.View()
	if !strings.Contains(content, "https://github.com/specific/repo/issues/99") {
		t.Errorf("expected per-entry repo URL; got: %q", content)
	}
	if strings.Contains(content, "https://github.com/default/repo/issues/99") {
		t.Errorf("defaultRepo should not be used when HistoryEntry.Repo is set; got: %q", content)
	}
}
