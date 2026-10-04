package engine

import (
	"testing"
	"time"
)

// awaitRunReady waits for Run()'s ReadyCh to close, failing the test with
// Run()'s own error if it returns first (a startup refusal) or after a
// deadline — so a setup failure fails fast instead of blocking on the channel
// until the package timeout (#2027, R5). Mirrors runEngineUntilShutdownWith.
func awaitRunReady(t *testing.T, readyCh <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-readyCh:
	case err := <-done:
		t.Fatalf("Run() exited before signaling ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run() did not become ready within 30s")
	}
}
