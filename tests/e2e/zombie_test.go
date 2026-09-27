//go:build e2e

package e2e

import (
	"os/exec"
	"testing"
	"time"
)

// TestSyscallSignalZero_TreatsZombieAsDead: an exited, unreaped child answers
// signal 0 but is not running. lockedPID relied on signal 0 alone, so a bed
// that had exited looked alive for as long as the suite binary (its parent)
// ran (0.0.83 gate run 3).
func TestSyscallSignalZero_TreatsZombieAsDead(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })

	deadline := time.Now().Add(5 * time.Second)
	for !processIsZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d never became a zombie", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscallSignalZero(pid); err == nil {
		t.Fatalf("syscallSignalZero(%d) = nil for a zombie, want an error", pid)
	}

	// A live process still reads as alive.
	live := exec.Command("sleep", "5")
	if err := live.Start(); err != nil {
		t.Fatalf("start live child: %v", err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })
	if err := syscallSignalZero(live.Process.Pid); err != nil {
		t.Errorf("syscallSignalZero(live %d) = %v, want nil", live.Process.Pid, err)
	}
}
