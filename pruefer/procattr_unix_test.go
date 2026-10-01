//go:build !windows

package pruefer

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// startSessionWorker starts a Setsid worker whose child, under job control
// (`set -m`), sits in a separate process group of the same session — the
// shape of a Claude Bash-tool command that kill(-pid, …) cannot reach (#1989).
func startSessionWorker(t *testing.T) (worker *exec.Cmd, childPID int) {
	t.Helper()
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.Command("sh", "-c", "set -m\nsleep 60 &\necho $! > "+pidFile+"\nwait\n")
	setCmdProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap the leader; avoids a zombie session member
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child pid never written")
	}
	t.Cleanup(func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	sid, _ := unix.Getsid(childPID)
	pgid, _ := unix.Getpgid(childPID)
	if sid != cmd.Process.Pid || pgid == cmd.Process.Pid {
		t.Fatalf("fixture: child sid=%d pgid=%d worker=%d; want same session, different group", sid, pgid, cmd.Process.Pid)
	}
	return cmd, childPID
}

func waitDead(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !isProcessAlive(pid)
}

func TestSetCmdProcAttr_MakesSessionLeader(t *testing.T) {
	cmd := startSleeper(t)
	sid, err := unix.Getsid(cmd.Process.Pid)
	if err != nil || sid != cmd.Process.Pid {
		t.Errorf("Getsid = %d, %v; want the worker's own PID %d", sid, err, cmd.Process.Pid)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func TestKillProcGroupGraceful_ReapsSameSessionChild(t *testing.T) {
	worker, child := startSessionWorker(t)
	killProcGroupGraceful(worker.Process.Pid, 1, "t", "max_wall_time", 200*time.Millisecond, 200*time.Millisecond)
	if !waitDead(child, 3*time.Second) {
		t.Fatalf("same-session child %d survived the session-wide escalation", child)
	}
}

func TestReapReviewSession_PostExitSweepKillsSameSessionChild(t *testing.T) {
	worker, child := startSessionWorker(t)
	// Post-exit state: the leader is gone, its separate-group child remains.
	if err := syscall.Kill(worker.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitDead(worker.Process.Pid, 3*time.Second) {
		t.Fatal("leader did not die")
	}
	if !isProcessAlive(child) {
		t.Fatal("fixture: child died with the leader")
	}
	if n := reapReviewSession(worker.Process.Pid, 1, "clean_exit"); n < 1 {
		t.Errorf("reapReviewSession = %d, want >= 1", n)
	}
	if !waitDead(child, 3*time.Second) {
		t.Fatal("child survived the post-exit sweep")
	}
}

func TestReapReviewSession_RefusesUnsafeSIDs(t *testing.T) {
	own, _ := unix.Getsid(0)
	for _, sid := range []int{0, 1, own} {
		if n := reapReviewSession(sid, 1, "clean_exit"); n != 0 {
			t.Errorf("reapReviewSession(%d) = %d, want 0", sid, n)
		}
	}
}
