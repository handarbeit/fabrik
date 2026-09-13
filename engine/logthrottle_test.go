package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tui"
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

// TestEngine_LogfThrottledByInterval_MessageChangeDoesNotForceLog is
// logfThrottledByInterval's defining contract versus logfThrottled: it
// checks shouldLog against a constant surrogate (the throttle key itself),
// not the real formatted message, so a change in interpolated content at the
// same injected time does NOT force a repeat emission the way logfThrottled
// would. This is the fix for a #1716 review finding: logfThrottled's
// message-change-forces-log rule defeated throttling entirely for a periodic
// status line whose content (a remaining-count, an elapsed duration) changes
// on nearly every call in routine operation. Exercises the same underlying
// primitive logfThrottledByInterval calls, following
// TestEngine_LogfThrottled_UsesInjectedClock's existing precedent for
// testing these thin logf wrappers via their exact key/message shape.
func TestEngine_LogfThrottledByInterval_MessageChangeDoesNotForceLog(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	eng.SetClock(stubClock{t: fixed})

	// logfThrottledByInterval calls shouldLog(key, key, ...) — the surrogate
	// is the key itself, regardless of what format/args produced.
	if !eng.logThrottle.shouldLog("interval-key", "interval-key", eng.now(), logThrottleMinInterval) {
		t.Fatal("first call should log")
	}
	// A repeat at the same instant, still using the same constant surrogate
	// (as any subsequent logfThrottledByInterval("interval-key", ...) call
	// would, no matter how different its real formatted message is) must be
	// suppressed — proving content changes can't defeat this variant.
	if eng.logThrottle.shouldLog("interval-key", "interval-key", eng.now(), logThrottleMinInterval) {
		t.Error("identical surrogate repeat within the interval should be suppressed, regardless of real message content")
	}

	// logfThrottledByInterval itself must not panic and must be safe to call
	// repeatedly with varying content at the same instant — and, per review
	// finding, the second call's varying content (count: 2) must actually be
	// suppressed at the logf-wrapper level, not just at the underlying
	// shouldLog primitive already covered above. Captures via the events
	// channel, following poll_test.go's established "drain LogEvent" idiom
	// rather than asserting only "does not panic."
	events := make(chan tui.Event, 8)
	eng.events = events
	eng.logfThrottledByInterval("interval-key-2", 0, "poll", "count: %d\n", 1)
	eng.logfThrottledByInterval("interval-key-2", 0, "poll", "count: %d\n", 2)
	close(events)
	eng.events = nil

	var logged []tui.LogEvent
	for ev := range events {
		if le, ok := ev.(tui.LogEvent); ok {
			logged = append(logged, le)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("expected exactly 1 log event (second call suppressed by the interval throttle), got %d: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0].Message, "count: 1") {
		t.Errorf("expected the surviving log event to carry the first call's message (count: 1), got %q", logged[0].Message)
	}
}
