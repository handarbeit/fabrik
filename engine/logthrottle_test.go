package engine

import (
	"testing"
	"time"
)

func TestLogThrottleState_ShouldLog_FirstOccurrenceAlwaysLogs(t *testing.T) {
	var s logThrottleState
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.shouldLog("k", "hello", now, time.Minute) {
		t.Error("first occurrence under a key should always log")
	}
}

func TestLogThrottleState_ShouldLog_IdenticalRepeatWithinIntervalSuppressed(t *testing.T) {
	var s logThrottleState
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.shouldLog("k", "hello", now, time.Minute) {
		t.Fatal("first call should log")
	}
	if s.shouldLog("k", "hello", now.Add(10*time.Second), time.Minute) {
		t.Error("identical repeat within the interval should be suppressed")
	}
}

func TestLogThrottleState_ShouldLog_ChangedMessageForcesLog(t *testing.T) {
	var s logThrottleState
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.shouldLog("k", "hello", now, time.Minute) {
		t.Fatal("first call should log")
	}
	if !s.shouldLog("k", "goodbye", now.Add(time.Second), time.Minute) {
		t.Error("a changed message must log even within the interval — a real state change must never be suppressed")
	}
}

func TestLogThrottleState_ShouldLog_ElapsedIntervalForcesLogEvenUnchanged(t *testing.T) {
	var s logThrottleState
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.shouldLog("k", "hello", now, time.Minute) {
		t.Fatal("first call should log")
	}
	if !s.shouldLog("k", "hello", now.Add(time.Minute), time.Minute) {
		t.Error("an unchanged message must still log once minInterval has elapsed")
	}
}

func TestLogThrottleState_ShouldLog_DistinctKeysIndependent(t *testing.T) {
	var s logThrottleState
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if !s.shouldLog("k1", "hello", now, time.Minute) {
		t.Fatal("first call under k1 should log")
	}
	if !s.shouldLog("k2", "hello", now, time.Minute) {
		t.Error("a different key with the identical message must log independently of k1's state")
	}
}

// TestEngine_LogfThrottled_UsesInjectedClock confirms logfThrottled reads
// e.now() (not time.Now()) so throttling stays controllable from a fixed
// clock in tests — a repeat classified only by wall-clock time would make
// this untestable deterministically.
func TestEngine_LogfThrottled_UsesInjectedClock(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	eng.SetClock(stubClock{t: fixed})

	if !eng.logThrottle.shouldLog("test-key", "same message", eng.now(), logThrottleMinInterval) {
		t.Fatal("first call should log")
	}
	// A second call at the identical injected time is a suppressible repeat.
	if eng.logThrottle.shouldLog("test-key", "same message", eng.now(), logThrottleMinInterval) {
		t.Error("identical repeat at the same injected time should be suppressed")
	}
}
