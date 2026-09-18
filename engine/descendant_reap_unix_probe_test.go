//go:build !windows

package engine

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestPidFingerprint_FindsSelf is a smoke test confirming the ps invocation
// pidFingerprint relies on parses correctly on this platform.
func TestPidFingerprint_FindsSelf(t *testing.T) {
	comm, lstart, err := pidFingerprint(os.Getpid())
	if err != nil {
		t.Fatalf("pidFingerprint: %v", err)
	}
	if comm == "" || lstart == "" {
		t.Errorf("expected non-empty comm/lstart, got comm=%q lstart=%q", comm, lstart)
	}
}

// TestSessionScopedDescendants_FindsRealChild spawns a real child process via
// exec.Command (no explicit Setsid on the child, so it inherits the test
// binary's own session) and confirms sessionScopedDescendants(ourSID) finds
// it — a basic correctness check of the unix.Getsid-based mechanism before
// relying on it in the heavier detachment-style tests.
func TestSessionScopedDescendants_FindsRealChild(t *testing.T) {
	// Use our own session ID as the "worker" PID: the test binary itself may
	// not be a session leader, so spawn a session-leading shell first and
	// treat its PID as the worker whose session the grandchild inherits.
	shell := exec.Command("/bin/sh", "-c", "sleep 5 & wait")
	setCmdProcAttr(shell) // Setsid — makes this the session leader, our "worker"
	if err := shell.Start(); err != nil {
		t.Fatalf("starting shell: %v", err)
	}
	defer func() {
		_ = shell.Process.Kill()
		_ = shell.Wait()
	}()
	workerPID := shell.Process.Pid

	var pids []int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := sessionScopedDescendants(workerPID)
		if err != nil {
			t.Fatalf("sessionScopedDescendants: %v", err)
		}
		if len(got) > 0 {
			pids = got
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pids) == 0 {
		t.Fatal("sessionScopedDescendants found no descendants of the session-leader shell")
	}
}
