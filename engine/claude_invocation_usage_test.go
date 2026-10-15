package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// capturedResumed429 is the alpha#6829 exit shape: a rejected seven_day
// rate_limit_event followed by an api_error/429 result whose turns and cost are
// the resumed session's cumulative figures.
const capturedResumed429 = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":1,"resetsAt":1790175600}}}}` + "\n" +
	`{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","api_error_status":429,"num_turns":1,"total_cost_usd":0.6891,"session_id":"sid-1","result":"You've hit your weekly limit · resets Oct 2 at 4am"}`

const userLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`

// interpretResumed runs raw through interpretClaudeResult as a resumed
// invocation against a session file carrying the given resume-failure count.
func interpretResumed(t *testing.T, raw string, count int) (sessPath string, err error) {
	t.Helper()
	sessPath = filepath.Join(t.TempDir(), "Validate.session")
	if err := os.WriteFile(sessPath, []byte("resumed-sid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		writeResumeFailureCount(count, sessPath, 1)
	}
	_, _, _, err = interpretClaudeResult(context.Background(), 1, []byte(raw), errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 2, -1)
	return sessPath, err
}

func TestInvocationGuardUsage(t *testing.T) {
	cum := TokenUsage{TurnsUsed: 1, CostUSD: 0.6891, InputTokens: 7}
	tests := []struct {
		name    string
		raw     string
		resumed bool
		want    TokenUsage
	}{
		{"fresh without user lines is unchanged", capturedResumed429, false, cum},
		{"fresh with user lines is unchanged", userLine + "\n" + capturedResumed429, false, cum},
		{"resumed without user lines zeroes turns and cost", capturedResumed429, true, TokenUsage{InputTokens: 7}},
		{"resumed with a user line is unchanged", userLine + "\n" + capturedResumed429, true, cum},
		{"resumed tolerates garbage and partial lines", "not json\n{\"type\":\n\n" + capturedResumed429, true, TokenUsage{InputTokens: 7}},
		{
			// Only the line type is read, never message content.
			name:    "resumed ignores a user-looking string inside an assistant payload",
			raw:     `{"type":"assistant","message":{"content":[{"type":"text","text":"{\"type\":\"user\"}"}]}}` + "\n" + capturedResumed429,
			resumed: true,
			want:    TokenUsage{InputTokens: 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := invocationGuardUsage(cum, []byte(tt.raw), tt.resumed); got != tt.want {
				t.Errorf("invocationGuardUsage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestInterpretClaudeResult_Resumed429_IsUsageLimitAndKeepsSession(t *testing.T) {
	for _, count := range []int{0, 1} {
		sessPath, err := interpretResumed(t, capturedResumed429, count)
		var limitErr *claudeUsageLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("count=%d: errors.As(*claudeUsageLimitError) = false; err = %v", count, err)
		}
		if limitErr.ResetAt.IsZero() {
			t.Errorf("count=%d: ResetAt should be parsed from the rate_limit_event (ADR-1815)", count)
		}
		var resumeErr *claudeResumeFailureError
		if errors.As(err, &resumeErr) {
			t.Errorf("count=%d: usage-limit exit must not be a resume failure", count)
		}
		if got := readResumeFailureCount(sessPath); got != count {
			t.Errorf("count=%d: sidecar count = %d, want untouched", count, got)
		}
		if _, statErr := os.Stat(sessPath); statErr != nil {
			t.Errorf("count=%d: session file must remain: %v", count, statErr)
		}
	}
}

func TestInterpretClaudeResult_ConsecutiveResumed429_DoesNotAbandonSession(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Validate.session")
	if err := os.WriteFile(sessPath, []byte("resumed-sid"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _, _, err := interpretClaudeResult(context.Background(), 1, []byte(capturedResumed429), errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 2, -1)
		var limitErr *claudeUsageLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("exit %d: want *claudeUsageLimitError, got %v", i+1, err)
		}
	}
	if _, err := os.Stat(sessPath); err != nil {
		t.Fatalf("session abandoned after consecutive usage-limit exits: %v", err)
	}
	if got := readResumeFailureCount(sessPath); got != 0 {
		t.Errorf("sidecar count = %d, want 0", got)
	}
}

func TestInterpretClaudeResult_ResumedBlockingLimitWithCumulativeCost(t *testing.T) {
	raw := `{"type":"result","is_error":true,"terminal_reason":"blocking_limit","num_turns":3,"total_cost_usd":1.25,"session_id":"sid-1"}`
	_, err := interpretResumed(t, raw, 0)
	var limitErr *claudeUsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("want *claudeUsageLimitError, got %T: %v", err, err)
	}
}

func TestInterpretClaudeResult_ResumedNon429APIErrorWithCumulativeCost(t *testing.T) {
	raw := `{"type":"result","is_error":true,"terminal_reason":"api_error","api_error_status":500,"num_turns":1,"total_cost_usd":0.6891,"session_id":"sid-1"}`
	sessPath, err := interpretResumed(t, raw, 1)
	var apiErr *claudeAPIErrorExit
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *claudeAPIErrorExit (R3), got %T: %v", err, err)
	}
	if got := readResumeFailureCount(sessPath); got != 1 {
		t.Errorf("sidecar count = %d, want 1 (untouched)", got)
	}
}

func TestInterpretClaudeResult_Resumed429AfterRealWork_NotClassified(t *testing.T) {
	_, err := interpretResumed(t, userLine+"\n"+capturedResumed429, 0)
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("a 429 after real work in this invocation must keep the current path; got %v", err)
	}
}

func TestInterpretClaudeResult_RejectedEventWithNonLimitTerminalReason_NotClassified(t *testing.T) {
	raw := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day"}}` + "\n" +
		`{"type":"result","is_error":true,"terminal_reason":"rapid_refill_breaker","num_turns":1,"total_cost_usd":0.6891,"session_id":"sid-1"}`
	_, err := interpretResumed(t, raw, 0)
	var limitErr *claudeUsageLimitError
	if errors.As(err, &limitErr) {
		t.Fatalf("a rejected event alone must not classify; got %v", err)
	}
}

func TestInterpretClaudeResult_Fresh429AtZeroCost_StillClassified(t *testing.T) {
	raw := `{"type":"result","is_error":true,"terminal_reason":"api_error","api_error_status":429,"num_turns":1,"total_cost_usd":0,"session_id":"sid-1"}`
	if _, err := interpretRaw(t, raw); !errors.As(err, new(*claudeUsageLimitError)) {
		t.Fatalf("fresh 429 at $0 must classify; got %v", err)
	}
	// A fresh invocation's usage is its own: cost > 0 means real work, even with no user lines.
	freshWork := `{"type":"result","is_error":true,"terminal_reason":"api_error","api_error_status":429,"num_turns":1,"total_cost_usd":0.6891,"session_id":"sid-1"}`
	if _, err := interpretRaw(t, freshWork); errors.As(err, new(*claudeUsageLimitError)) {
		t.Fatalf("fresh 429 with cost must stay unclassified; got %v", err)
	}
}

// TestInterpretClaudeResult_Resumed429_FailsWithFixNeutralised proves the
// resumed fixture is only classified because of invocationGuardUsage.
func TestInterpretClaudeResult_Resumed429_FailsWithFixNeutralised(t *testing.T) {
	invocationOwnUsageDisabledForTest = true
	t.Cleanup(func() { invocationOwnUsageDisabledForTest = false })

	_, err := interpretResumed(t, capturedResumed429, 0)
	if errors.As(err, new(*claudeUsageLimitError)) {
		t.Fatalf("with the fix neutralised the resumed fixture must not classify as a usage limit")
	}
	if !errors.As(err, new(*claudeResumeFailureError)) {
		t.Fatalf("with the fix neutralised the exit should fall to resume-failure handling; got %v", err)
	}
}
