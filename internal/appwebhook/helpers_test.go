package appwebhook

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/warnings"
)

// newReceiverForTest mirrors the old constructor signature the unit tests use.
func newReceiverForTest(name, path string, listen func() (net.Listener, error), secret string, logf LogFunc, health HealthSink) *Receiver {
	return New(Config{Name: name, Path: path, Listen: listen, Secret: secret, Logf: logf, Health: health})
}

func isolateWarnings(t *testing.T) {
	t.Helper()
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
}

func replayWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
