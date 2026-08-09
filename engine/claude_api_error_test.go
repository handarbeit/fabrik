package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestClassifyAPIErrorExit covers structural detection from the CLI's own
// result object (claudeResponse.TerminalReason) — never from output prose,
// mirroring the discipline TestClassifyUsageLimitExit exercises for
// "blocking_limit". See #1458.
func TestClassifyAPIErrorExit(t *testing.T) {
	tests := []struct {
		name         string
		resp         claudeResponse
		usage        TokenUsage
		wantDetected bool
	}{
		{
			name:         "api_error with zero turns and zero cost detects",
			resp:         claudeResponse{TerminalReason: "api_error"},
			usage:        TokenUsage{},
			wantDetected: true,
		},
		{
			// Observed live #1458 shape: 1 turn, $0.0000 — TurnsUsed>0 alone
			// does not exclude; the guard is conjunctive (both must be
			// nonzero to exclude).
			name:         "api_error with one turn and zero cost detects",
			resp:         claudeResponse{TerminalReason: "api_error"},
			usage:        TokenUsage{TurnsUsed: 1, CostUSD: 0},
			wantDetected: true,
		},
		{
			// Belt-and-suspenders exclusion reused unchanged from
			// classifyUsageLimitExit (#1458 R5): real progress (turns and
			// cost both nonzero) is never classified as a did-not-run exit,
			// regardless of terminal_reason.
			name:         "api_error with TurnsUsed>0 && CostUSD>0 does not detect",
			resp:         claudeResponse{TerminalReason: "api_error"},
			usage:        TokenUsage{TurnsUsed: 51, CostUSD: 2.2766},
			wantDetected: false,
		},
		{
			name:         "blocking_limit does not detect as api_error",
			resp:         claudeResponse{TerminalReason: "blocking_limit"},
			usage:        TokenUsage{},
			wantDetected: false,
		},
		{
			name:         "empty terminal_reason does not detect",
			resp:         claudeResponse{TerminalReason: ""},
			usage:        TokenUsage{},
			wantDetected: false,
		},
		{
			name:         "unrelated terminal_reason does not detect",
			resp:         claudeResponse{TerminalReason: "rapid_refill_breaker"},
			usage:        TokenUsage{},
			wantDetected: false,
		},
		{
			// The exclusion is conjunctive: cost without turns (or vice
			// versa) is not evidence the invocation ran, so detection stands.
			name:         "cost recorded but zero turns still detects",
			resp:         claudeResponse{TerminalReason: "api_error"},
			usage:        TokenUsage{TurnsUsed: 0, CostUSD: 0.01},
			wantDetected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, detected := classifyAPIErrorExit(tt.resp, tt.usage)
			if detected != tt.wantDetected {
				t.Fatalf("detected = %v, want %v (msg=%q)", detected, tt.wantDetected, msg)
			}
			if detected && msg == "" {
				t.Errorf("expected non-empty msg when detected")
			}
		})
	}
}

// TestInterpretClaudeResult_APIErrorExit_ReturnsSentinelError verifies that
// interpretClaudeResult, when Claude exits non-zero with terminal_reason
// "api_error" and no FABRIK_STAGE_COMPLETE marker, returns a
// *claudeAPIErrorExit via errors.As rather than the generic "claude exited
// with error" wrapper or a *claudeUsageLimitError — this is the seam
// finalizeStageOutcome classifies on.
func TestInterpretClaudeResult_APIErrorExit_ReturnsSentinelError(t *testing.T) {
	raw := []byte(`{"result":"","session_id":"sid-1","terminal_reason":"api_error","is_error":true,"num_turns":1,"total_cost_usd":0}`)
	text, completed, usage, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var apiErr *claudeAPIErrorExit
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As(err, *claudeAPIErrorExit) = false; err = %v", err)
	}
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("api_error exit must not also be classified as claudeUsageLimitError")
	}
	if apiErr.TerminalReason != "api_error" {
		t.Errorf("TerminalReason = %q, want %q", apiErr.TerminalReason, "api_error")
	}
	if apiErr.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1", apiErr.NumTurns)
	}
	if usage.TurnsUsed != 1 {
		t.Errorf("usage.TurnsUsed = %d, want 1", usage.TurnsUsed)
	}
	if completed {
		t.Errorf("expected completed=false for an api_error exit")
	}
	_ = text
}

// TestInterpretClaudeResult_APIErrorExit_MarkerTakesPrecedence verifies that
// a genuine stage completion (FABRIK_STAGE_COMPLETE present) is never
// misclassified as an api_error exit, even when the structural
// terminal_reason field is also present — the marker check runs first in
// interpretClaudeResult and returns before api_error detection is reached.
func TestInterpretClaudeResult_APIErrorExit_MarkerTakesPrecedence(t *testing.T) {
	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","terminal_reason":"api_error"}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error (non-zero exit)")
	}
	var apiErr *claudeAPIErrorExit
	if errors.As(err, &apiErr) {
		t.Fatalf("expected marker-completion to take precedence, got claudeAPIErrorExit")
	}
	if !completed {
		t.Errorf("expected completed=true (marker present)")
	}
}

// TestInterpretClaudeResult_APIErrorExit_UnparseableJSON_NotClassified
// verifies that when JSON parsing fails (ok == false), there is no
// structured result object to trust, so the invocation is never classified
// as an api_error exit by any means — it falls through to the generic
// error/timeout handling paths. Mirrors #1183 Requirement 2, applied to the
// new classifier (#1458 Acceptance 6).
func TestInterpretClaudeResult_APIErrorExit_UnparseableJSON_NotClassified(t *testing.T) {
	raw := []byte("not valid json, but mentions api_error anyway")
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error (non-zero exit)")
	}
	var apiErr *claudeAPIErrorExit
	if errors.As(err, &apiErr) {
		t.Fatalf("unparseable JSON must never be classified as an api_error exit, got claudeAPIErrorExit")
	}
	if completed {
		t.Errorf("expected completed=false")
	}
	if !strings.Contains(err.Error(), "claude exited with error") {
		t.Errorf("expected generic error, got %v", err)
	}
}

// TestInterpretClaudeResult_APIErrorWithRealProgress_NotClassified is the
// full-pipeline regression for the guard-exclusion path (#1458 R5/R1): a raw
// payload carrying both nonzero num_turns and nonzero total_cost_usd must
// not be classified as an api_error exit, even when terminal_reason is
// "api_error" — the exclusion gate only fires if tokenUsageFromResponse
// actually populates TurnsUsed/CostUSD from the JSON, so this asserts on the
// parsed usage rather than hand-supplied values (mirrors
// TestInterpretClaudeResult_TurnCappedQuotingMessage_NotUsageLimit's
// pipeline-level shape).
func TestInterpretClaudeResult_APIErrorWithRealProgress_NotClassified(t *testing.T) {
	raw := []byte(`{"terminal_reason":"api_error","is_error":true,` +
		`"num_turns":12,"total_cost_usd":0.4321,` +
		`"result":"partial work before a transient API error",` +
		`"errors":["transient API error"]}`)

	_, completed, usage, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	// Guard the wiring the exclusion depends on: if these are zero, the gate is
	// inert and the assertion below would pass for the wrong reason.
	if usage.TurnsUsed == 0 || usage.CostUSD == 0 {
		t.Fatalf("usage not parsed from raw payload: TurnsUsed=%d CostUSD=%v — exclusion gate would be inert", usage.TurnsUsed, usage.CostUSD)
	}

	var apiErr *claudeAPIErrorExit
	if errors.As(err, &apiErr) {
		t.Fatalf("an api_error exit with real progress (turns=%d, cost=$%v) was classified as a did-not-run exit", usage.TurnsUsed, usage.CostUSD)
	}
	if completed {
		t.Errorf("expected completed=false")
	}
	if !strings.Contains(err.Error(), "claude exited with error") {
		t.Errorf("expected generic error for excluded api_error exit, got %v", err)
	}
}
