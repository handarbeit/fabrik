//go:build !windows

package engine

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// installStraySighupSink keeps a process-lifetime SIGHUP subscription open in the
// engine test binary.
//
// Several tests (sighup_test.go, sighup_drain_test.go, localapi_lifecycle_test.go)
// deliver a real SIGHUP to this process. A SIGHUP that arrives while no
// signal.Notify channel wants it gets Go's default action and kills the whole
// binary: every engine test still running is lost and the only output is
// "signal: hangup", with no test named (#2596 CI). With this sink registered the
// signal is always wanted, so a stray one is discarded here instead. signal.Notify
// delivers to every registered channel, so the engine's own SIGHUP handler still
// receives each signal and the tests still exercise real signals; a SIGHUP an
// engine missed now fails the test waiting for it, by name.
//
// SIGHUP only: SIGINT and SIGTERM keep their default action so a cancelled CI run
// or a ^C still stops the binary.
func installStraySighupSink() {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for range ch {
		}
	}()
}

// A SIGHUP with no engine handler registered must not kill the test binary.
// Without installStraySighupSink this test takes the whole package down.
func TestStraySighupDoesNotKillTestBinary(t *testing.T) {
	signalSelf(t, syscall.SIGHUP)
	time.Sleep(200 * time.Millisecond) // the default action, if any, is immediate
}
