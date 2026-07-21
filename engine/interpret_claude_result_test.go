package engine

import (
	"context"
	"errors"
	"os/exec"
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
