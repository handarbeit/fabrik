package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise interpretClaudeResult, extracted from runClaude, in
// isolation from process invocation.

func TestInterpretClaudeResult_SuccessWithCompletionMarker(t *testing.T) {
	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sid-1","num_turns":3,"total_cost_usd":0.5}`)
	text, completed, usage, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !completed {
		t.Errorf("expected completed=true")
	}
	if !strings.Contains(text, "FABRIK_STAGE_COMPLETE") {
		t.Errorf("text = %q, want marker present", text)
	}
	if usage.TurnsUsed != 3 {
		t.Errorf("usage.TurnsUsed = %d, want 3", usage.TurnsUsed)
	}
}

func TestInterpretClaudeResult_SuccessNoCompletionMarker(t *testing.T) {
	raw := []byte(`{"result":"still working","session_id":"sid-2"}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Errorf("expected completed=false")
	}
}

func TestInterpretClaudeResult_RunErrWithoutMarker_EngineShutdown(t *testing.T) {
	raw := []byte(`{"result":"partial","session_id":"sid-3"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate engine shutdown: ctx.Err() != nil
	_, completed, _, err := interpretClaudeResult(ctx, 1, raw, errors.New("boom"), false, t.TempDir()+"/sess", t.TempDir())
	if err == nil {
		t.Fatalf("expected error")
	}
	if completed {
		t.Errorf("expected completed=false on engine-shutdown path")
	}
}

func TestInterpretClaudeResult_RunErrWithMarker_TreatedAsCompleted(t *testing.T) {
	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sid-4"}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit 1"), false, t.TempDir()+"/sess", t.TempDir())
	if err == nil {
		t.Fatalf("expected error to be returned alongside completed=true")
	}
	if !completed {
		t.Errorf("expected completed=true when marker present despite non-zero exit")
	}
}

func TestInterpretClaudeResult_ParseFailure_NotTimedOut(t *testing.T) {
	raw := []byte(`not json at all`)
	text, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, t.TempDir()+"/sess", "/some/log/dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Errorf("expected completed=false")
	}
	if !strings.Contains(text, "could not be parsed") {
		t.Errorf("text = %q, want parse-failure message", text)
	}
}

func TestInterpretClaudeResult_ParseFailure_TimedOut_ExtractsAssistantText(t *testing.T) {
	raw := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"partial output\nFABRIK_STAGE_COMPLETE"}]}}` + "\n")
	text, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, true, t.TempDir()+"/sess", t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !completed {
		t.Errorf("expected completed=true (marker recovered from streamed assistant text)")
	}
	if !strings.Contains(text, "partial output") {
		t.Errorf("text = %q, want recovered assistant text", text)
	}
}

func TestInterpretClaudeResult_WaitDelayOverride_TreatedAsCleanExit(t *testing.T) {
	raw := []byte(`{"result":"done\nFABRIK_STAGE_COMPLETE","session_id":"sid-5"}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, exec.ErrWaitDelay, false, t.TempDir()+"/sess", t.TempDir())
	if err != nil {
		t.Fatalf("expected WaitDelay to be treated as clean exit, got error: %v", err)
	}
	if !completed {
		t.Errorf("expected completed=true")
	}
}

// TestInterpretClaudeResult_StaleSession_DeletesFileAndDoesNotResave uses the
// exact production payload shape from #1128: error_during_execution subtype,
// an errors[] entry naming the dead session ID, that same ID echoed back in
// session_id, and zero turns/cost. Before the fix, saveSessionIDDirect would
// unconditionally rewrite the just-deleted file with this echoed dead ID,
// making the stale pointer self-renewing instead of merely stale.
func TestInterpretClaudeResult_StaleSession_DeletesFileAndDoesNotResave(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "issue-815", "Specify.session")
	if err := os.MkdirAll(filepath.Dir(sessPath), 0700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(sessPath, []byte("3fefda47-9ea5-422a-9cdb-33da6e13244d"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	raw := []byte(`{"subtype":"error_during_execution","duration_ms":0,"num_turns":0,"total_cost_usd":0,` +
		`"session_id":"3fefda47-9ea5-422a-9cdb-33da6e13244d","is_error":true,` +
		`"errors":["No conversation found with session ID: 3fefda47-9ea5-422a-9cdb-33da6e13244d"]}`)

	text, completed, _, err := interpretClaudeResult(context.Background(), 815, raw, nil, false, sessPath, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed {
		t.Errorf("expected completed=false (no FABRIK_STAGE_COMPLETE marker in this error response)")
	}
	if text != "" {
		t.Errorf("text = %q, want empty result text", text)
	}
	if _, statErr := os.Stat(sessPath); !os.IsNotExist(statErr) {
		t.Errorf("expected stale session file to be deleted and NOT recreated, stat err = %v", statErr)
	}
}

// TestInterpretClaudeResult_NonStaleError_StillSavesSession guards the
// narrow-scoping decision: an IsError response that does not match both the
// error_during_execution subtype and the dead-session errors[] substring
// must still persist its session ID exactly as before (e.g. transient
// errors like rate limits, where the session remains valid and resumable).
func TestInterpretClaudeResult_NonStaleError_StillSavesSession(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Specify.session")

	raw := []byte(`{"subtype":"error_during_execution","is_error":true,"session_id":"sid-live",` +
		`"errors":["rate limit exceeded"]}`)

	_, _, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, sessPath, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, statErr := os.ReadFile(sessPath)
	if statErr != nil {
		t.Fatalf("expected session file to be saved, stat err = %v", statErr)
	}
	if string(got) != "sid-live" {
		t.Errorf("session file content = %q, want %q", got, "sid-live")
	}
}

// TestInterpretClaudeResult_TurnCapExit_ClassifiedAsTurnLimitError uses the
// exact production payload shape from #1114 (Implement,
// Implement-20260727-125747-*.log): subtype error_max_turns, terminal_reason
// max_turns, is_error true, and a CLI-reported num_turns (51) past the
// configured cap (50) — confirming the classification is read directly from
// subtype rather than inferred from a turn-count comparison. session_id is
// included per the confirmed carve-out (#1081): the CLI still reports
// session_id on a max_turns hit even though result text is empty, which is
// what parseClaudeJSON's ok-gate requires to accept the payload.
func TestInterpretClaudeResult_TurnCapExit_ClassifiedAsTurnLimitError(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"error_max_turns","terminal_reason":"max_turns",` +
		`"session_id":"sid-1114","errors":["Reached maximum number of turns (50)"],"is_error":true,"num_turns":51}`)

	_, completed, _, err := interpretClaudeResult(context.Background(), 1114, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())
	if err == nil {
		t.Fatalf("expected error to be returned")
	}
	if completed {
		t.Errorf("expected completed=false (no FABRIK_STAGE_COMPLETE marker)")
	}
	var turnLimitErr *claudeTurnLimitError
	if !errors.As(err, &turnLimitErr) {
		t.Fatalf("expected *claudeTurnLimitError, got %T: %v", err, err)
	}
	if turnLimitErr.TerminalReason != "max_turns" {
		t.Errorf("TerminalReason = %q, want %q", turnLimitErr.TerminalReason, "max_turns")
	}
	if turnLimitErr.NumTurns != 51 {
		t.Errorf("NumTurns = %d, want 51", turnLimitErr.NumTurns)
	}
}

// TestInterpretClaudeResult_GenuineError_NotClassifiedAsTurnLimit uses the
// exact production payload shape from #1128 (reaped session): subtype
// error_during_execution, distinct from error_max_turns. It must classify as
// a generic error, never as *claudeTurnLimitError.
func TestInterpretClaudeResult_GenuineError_NotClassifiedAsTurnLimit(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"error_during_execution","session_id":"3fefda47-9ea5-422a-9cdb-33da6e13244d",` +
		`"errors":["No conversation found with session ID: 3fefda47-…"],"is_error":true,"num_turns":0}`)

	_, completed, _, err := interpretClaudeResult(context.Background(), 1128, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())
	if err == nil {
		t.Fatalf("expected error to be returned")
	}
	if completed {
		t.Errorf("expected completed=false")
	}
	var turnLimitErr *claudeTurnLimitError
	if errors.As(err, &turnLimitErr) {
		t.Errorf("expected genuine error NOT to be classified as *claudeTurnLimitError, got %v", turnLimitErr)
	}
}

// TestInterpretClaudeResult_IncompleteWithoutCap_NotClassifiedAsTurnLimit
// covers a non-zero exit with neither a completion marker nor any recognized
// subtype (e.g. a crash or unrecognized failure) — must fall through to the
// generic error path, not be misclassified as a turn cap.
func TestInterpretClaudeResult_IncompleteWithoutCap_NotClassifiedAsTurnLimit(t *testing.T) {
	raw := []byte(`{"result":"partial work, no marker","session_id":"sid-6"}`)

	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())
	if err == nil {
		t.Fatalf("expected error to be returned")
	}
	if completed {
		t.Errorf("expected completed=false")
	}
	var turnLimitErr *claudeTurnLimitError
	if errors.As(err, &turnLimitErr) {
		t.Errorf("expected incomplete-without-cap NOT to be classified as *claudeTurnLimitError, got %v", turnLimitErr)
	}
}

// TestInterpretClaudeResult_HealthyResume_SessionIDSaved confirms the
// ordinary success path (no IsError) still persists the session ID —
// the resave guard must not regress normal resume behavior.
func TestInterpretClaudeResult_HealthyResume_SessionIDSaved(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Specify.session")

	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sid-healthy","num_turns":3,"total_cost_usd":0.5}`)

	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, sessPath, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !completed {
		t.Errorf("expected completed=true")
	}
	got, statErr := os.ReadFile(sessPath)
	if statErr != nil {
		t.Fatalf("expected session file to be saved, stat err = %v", statErr)
	}
	if string(got) != "sid-healthy" {
		t.Errorf("session file content = %q, want %q", got, "sid-healthy")
	}
}
