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
	// The session reap treats a dead-but-unreaped process as gone, so it can
	// return a moment before startSleeper's reaper goroutine collects the exit
	// status; poll rather than probe once.
	deadline := time.Now().Add(time.Second)
	for isProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
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

// startSessionWorker starts a Setsid worker whose child setpgrp()s itself
// (via perl; dash has no usable `set -m`) into a separate process group of the same session — the
// shape of a Claude Bash-tool command that kill(-pid, …) cannot reach (#1989).
func startSessionWorker(t *testing.T) (worker *exec.Cmd, childPID int) {
	t.Helper()
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.Command("sh", "-c", "perl -e '$SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); setpgrp(0,0); exec @ARGV' sleep 60 &\necho $! > "+pidFile+"\nwait\n")
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

// startBashToolWorker models the real Claude CLI shape (#1989 validation
// finding): the worker leads a session, and its Bash-tool command shell calls
// setsid(), so the command and the test tree below it live in a session of
// their own that the worker's SID does not reach.
//
//	worker W (sid W) ── command shell C (sid C) ── child G (sid C, own pgid)
func startBashToolWorker(t *testing.T) (worker *exec.Cmd, shell, child int) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	childSh := write("child.sh", "trap 'echo x >> "+dir+"/sigs' INT TERM\necho $$ > "+dir+"/child.pid\nwhile :; do sleep 0.05; done\n")
	cmdSh := write("cmd.sh", "perl -e '$SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); setpgrp(0,0); exec @ARGV' sh "+childSh+" >/dev/null 2>&1 &\necho $$ > "+dir+"/shell.pid\nwait\n")
	workerSh := write("worker.sh", "perl -MPOSIX -e 'POSIX::setsid(); $SIG{INT}=q(DEFAULT); $SIG{QUIT}=q(DEFAULT); exec @ARGV' sh "+cmdSh+" >/dev/null 2>&1 &\nsleep 60\n")
	worker = exec.Command("sh", workerSh)
	setCmdProcAttr(worker)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	go worker.Wait()
	readPID := func(name string) int {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(dir + "/" + name); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
					return pid
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s never written", name)
		return 0
	}
	shell, child = readPID("shell.pid"), readPID("child.pid")
	t.Cleanup(func() {
		for _, p := range []int{child, shell} {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
	})
	if sid, _ := unix.Getsid(shell); sid != shell || shell == worker.Process.Pid {
		t.Fatalf("fixture: command shell sid=%d pid=%d worker=%d; it must lead its own session", sid, shell, worker.Process.Pid)
	}
	if sid, _ := unix.Getsid(child); sid != shell {
		t.Fatalf("fixture: child sid=%d, want the command shell's session %d", sid, shell)
	}
	return worker, shell, child
}

func TestKillProcGroupGraceful_ReapsBashToolCommandSession(t *testing.T) {
	worker, shell, child := startBashToolWorker(t)
	stop := trackReviewSessions(t.Context(), worker.Process.Pid, 1)
	defer stop()
	killProcGroupGraceful(worker.Process.Pid, 1, "t", "max_wall_time", 300*time.Millisecond, 300*time.Millisecond)
	if !waitDead(child, 3*time.Second) || !waitDead(shell, 3*time.Second) {
		t.Fatalf("Bash-tool command session survived the stop (child alive=%v shell alive=%v)", isProcessAlive(child), isProcessAlive(shell))
	}
}

func TestReapReviewSession_PostExitSweepReapsSampledCommandSession(t *testing.T) {
	worker, shell, child := startBashToolWorker(t)
	prev := reviewSessionSampleInterval
	reviewSessionSampleInterval = 20 * time.Millisecond
	defer func() { reviewSessionSampleInterval = prev }()
	stop := trackReviewSessions(t.Context(), worker.Process.Pid, 1)
	defer stop()
	time.Sleep(300 * time.Millisecond) // let the sampler see the command session
	if err := syscall.Kill(worker.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitDead(worker.Process.Pid, 3*time.Second) {
		t.Fatal("worker did not die")
	}
	if !isProcessAlive(child) || !isProcessAlive(shell) {
		t.Fatal("fixture: command session died with the worker")
	}
	if n := reapReviewSession(worker.Process.Pid, 1, "clean_exit"); n < 2 {
		t.Errorf("reapReviewSession = %d, want >= 2 (shell and child)", n)
	}
	if !waitDead(child, 3*time.Second) || !waitDead(shell, 3*time.Second) {
		t.Fatalf("command session survived the post-exit sweep (child alive=%v shell alive=%v)", isProcessAlive(child), isProcessAlive(shell))
	}
}
func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !isProcessAlive(pid)
}

// The normal post-exit state (#1957 R1): the worker leader has exited and been
// reaped, a grandchild in its group lives on, and killProcGroup must still
// clean it up.
func TestKillProcGroup_PostExitGrandchildIsKilled(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
	setCmdProcAttr(cmd)
	out, err := cmd.Output() // Wait() reaps the leader
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("grandchild pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	if isProcessAlive(cmd.Process.Pid) {
		t.Fatal("fixture: leader still alive after Wait")
	}
	killProcGroup(cmd, 42, "test")
	if !waitGone(grandchild, 3*time.Second) {
		t.Fatal("post-exit grandchild survived killProcGroup")
	}
}

// A stored PID whose live holder leads its own group but is not ours — the shape of a
// recycled PID — must not be group-signalled (#1957 R1).
func TestKillProcGroup_StaleStoredPIDIsSkipped(t *testing.T) {
	bystander := startForeignGroupLeader(t)

	// A cmd whose recorded PID is now held by an unrelated live process.
	stale := &exec.Cmd{Process: &os.Process{Pid: bystander}}
	killProcGroup(stale, 42, "test")
	time.Sleep(200 * time.Millisecond)
	if !isProcessAlive(bystander) {
		t.Fatal("killProcGroup signalled a stored PID it does not own")
	}
}

func TestKillProcGroup_CatastrophicPIDsRefused(t *testing.T) {
	for _, pid := range []int{-1, 0, 1, syscall.Getpgrp()} {
		// Must return without signalling anything (the test process surviving
		// is the assertion for the own-group case).
		killProcGroup(&exec.Cmd{Process: &os.Process{Pid: pid}}, 42, "test")
	}
}

// startForeignGroupLeader returns the PID of a live process that leads its own
// process group but is neither our child nor recorded anywhere — the shape of a
// recycled stored PID. `set -m` gives the background job its own group, and the
// starter shell is reaped by Output(), so the bystander is reparented. Without
// the ownership check, kill(-pid) would hit it.
func startForeignGroupLeader(t *testing.T) int {
	t.Helper()
	starter := exec.Command("sh", "-c", "set -m; sleep 60 >/dev/null 2>&1 & echo $!")
	setCmdProcAttr(starter)
	out, err := starter.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("bystander pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Skipf("fixture: bystander %d does not lead its own group (pgid=%d, err=%v)", pid, pgid, err)
	}
	return pid
}
