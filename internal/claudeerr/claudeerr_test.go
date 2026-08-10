package claudeerr

import (
	"errors"
	"testing"
)

// TestUsageLimitError_ErrorFormat pins Error()'s exact string shape. engine's
// own tests assert behavior through errors.As and field values, never through
// this string, but a change here would still be a silent behavior change for
// any consumer (log lines, comment text) that formats the error — see the
// pure-move guard-rail in the issue's spec review comment.
func TestUsageLimitError_ErrorFormat(t *testing.T) {
	withReset := &UsageLimitError{Message: "terminal_reason=\"blocking_limit\"", ResetTime: "2026-01-01T00:00:00Z"}
	if got, want := withReset.Error(), `claude usage limit hit: terminal_reason="blocking_limit" (resets 2026-01-01T00:00:00Z)`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	noReset := &UsageLimitError{Message: "terminal_reason=\"blocking_limit\""}
	if got, want := noReset.Error(), `claude usage limit hit: terminal_reason="blocking_limit"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestTurnLimitError_ErrorFormat(t *testing.T) {
	e := &TurnLimitError{TerminalReason: "error_max_turns", NumTurns: 42}
	if got, want := e.Error(), "claude exited: turn limit reached (num_turns=42)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAPIErrorExit_ErrorFormat(t *testing.T) {
	e := &APIErrorExit{TerminalReason: "api_error", NumTurns: 1, CostUSD: 0}
	if got, want := e.Error(), `claude exited: transient api_error (terminal_reason="api_error", num_turns=1, cost=$0.0000)`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestResumeFailureError_ErrorFormatAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := &ResumeFailureError{
		Cause:               cause,
		SessionID:           "sess-123",
		ConsecutiveFailures: 2,
		Threshold:           2,
		Abandoned:           true,
	}
	if got, want := e.Error(), "claude exited with error while resuming session sess-123 (consecutive failures=2/2, abandoned=true): boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(e, cause) {
		t.Error("Unwrap() should expose Cause via errors.Is")
	}
}

// TestErrorsAs_MatchesConcreteTypes confirms these types are ordinary
// errors.As-matchable values — the property engine/item.go's five call sites
// rely on, and that a type alias (rather than a rename) preserves for the
// existing unexported-name assertions in engine's own test files.
func TestErrorsAs_MatchesConcreteTypes(t *testing.T) {
	var err error = &UsageLimitError{Message: "x"}
	var target *UsageLimitError
	if !errors.As(err, &target) {
		t.Error("errors.As should match *UsageLimitError")
	}
}
