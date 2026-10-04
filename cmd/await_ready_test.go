package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// readyDeadline bounds every wait for Execute()'s engine to signal testReadyCh.
const readyDeadline = 30 * time.Second

// awaitReady waits for readyCh to close, for Execute() to return early on
// done, or for the timeout — whichever comes first — and returns nil only for
// the first. It never blocks unbounded: a setup failure (e.g. App-auth startup
// against a fake org, which fails before testReadyCh is closed) is reported
// with Execute()'s own error instead of hanging until the package timeout
// (#2027, R5).
func awaitReady(readyCh <-chan struct{}, done <-chan error, timeout time.Duration) error {
	select {
	case <-readyCh:
		return nil
	case err := <-done:
		if err == nil {
			return errors.New("Execute() returned before the engine signalled ready (nil error)")
		}
		return errors.New("Execute() returned before the engine signalled ready: " + err.Error())
	case <-time.After(timeout):
		return errors.New("engine did not signal ready within " + timeout.String())
	}
}

// mustAwaitReady fails the test with the Execute() error if the engine does not
// become ready.
func mustAwaitReady(t *testing.T, readyCh <-chan struct{}, done <-chan error) {
	t.Helper()
	if err := awaitReady(readyCh, done, readyDeadline); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitReady(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		ready := make(chan struct{})
		close(ready)
		if err := awaitReady(ready, make(chan error), time.Second); err != nil {
			t.Fatal(err)
		}
	})

	// The hang regression: setup fails, readyCh is never closed, Execute()
	// returns its error. awaitReady must surface it immediately rather than
	// waiting out the deadline.
	t.Run("setup failure returns the Execute error quickly", func(t *testing.T) {
		done := make(chan error, 1)
		done <- errors.New("github-app: installation not found")
		start := time.Now()
		err := awaitReady(make(chan struct{}), done, time.Minute)
		if err == nil || !strings.Contains(err.Error(), "installation not found") {
			t.Fatalf("expected the Execute() error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %v; must fail fast, not at the deadline", elapsed)
		}
	})

	t.Run("early nil return is an error", func(t *testing.T) {
		done := make(chan error, 1)
		done <- nil
		if err := awaitReady(make(chan struct{}), done, time.Minute); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		err := awaitReady(make(chan struct{}), make(chan error), 20*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "did not signal ready") {
			t.Fatalf("got %v", err)
		}
	})
}
