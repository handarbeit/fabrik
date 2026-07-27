package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestClassifyUsageLimitExit covers structural detection from the CLI's own
// result object (claudeResponse.TerminalReason) — never from output prose.
// See #1183: the original prose-matching detectUsageLimitExit self-triggered
// on any stage whose output merely discussed usage limits.
func TestClassifyUsageLimitExit(t *testing.T) {
	tests := []struct {
		name         string
		resp         claudeResponse
		usage        TokenUsage
		wantDetected bool
	}{
		{
			name:         "blocking_limit with zero turns and zero cost detects",
			resp:         claudeResponse{TerminalReason: "blocking_limit"},
			usage:        TokenUsage{},
			wantDetected: true,
		},
		{
			// The exclusion is a belt-and-suspenders carryover from #1184's
			// prose-path guard: a genuine usage-limit exit terminates
			// immediately, so turns+cost both nonzero rules it out regardless
			// of terminal_reason.
			name:         "blocking_limit with TurnsUsed>0 && CostUSD>0 does not detect",
			resp:         claudeResponse{TerminalReason: "blocking_limit"},
			usage:        TokenUsage{TurnsUsed: 51, CostUSD: 2.2766},
			wantDetected: false,
		},
		{
			// rapid_refill_breaker is a distinct, unconfirmed terminal_reason
			// value — deliberately not treated as a usage limit; see #1183.
			name:         "rapid_refill_breaker does not detect",
			resp:         claudeResponse{TerminalReason: "rapid_refill_breaker"},
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
			resp:         claudeResponse{TerminalReason: "max_turns"},
			usage:        TokenUsage{},
			wantDetected: false,
		},
		{
			// The exclusion is conjunctive: cost without turns (or vice
			// versa) is not evidence the invocation ran, so detection stands.
			name:         "cost recorded but zero turns still detects",
			resp:         claudeResponse{TerminalReason: "blocking_limit"},
			usage:        TokenUsage{TurnsUsed: 0, CostUSD: 0.01},
			wantDetected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, detected := classifyUsageLimitExit(tt.resp, tt.usage)
			if detected != tt.wantDetected {
				t.Fatalf("detected = %v, want %v (msg=%q)", detected, tt.wantDetected, msg)
			}
			if detected && msg == "" {
				t.Errorf("expected non-empty msg when detected")
			}
		})
	}
}

// TestInterpretClaudeResult_UsageLimitExit_ReturnsSentinelError verifies that
// interpretClaudeResult, when Claude exits non-zero with terminal_reason
// "blocking_limit" and no FABRIK_STAGE_COMPLETE marker, returns a
// *claudeUsageLimitError via errors.As rather than the generic "claude
// exited with error" wrapper — this is the seam finalizeStageOutcome (and a
// mocked ClaudeInvoker in item-level tests) classifies on.
func TestInterpretClaudeResult_UsageLimitExit_ReturnsSentinelError(t *testing.T) {
	raw := []byte(`{"result":"","session_id":"sid-1","terminal_reason":"blocking_limit","is_error":true,"num_turns":0,"total_cost_usd":0}`)
	text, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var limitErr *claudeUsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("errors.As(err, *claudeUsageLimitError) = false; err = %v", err)
	}
	if limitErr.ResetTime != "" {
		t.Errorf("ResetTime = %q, want empty — structural detection never populates a parsed reset time", limitErr.ResetTime)
	}
	if completed {
		t.Errorf("expected completed=false for a usage-limit exit")
	}
	_ = text
}

// TestInterpretClaudeResult_TurnCappedQuotingMessage_NotUsageLimit is the
// full-pipeline regression for #1183. The unit-level table test hand-supplies a
// TokenUsage, so it cannot catch the field wiring silently breaking: the raw
// payload here carries both num_turns and a nonzero total_cost_usd, so the
// exclusion gate only fires if tokenUsageFromResponse/parseClaudeJSON actually
// populate TurnsUsed and CostUSD from the JSON.
//
// The shape reproduces the incident: a turn-capped run whose own output quotes
// the usage-limit message (routine in this repository, where issues, specs and
// tests discuss these exact strings). It must NOT return the sentinel — and
// with structural detection, it can't even be considered, since
// terminal_reason is "max_turns", not "blocking_limit".
func TestInterpretClaudeResult_TurnCappedQuotingMessage_NotUsageLimit(t *testing.T) {
	raw := []byte(`{"subtype":"error_max_turns","terminal_reason":"max_turns","is_error":true,` +
		`"num_turns":51,"total_cost_usd":2.2766,` +
		`"result":"added a test fixture: You've hit your session limit · resets 10:20pm (America/Edmonton)",` +
		`"errors":["Reached maximum number of turns (50)"]}`)

	_, completed, usage, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	// Guard the wiring the exclusion depends on: if these are zero, the gate is
	// inert and the assertion below would pass for the wrong reason.
	if usage.TurnsUsed == 0 || usage.CostUSD == 0 {
		t.Fatalf("usage not parsed from raw payload: TurnsUsed=%d CostUSD=%v — exclusion gate would be inert", usage.TurnsUsed, usage.CostUSD)
	}

	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("turn-capped run quoting the usage-limit message was classified as a usage-limit exit (ResetTime=%q); #1183 regression", limitErr.ResetTime)
	}
	var turnErr *claudeTurnLimitError
	if !errors.As(err, &turnErr) {
		t.Errorf("expected a claudeTurnLimitError for a turn-capped exit, got %v", err)
	}
	if completed {
		t.Errorf("expected completed=false for a turn-capped run")
	}
}

// TestInterpretClaudeResult_UsageLimitExit_MarkerTakesPrecedence verifies that
// a genuine stage completion (FABRIK_STAGE_COMPLETE present) is never
// misclassified as a usage-limit exit, even when the structural
// terminal_reason field is also present — the marker check runs first in
// interpretClaudeResult and returns before usage-limit detection is reached.
func TestInterpretClaudeResult_UsageLimitExit_MarkerTakesPrecedence(t *testing.T) {
	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","terminal_reason":"blocking_limit"}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error (non-zero exit)")
	}
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("expected marker-completion to take precedence, got claudeUsageLimitError")
	}
	if !completed {
		t.Errorf("expected completed=true (marker present)")
	}
}

// TestInterpretClaudeResult_UsageLimitExit_UnparseableJSON_NotClassified
// verifies that when JSON parsing fails (ok == false), there is no
// structured result object to trust, so the invocation is never classified
// as a usage-limit exit by any means — it falls through to the generic
// error/timeout handling paths. See #1183 Requirement 2.
func TestInterpretClaudeResult_UsageLimitExit_UnparseableJSON_NotClassified(t *testing.T) {
	raw := []byte("not valid json, but mentions hit your session limit anyway")
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error (non-zero exit)")
	}
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("unparseable JSON must never be classified as a usage-limit exit, got claudeUsageLimitError")
	}
	if completed {
		t.Errorf("expected completed=false")
	}
	if !strings.Contains(err.Error(), "claude exited with error") {
		t.Errorf("expected generic error, got %v", err)
	}
}
