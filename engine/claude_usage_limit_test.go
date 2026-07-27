package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDetectUsageLimitExit covers the shapes a real usage-limit exit might
// take (message in a JSON result field, message only in an errors[]-shaped
// fixture, message in non-JSON raw stdout, the weekly-limit variant, and
// reset-time present/absent), plus negative cases that must NOT match a
// genuine unrelated failure.
func TestDetectUsageLimitExit(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantDetected  bool
		wantMsg       string
		wantResetTime string
	}{
		{
			name:          "message in JSON result field with reset time",
			raw:           `{"result":"You've hit your session limit · resets 10:20pm (America/Edmonton)","is_error":true}`,
			wantDetected:  true,
			wantMsg:       "hit your session limit",
			wantResetTime: "10:20pm (America/Edmonton)",
		},
		{
			name: "message only in errors[]-shaped fixture (result empty)",
			raw: `{"subtype":"error_during_execution","is_error":true,"session_id":"sid-1",` +
				`"errors":["You've hit your session limit · resets 6:05am (UTC)"]}`,
			wantDetected:  true,
			wantMsg:       "hit your session limit",
			wantResetTime: "6:05am (UTC)",
		},
		{
			name:          "message in non-JSON raw stdout",
			raw:           "some preamble\nYou've hit your session limit · resets 10:20pm (America/Edmonton)\ntrailing",
			wantDetected:  true,
			wantMsg:       "hit your session limit",
			wantResetTime: "10:20pm (America/Edmonton)",
		},
		{
			// The reset-time regex expects a bare HH:MM(am/pm) — a day-name
			// prefix like "Mon" doesn't match, so this degrades gracefully to
			// detected=true with resetTime="" rather than failing detection.
			name:          "weekly limit variant with day-prefixed reset time",
			raw:           `{"result":"You've hit your weekly limit · resets Mon 9:00am (UTC)"}`,
			wantDetected:  true,
			wantMsg:       "hit your weekly limit",
			wantResetTime: "",
		},
		{
			name:          "reset time absent — degrades gracefully",
			raw:           `{"result":"You've hit your session limit for now."}`,
			wantDetected:  true,
			wantMsg:       "hit your session limit",
			wantResetTime: "",
		},
		{
			name:         "negative: genuine context-limit-exceeded message must not match",
			raw:          `{"result":"Error: context limit exceeded for this conversation"}`,
			wantDetected: false,
		},
		{
			name:         "negative: genuine unrelated rate-limit message must not match",
			raw:          `{"subtype":"error_during_execution","is_error":true,"errors":["rate limit exceeded, please retry"]}`,
			wantDetected: false,
		},
		{
			name:         "negative: empty output",
			raw:          ``,
			wantDetected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, resetTime, detected := detectUsageLimitExit([]byte(tt.raw))
			if detected != tt.wantDetected {
				t.Fatalf("detected = %v, want %v (msg=%q, resetTime=%q)", detected, tt.wantDetected, msg, resetTime)
			}
			if !detected {
				return
			}
			if !strings.EqualFold(msg, tt.wantMsg) {
				t.Errorf("msg = %q, want %q", msg, tt.wantMsg)
			}
			if resetTime != tt.wantResetTime {
				t.Errorf("resetTime = %q, want %q", resetTime, tt.wantResetTime)
			}
		})
	}
}

// TestInterpretClaudeResult_UsageLimitExit_ReturnsSentinelError verifies that
// interpretClaudeResult, when Claude exits non-zero with a usage-limit message
// and no FABRIK_STAGE_COMPLETE marker, returns a *claudeUsageLimitError via
// errors.As rather than the generic "claude exited with error" wrapper — this
// is the seam finalizeStageOutcome (and a mocked ClaudeInvoker in item-level
// tests) classifies on.
func TestInterpretClaudeResult_UsageLimitExit_ReturnsSentinelError(t *testing.T) {
	raw := []byte(`{"result":"You've hit your session limit · resets 10:20pm (America/Edmonton)","num_turns":1,"total_cost_usd":0}`)
	text, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, t.TempDir()+"/sess", t.TempDir())

	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var limitErr *claudeUsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("errors.As(err, *claudeUsageLimitError) = false; err = %v", err)
	}
	if limitErr.ResetTime != "10:20pm (America/Edmonton)" {
		t.Errorf("ResetTime = %q, want %q", limitErr.ResetTime, "10:20pm (America/Edmonton)")
	}
	if completed {
		t.Errorf("expected completed=false for a usage-limit exit")
	}
	if !strings.Contains(text, "hit your session limit") {
		t.Errorf("text = %q, want it to still contain the result text", text)
	}
}

// TestInterpretClaudeResult_UsageLimitExit_MarkerTakesPrecedence verifies that
// a genuine stage completion (FABRIK_STAGE_COMPLETE present) is never
// misclassified as a usage-limit exit, even if the output happens to also
// contain limit-shaped text — the marker check runs first in
// interpretClaudeResult and returns before usage-limit detection is reached.
func TestInterpretClaudeResult_UsageLimitExit_MarkerTakesPrecedence(t *testing.T) {
	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE\nNote: earlier you hit your session limit · resets 10:20pm (America/Edmonton)"}`)
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
