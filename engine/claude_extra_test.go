package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSaveSessionIDDirect_EmptyID_IsNoop verifies that an empty session ID is skipped.
func TestSaveSessionIDDirect_EmptyID_IsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "session.txt")
	saveSessionIDDirect(path, "")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should not be created for empty session ID")
	}
}

// TestSaveSessionIDDirect_WritesSID verifies that a non-empty session ID is written.
func TestSaveSessionIDDirect_WritesSID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions", "session.txt")
	saveSessionIDDirect(path, "test-session-id")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session file not written: %v", err)
	}
	if string(content) != "test-session-id" {
		t.Errorf("session content = %q, want %q", content, "test-session-id")
	}
}

// TestSaveDebugLog_WritesFile verifies that saveDebugLog creates a log file.
func TestSaveDebugLog_WritesFile(t *testing.T) {
	// Change to a temp dir so .fabrik/debug is created there
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	saveDebugLog(1, "Research", "some debug output")

	debugDir := filepath.Join(tmpDir, ".fabrik", "debug")
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatalf("debug dir not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one debug log file")
	}
}

// TestIsUserTurnLine verifies user-type detection from NDJSON lines.
func TestIsUserTurnLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`{"type":"user","message":{}}` + "\n", true},
		{`{"type":"assistant","message":{}}` + "\n", false},
		{`{"type":"result","num_turns":5}` + "\n", false},
		{`{"type":"tool_use"}` + "\n", false},
		{"not json\n", false},
		{"", false},
		{"{}\n", false},
	}
	for _, tt := range tests {
		got := isUserTurnLine([]byte(tt.line))
		if got != tt.want {
			t.Errorf("isUserTurnLine(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// TestTurnCountingWriter_CountsUserTurns verifies that turnCountingWriter
// increments count and calls the callback on each user-type NDJSON line,
// not on assistant-type lines.
func TestTurnCountingWriter_CountsUserTurns(t *testing.T) {
	var inner bytes.Buffer
	var calls []struct{ turns, max int }
	prev := claudeTurnProgress
	claudeTurnProgress = func(issueNumber, turnsUsed, maxTurns int) {
		if issueNumber != 42 {
			t.Errorf("unexpected issueNumber %d", issueNumber)
		}
		calls = append(calls, struct{ turns, max int }{turnsUsed, maxTurns})
	}
	defer func() { claudeTurnProgress = prev }()

	tcw := &turnCountingWriter{inner: &inner, issueNumber: 42, maxTurns: 10}

	// Two user lines separated by a non-user line; only user lines should trigger callbacks.
	line1 := []byte(`{"type":"user"}` + "\n")
	line2 := []byte(`{"type":"result","num_turns":1}` + "\n")
	line3 := []byte(`{"type":"user"}` + "\n")

	tcw.Write(line1)
	tcw.Write(line2)
	tcw.Write(line3)

	if len(calls) != 2 {
		t.Fatalf("expected 2 callback calls, got %d", len(calls))
	}
	if calls[0].turns != 1 || calls[0].max != 10 {
		t.Errorf("call[0] = %+v, want {1 10}", calls[0])
	}
	if calls[1].turns != 2 || calls[1].max != 10 {
		t.Errorf("call[1] = %+v, want {2 10}", calls[1])
	}

	// ±1 boundary: at max_turns=2, two user events produce TurnsUsed=2.
	tcw2 := &turnCountingWriter{inner: &inner, issueNumber: 42, maxTurns: 2}
	var boundCalls []int
	claudeTurnProgress = func(_, turnsUsed, _ int) { boundCalls = append(boundCalls, turnsUsed) }
	tcw2.Write([]byte(`{"type":"user"}` + "\n"))
	tcw2.Write([]byte(`{"type":"user"}` + "\n"))
	if len(boundCalls) != 2 || boundCalls[1] != 2 {
		t.Errorf("boundary: got calls %v, want [1 2]", boundCalls)
	}
}

// TestTurnCountingWriter_SplitLine verifies detection when a line arrives in multiple writes.
func TestTurnCountingWriter_SplitLine(t *testing.T) {
	var inner bytes.Buffer
	callCount := 0
	prev := claudeTurnProgress
	claudeTurnProgress = func(_, _, _ int) { callCount++ }
	defer func() { claudeTurnProgress = prev }()

	tcw := &turnCountingWriter{inner: &inner, issueNumber: 1, maxTurns: 5}
	// Split the line across two Write calls.
	tcw.Write([]byte(`{"type":"us`))
	tcw.Write([]byte(`er"}` + "\n"))

	if callCount != 1 {
		t.Errorf("expected 1 callback after split-line write, got %d", callCount)
	}
}

// TestTurnCountingWriter_NilCallback verifies that a nil claudeTurnProgress does not panic.
func TestTurnCountingWriter_NilCallback(t *testing.T) {
	prev := claudeTurnProgress
	claudeTurnProgress = nil
	defer func() { claudeTurnProgress = prev }()

	var inner bytes.Buffer
	tcw := &turnCountingWriter{inner: &inner, issueNumber: 1, maxTurns: 5}
	// Should not panic.
	tcw.Write([]byte(`{"type":"user"}` + "\n"))
}

// TestTurnCountingWriter_MultiToolUseSequence verifies that a realistic turn with
// multiple tool-use blocks (one user event + multiple assistant events) counts as
// exactly one turn, not four.
func TestTurnCountingWriter_MultiToolUseSequence(t *testing.T) {
	var inner bytes.Buffer
	callCount := 0
	prev := claudeTurnProgress
	claudeTurnProgress = func(_, _, _ int) { callCount++ }
	defer func() { claudeTurnProgress = prev }()

	tcw := &turnCountingWriter{inner: &inner, issueNumber: 1, maxTurns: 5}

	// One logical turn: one user event (tool results) followed by three assistant
	// events (text response + two tool_use blocks).
	tcw.Write([]byte(`{"type":"user"}` + "\n"))
	tcw.Write([]byte(`{"type":"assistant"}` + "\n"))
	tcw.Write([]byte(`{"type":"assistant"}` + "\n"))
	tcw.Write([]byte(`{"type":"assistant"}` + "\n"))
	// Second logical turn.
	tcw.Write([]byte(`{"type":"user"}` + "\n"))

	if callCount != 2 {
		t.Errorf("expected 2 callback calls (one per logical turn), got %d", callCount)
	}
}
