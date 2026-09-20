package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestClassifyUsageLimitExit_APIError429 covers the second structural shape of
// a session-limit exit: terminal_reason "api_error" with api_error_status 429
// (ADR-1811). Only structured fields are consulted; the result text is never
// read.
func TestClassifyUsageLimitExit_APIError429(t *testing.T) {
	tests := []struct {
		name         string
		resp         claudeResponse
		usage        TokenUsage
		wantDetected bool
	}{
		{"429 at zero turns and cost detects", claudeResponse{TerminalReason: "api_error", APIErrorStatus: 429}, TokenUsage{}, true},
		{"429 at one turn and zero cost detects", claudeResponse{TerminalReason: "api_error", APIErrorStatus: 429}, TokenUsage{TurnsUsed: 1}, true},
		{"429 with real turns and cost does not detect (R5)", claudeResponse{TerminalReason: "api_error", APIErrorStatus: 429}, TokenUsage{TurnsUsed: 12, CostUSD: 0.4}, false},
		{"500 does not detect", claudeResponse{TerminalReason: "api_error", APIErrorStatus: 500}, TokenUsage{}, false},
		{"502 does not detect", claudeResponse{TerminalReason: "api_error", APIErrorStatus: 502}, TokenUsage{}, false},
		{"529 does not detect", claudeResponse{TerminalReason: "api_error", APIErrorStatus: 529}, TokenUsage{}, false},
		{"absent/zero status does not detect (R3)", claudeResponse{TerminalReason: "api_error"}, TokenUsage{}, false},
		{"429 with another terminal_reason does not detect", claudeResponse{TerminalReason: "max_turns", APIErrorStatus: 429}, TokenUsage{}, false},
		{"429 with empty terminal_reason does not detect", claudeResponse{APIErrorStatus: 429}, TokenUsage{}, false},
		{
			// R2: session-limit prose in Result is never consulted.
			name:         "session-limit prose with absent status does not detect",
			resp:         claudeResponse{TerminalReason: "api_error", Result: "You've hit your session limit · resets 3:30am (America/New_York)"},
			usage:        TokenUsage{},
			wantDetected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, detected := classifyUsageLimitExit(tt.resp, tt.usage)
			if detected != tt.wantDetected {
				t.Fatalf("detected = %v, want %v (msg=%q)", detected, tt.wantDetected, msg)
			}
			if detected && !strings.Contains(msg, "api_error_status=429") {
				t.Errorf("msg = %q, want it to name api_error_status=429", msg)
			}
		})
	}
}

func interpretRaw(t *testing.T, raw string) (bool, error) {
	t.Helper()
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, []byte(raw), errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir(), "", 2, -1)
	return completed, err
}

// TestInterpretClaudeResult_APIError429_ReturnsUsageLimitError is the
// full-pipeline check using the live-captured exit shape.
func TestInterpretClaudeResult_APIError429_ReturnsUsageLimitError(t *testing.T) {
	raw := `{"type":"result","subtype":"success","is_error":true,` +
		`"terminal_reason":"api_error","api_error_status":429,` +
		`"num_turns":1,"total_cost_usd":0,"duration_ms":458,` +
		`"result":"You've hit your session limit · resets 3:30am (America/New_York)"}`
	completed, err := interpretRaw(t, raw)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var limitErr *claudeUsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("errors.As(err, *claudeUsageLimitError) = false; err = %v", err)
	}
	if limitErr.ResetTime != "" {
		t.Errorf("ResetTime = %q, want empty (never parsed from result text)", limitErr.ResetTime)
	}
	var apiErr *claudeAPIErrorExit
	if errors.As(err, &apiErr) {
		t.Errorf("429 api_error must not also classify as claudeAPIErrorExit")
	}
	if completed {
		t.Errorf("expected completed=false")
	}
}

// TestInterpretClaudeResult_APIErrorNon429_StaysAPIErrorExit covers R3/R4:
// non-429, absent, null, and wrong-typed statuses keep ADR-1458's handling —
// and a wrong-typed status must not drop the whole result object.
func TestInterpretClaudeResult_APIErrorNon429_StaysAPIErrorExit(t *testing.T) {
	for _, status := range []string{
		`,"api_error_status":500`,
		`,"api_error_status":502`,
		`,"api_error_status":529`,
		``,
		`,"api_error_status":0`,
		`,"api_error_status":null`,
		`,"api_error_status":"429"`,
		`,"api_error_status":429.5`,
		`,"api_error_status":{"code":429}`,
	} {
		t.Run(status, func(t *testing.T) {
			raw := `{"result":"You've hit your session limit","terminal_reason":"api_error","is_error":true,"num_turns":1,"total_cost_usd":0` + status + `}`
			_, err := interpretRaw(t, raw)
			var limitErr *claudeUsageLimitError
			if errors.As(err, &limitErr) {
				t.Fatalf("classified as usage limit; err = %v", err)
			}
			var apiErr *claudeAPIErrorExit
			if !errors.As(err, &apiErr) {
				t.Fatalf("errors.As(err, *claudeAPIErrorExit) = false; err = %v", err)
			}
		})
	}
}

// TestInterpretClaudeResult_WrongTypedStatus_BlockingLimitStillClassifies
// guards the parse-drop failure mode: a wrong-typed api_error_status must not
// erase the rest of the result object.
func TestInterpretClaudeResult_WrongTypedStatus_BlockingLimitStillClassifies(t *testing.T) {
	raw := `{"result":"","session_id":"sid-1","terminal_reason":"blocking_limit","is_error":true,"num_turns":0,"total_cost_usd":0,"api_error_status":"429"}`
	_, err := interpretRaw(t, raw)
	var limitErr *claudeUsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("errors.As(err, *claudeUsageLimitError) = false; err = %v", err)
	}
}

func TestInterpretClaudeResult_APIError429_MarkerTakesPrecedence(t *testing.T) {
	completed, err := interpretRaw(t, `{"result":"work done\nFABRIK_STAGE_COMPLETE","terminal_reason":"api_error","api_error_status":429}`)
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("expected marker-completion to take precedence over a 429 classification")
	}
	if !completed {
		t.Errorf("expected completed=true (marker present)")
	}
}

func TestInterpretClaudeResult_APIError429WithRealProgress_NotClassified(t *testing.T) {
	raw := `{"terminal_reason":"api_error","api_error_status":429,"is_error":true,"num_turns":12,"total_cost_usd":0.4321,"result":"partial work"}`
	_, err := interpretRaw(t, raw)
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("429 api_error with real progress was classified as a usage-limit exit")
	}
}
