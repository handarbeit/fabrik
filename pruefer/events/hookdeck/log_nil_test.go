package hookdeck

import "testing"

// TestSetLogfNilDoesNotPanic is the regression test for SetLogf(nil) storing
// &fn — always a non-nil *T even when fn is nil — which made logf's `p != nil`
// check pass and then invoke a nil func value. The panic surfaced on the next
// log line, not at the SetLogf call, from a function whose doc says it is safe
// to call at any time.
func TestSetLogfNilDoesNotPanic(t *testing.T) {
	t.Cleanup(func() { logfHook.Store(nil) })

	SetLogf(func(string, ...any) {})
	SetLogf(nil)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("logf panicked after SetLogf(nil): %v", r)
		}
	}()
	logf("probe %d\n", 1)
}

// TestSetLogfNilRestoresStderrFallback: clearing the hook must fall back to
// stderr rather than silently dropping output, which is what the doc comment
// promises and what an operator debugging a live daemon depends on.
func TestSetLogfNilRestoresStderrFallback(t *testing.T) {
	t.Cleanup(func() { logfHook.Store(nil) })

	called := 0
	SetLogf(func(string, ...any) { called++ })
	logf("first\n")
	if called != 1 {
		t.Fatalf("installed hook received %d calls, want 1", called)
	}

	SetLogf(nil)
	logf("second\n")
	if called != 1 {
		t.Fatalf("hook received %d calls after SetLogf(nil), want it to stay at 1", called)
	}
	if p := logfHook.Load(); p != nil {
		t.Fatal("SetLogf(nil) left a non-nil hook pointer, so logf would call through it")
	}
}

// TestSetLogfReinstallAfterNil guards the round trip: clearing then reinstalling
// must route to the new hook.
func TestSetLogfReinstallAfterNil(t *testing.T) {
	t.Cleanup(func() { logfHook.Store(nil) })

	SetLogf(nil)
	got := 0
	SetLogf(func(string, ...any) { got++ })
	logf("third\n")
	if got != 1 {
		t.Fatalf("reinstalled hook received %d calls, want 1", got)
	}
}
