//go:build !windows

package pruefer

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// startSleeper starts a detached "sleep" process in its own process group
// and returns the *exec.Cmd (already started). Callers must ensure the
// process is reaped (it will be, once killed, since no one calls Wait —
// tests only assert liveness via signal 0, which does not require reaping
// for the ESRCH check to eventually succeed on most platforms; to keep
// this simple and avoid zombies, tests call cmd.Wait() in a goroutine).
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process-group signaling is unix-only")
	}
	cmd := exec.Command("sleep", "30")
	setCmdProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep process: %v", err)
	}
	go cmd.Wait() // reap to avoid a zombie once killed
	return cmd
}

func TestIsProcessAlive(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	if !isProcessAlive(pid) {
		t.Fatal("expected process to be alive immediately after start")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing process: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for isProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if isProcessAlive(pid) {
		t.Error("expected process to be dead after Kill")
	}
}

func TestKillProcGroup_SendsSIGKILL(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	killProcGroup(cmd, 42, "test")
	deadline := time.Now().Add(2 * time.Second)
	for isProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if isProcessAlive(pid) {
		t.Error("expected process to be dead after killProcGroup")
	}
}

func TestKillProcGroupGraceful_EscalatesToSIGKILL(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	// Zero grace windows collapse straight to SIGKILL — this exercises the
	// full escalation call path without a real multi-second test.
	killProcGroupGraceful(pid, 42, "test", "unit_test", 0, 0)
	deadline := time.Now().Add(2 * time.Second)
	for isProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if isProcessAlive(pid) {
		t.Error("expected process to be dead after killProcGroupGraceful")
	}
}

func TestKillProcGroupGraceful_SigIntSufficient(t *testing.T) {
	// A process that exits cleanly on SIGINT (the default disposition for a
	// plain "sleep") should be gone before the SIGTERM/SIGKILL steps ever run.
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	killProcGroupGraceful(pid, 42, "test", "unit_test", 50*time.Millisecond, 2*time.Second)
	if isProcessAlive(pid) {
		t.Error("expected process to be dead after SIGINT grace window (sleep exits on SIGINT)")
	}
}

func TestKillProcGroupGraceful_ZeroPID_NoOp(t *testing.T) {
	// Must not panic or signal PID 0 (which would hit the caller's own
	// process group) when passed a zero/negative PID.
	killProcGroupGraceful(0, 42, "test", "unit_test", 0, 0)
}

func TestKillProcGroup_NilProcess_NoOp(t *testing.T) {
	cmd := &exec.Cmd{}
	killProcGroup(cmd, 42, "test") // must not panic
}
