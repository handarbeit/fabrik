package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise interpretClaudeResult's consecutive-resume-failure
// classification (#1414): classifyResumeFailure's increment/threshold/abandon
// logic, and the reset points at success, completed-despite-error, and
// turn-cap exits. See classifyResumeFailure's doc comment for the full
// per-outcome table.

// genericFailureRaw is a parseable result payload matching neither the
// stale-session, turn-cap, usage-limit, nor api_error classifiers — the
// generic fallthrough classifyResumeFailure is reached from.
const genericFailureRaw = `{"result":"partial work, no marker","session_id":"sid-generic"}`

func TestClassifyResumeFailure_BelowThreshold_IncrementsWithoutDeletingSession(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	if err := os.WriteFile(sessPath, []byte("resumed-sid"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	raw := []byte(genericFailureRaw)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 3)
	if completed {
		t.Errorf("expected completed=false")
	}

	var resumeErr *claudeResumeFailureError
	if !errors.As(err, &resumeErr) {
		t.Fatalf("expected *claudeResumeFailureError, got %T: %v", err, err)
	}
	if resumeErr.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", resumeErr.ConsecutiveFailures)
	}
	if resumeErr.Abandoned {
		t.Errorf("expected Abandoned=false below threshold")
	}
	if resumeErr.SessionID != "resumed-sid" {
		t.Errorf("SessionID = %q, want %q", resumeErr.SessionID, "resumed-sid")
	}

	// Session file must survive — only abandonment removes it.
	if _, statErr := os.Stat(sessPath); statErr != nil {
		t.Errorf("expected session file to survive a below-threshold failure, stat err = %v", statErr)
	}
	if got := readResumeFailureCount(sessPath); got != 1 {
		t.Errorf("sidecar count = %d, want 1", got)
	}
}

func TestClassifyResumeFailure_AtThreshold_AbandonsSessionAndResetsSidecar(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	if err := os.WriteFile(sessPath, []byte("resumed-sid"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Simulate one prior consecutive failure already recorded.
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(genericFailureRaw)
	_, _, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 2)

	var resumeErr *claudeResumeFailureError
	if !errors.As(err, &resumeErr) {
		t.Fatalf("expected *claudeResumeFailureError, got %T: %v", err, err)
	}
	if resumeErr.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", resumeErr.ConsecutiveFailures)
	}
	if !resumeErr.Abandoned {
		t.Errorf("expected Abandoned=true at threshold")
	}
	if resumeErr.Threshold != 2 {
		t.Errorf("Threshold = %d, want 2", resumeErr.Threshold)
	}

	if _, statErr := os.Stat(sessPath); !os.IsNotExist(statErr) {
		t.Errorf("expected session file to be removed at threshold, stat err = %v", statErr)
	}
	if got := readResumeFailureCount(sessPath); got != 0 {
		t.Errorf("sidecar count after abandonment = %d, want 0 (reset)", got)
	}
}

func TestClassifyResumeFailure_Success_ResetsExistingCount(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sid-healthy","num_turns":3,"total_cost_usd":0.5}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, nil, false, sessPath, t.TempDir(), "resumed-sid", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !completed {
		t.Errorf("expected completed=true")
	}
	if got := readResumeFailureCount(sessPath); got != 0 {
		t.Errorf("sidecar count after success = %d, want 0 (reset)", got)
	}
}

func TestClassifyResumeFailure_CompletedDespiteError_ResetsExistingCount(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(`{"result":"work done\nFABRIK_STAGE_COMPLETE","session_id":"sid-4"}`)
	_, completed, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit 1"), false, sessPath, t.TempDir(), "resumed-sid", 2)
	if err == nil {
		t.Fatalf("expected error to be returned alongside completed=true")
	}
	if !completed {
		t.Errorf("expected completed=true when marker present despite non-zero exit")
	}
	if got := readResumeFailureCount(sessPath); got != 0 {
		t.Errorf("sidecar count after completed-despite-error = %d, want 0 (reset)", got)
	}
}

func TestClassifyResumeFailure_TurnCapExit_ResetsExistingCount(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(`{"type":"result","subtype":"error_max_turns","terminal_reason":"max_turns",` +
		`"session_id":"sid-1114","errors":["Reached maximum number of turns (50)"],"is_error":true,"num_turns":51}`)
	_, _, _, err := interpretClaudeResult(context.Background(), 1114, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 2)

	var turnLimitErr *claudeTurnLimitError
	if !errors.As(err, &turnLimitErr) {
		t.Fatalf("expected *claudeTurnLimitError, got %T: %v", err, err)
	}
	var resumeErr *claudeResumeFailureError
	if errors.As(err, &resumeErr) {
		t.Errorf("turn-cap exit must not also be classified as *claudeResumeFailureError")
	}
	if got := readResumeFailureCount(sessPath); got != 0 {
		t.Errorf("sidecar count after turn-cap exit = %d, want 0 (reset — real turns/cost is evidence of a healthy session)", got)
	}
}

func TestClassifyResumeFailure_UsageLimitExit_LeavesCountUntouched(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(`{"result":"","session_id":"sid-1","terminal_reason":"blocking_limit","is_error":true,"num_turns":0,"total_cost_usd":0}`)
	_, _, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 2)

	var limitErr *claudeUsageLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected *claudeUsageLimitError, got %T: %v", err, err)
	}
	var resumeErr *claudeResumeFailureError
	if errors.As(err, &resumeErr) {
		t.Errorf("usage-limit exit must not also be classified as *claudeResumeFailureError")
	}
	if got := readResumeFailureCount(sessPath); got != 1 {
		t.Errorf("sidecar count after usage-limit exit = %d, want 1 (untouched — the stage never ran, says nothing about session health)", got)
	}
}

func TestClassifyResumeFailure_APIErrorExit_LeavesCountUntouched(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(`{"result":"","session_id":"sid-1","terminal_reason":"api_error","is_error":true,"num_turns":1,"total_cost_usd":0}`)
	_, _, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 2)

	var apiErr *claudeAPIErrorExit
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *claudeAPIErrorExit, got %T: %v", err, err)
	}
	var resumeErr *claudeResumeFailureError
	if errors.As(err, &resumeErr) {
		t.Errorf("api_error exit must not also be classified as *claudeResumeFailureError")
	}
	if got := readResumeFailureCount(sessPath); got != 1 {
		t.Errorf("sidecar count after api_error exit = %d, want 1 (untouched)", got)
	}
}

func TestClassifyResumeFailure_ColdStartFailure_NotWrappedAndResetsStaleCount(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	// A stale leftover count from a since-abandoned/pruned prior session
	// lineage — must be cleared by a cold start's own failure.
	writeResumeFailureCount(1, sessPath, 1)

	raw := []byte(genericFailureRaw)
	_, _, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "" /* cold start */, 2)

	var resumeErr *claudeResumeFailureError
	if errors.As(err, &resumeErr) {
		t.Fatalf("a cold start's own failure must never be wrapped as *claudeResumeFailureError, got %v", err)
	}
	if err == nil {
		t.Fatalf("expected a plain error")
	}
	if got := readResumeFailureCount(sessPath); got != 0 {
		t.Errorf("sidecar count after cold-start failure = %d, want 0 (reset)", got)
	}
}

func TestClassifyResumeFailure_ThresholdDisabled_NeverWraps(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")

	raw := []byte(genericFailureRaw)
	_, _, _, err := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "resumed-sid", 0)

	var resumeErr *claudeResumeFailureError
	if errors.As(err, &resumeErr) {
		t.Fatalf("maxResumeFailures<=0 must disable the mechanism entirely, got %v", err)
	}
	if err == nil {
		t.Fatalf("expected a plain error")
	}
}

// TestClassifyResumeFailure_AlternatingPaths_StillReachesThreshold is the
// alternating-invocation-path requirement from the issue: the counter is
// keyed purely by sessFilePath, so a sequence of calls simulating the stage
// path failing, then the comment-review path failing (both resuming the same
// session, both against the identical sessFilePath), still reaches the
// threshold — neither path gets its own independent counter.
func TestClassifyResumeFailure_AlternatingPaths_StillReachesThreshold(t *testing.T) {
	sessPath := filepath.Join(t.TempDir(), "Implement.session")
	if err := os.WriteFile(sessPath, []byte("shared-sid"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	const threshold = 2
	raw := []byte(genericFailureRaw)

	// Call 1: "stage path" resume fails.
	_, _, _, err1 := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "shared-sid", threshold)
	var resumeErr1 *claudeResumeFailureError
	if !errors.As(err1, &resumeErr1) {
		t.Fatalf("call 1: expected *claudeResumeFailureError, got %T: %v", err1, err1)
	}
	if resumeErr1.ConsecutiveFailures != 1 || resumeErr1.Abandoned {
		t.Fatalf("call 1: ConsecutiveFailures=%d Abandoned=%v, want 1/false", resumeErr1.ConsecutiveFailures, resumeErr1.Abandoned)
	}
	if _, statErr := os.Stat(sessPath); statErr != nil {
		t.Fatalf("call 1: session file should still exist, stat err = %v", statErr)
	}

	// Call 2: "comment-review path" resume fails against the SAME session
	// file — this is what resolveResumeSessionID would have returned for
	// InvokeClaudeForComments too, since it reads the identical sessFilePath.
	_, _, _, err2 := interpretClaudeResult(context.Background(), 1, raw, errors.New("exit status 1"), false, sessPath, t.TempDir(), "shared-sid", threshold)
	var resumeErr2 *claudeResumeFailureError
	if !errors.As(err2, &resumeErr2) {
		t.Fatalf("call 2: expected *claudeResumeFailureError, got %T: %v", err2, err2)
	}
	if resumeErr2.ConsecutiveFailures != 2 || !resumeErr2.Abandoned {
		t.Fatalf("call 2: ConsecutiveFailures=%d Abandoned=%v, want 2/true", resumeErr2.ConsecutiveFailures, resumeErr2.Abandoned)
	}

	// The session must now be abandoned — the very next invocation on
	// EITHER path cold-starts, since resolveResumeSessionID treats an absent
	// file as a cold start identically for both callers.
	if _, statErr := os.Stat(sessPath); !os.IsNotExist(statErr) {
		t.Errorf("expected session to be abandoned after alternating-path threshold, stat err = %v", statErr)
	}
	if got := resolveResumeSessionID(1, "Implement", sessPath, true); got != "" {
		t.Errorf("resolveResumeSessionID after abandonment = %q, want \"\" (cold start)", got)
	}
}
