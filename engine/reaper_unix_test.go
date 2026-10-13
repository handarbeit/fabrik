//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spawnSetsidProcess starts a long-lived process detached into its own
// session (and therefore its own process group — the classic escape path for
// PGID-scoped killProcGroup), with its cwd set to dir. It returns the started
// *exec.Cmd; the caller is responsible for reaping/cleanup.
func spawnSetsidProcess(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not found in PATH")
	}
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting setsid process: %v", err)
	}
	// Reap the child as soon as it exits, exactly as init (PID 1) would for a
	// real orphaned descendant. Without this, a killed child lingers as a
	// zombie until Wait()'d, and kill(pid, 0) reports zombies as "alive" —
	// which would make the liveness check below meaningless.
	go func() { _ = cmd.Wait() }()
	return cmd
}

// waitForPGID polls until syscall.Getpgid(pid) succeeds, returning the PGID.
// A freshly setsid'd process needs a moment before its PGID is queryable.
func waitForPGID(t *testing.T, pid int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pgid, err := syscall.Getpgid(pid)
		if err == nil {
			return pgid
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not read pgid for pid %d: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// useRealLsofReaper re-enables the real macOS `lsof` enumeration for one test
// (TestMain disables it process-wide, #1805) and restores the prior value on
// cleanup. A no-op in effect on Linux, where the seam is not consulted.
func useRealLsofReaper(t *testing.T) {
	t.Helper()
	prev := worktreeReapLsofDisabled.Load()
	SetWorktreeReapLsofDisabledForTest(false)
	t.Cleanup(func() { SetWorktreeReapLsofDisabledForTest(prev) })
}

// stubLsof writes an executable `lsof` shell script with the given body into a
// fresh directory and puts that directory first on PATH. Darwin only: the
// Linux reaper never calls lsof.
func stubLsof(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("the lsof path is darwin-only")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing lsof stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testLogf(logs *[]string) func(int, string, string, ...any) {
	return func(issueNumber int, tag, format string, args ...any) {
		*logs = append(*logs, fmt.Sprintf("[#%d %s] "+format, append([]any{issueNumber, tag}, args...)...))
	}
}

// TestReapWorktreeProcesses_KillsSetsidDescendant exercises the actual bug
// this reaper fixes: a descendant that called setsid() lands in its own
// process group and is invisible to killProcGroup's PGID-scoped kill. A test
// that only spawned an ordinary child would pass under the pre-fix code too
// and would prove nothing about this change.
func TestReapWorktreeProcesses_KillsSetsidDescendant(t *testing.T) {
	useRealLsofReaper(t)
	wtDir := t.TempDir()
	cmd := spawnSetsidProcess(t, wtDir)
	pid := cmd.Process.Pid
	// The spawning goroutine's Wait() reaps the process; calling Wait() again
	// here would race with it, so cleanup only needs to ensure it's dead.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})

	childPGID := waitForPGID(t, pid)
	callerPGID, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid(self): %v", err)
	}
	if childPGID == callerPGID {
		t.Fatalf("expected setsid child to have escaped the caller's PGID, both are %d", callerPGID)
	}
	if childPGID != pid {
		t.Fatalf("expected setsid child's PGID to equal its own PID, got pgid=%d pid=%d", childPGID, pid)
	}

	var logs []string
	reapWorktreeProcesses(wtDir, 42, testLogf(&logs))

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return // reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("setsid descendant (pid %d) still alive after reapWorktreeProcesses; logs: %v", pid, logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReapWorktreeProcesses_LeavesProcessesOutsideWorktree proves precision,
// not just breadth: a process whose cwd is outside the reaped worktree must
// survive even though it is (deliberately) also setsid'd, so a coincidental
// PGID match could never explain a false-positive kill.
func TestReapWorktreeProcesses_LeavesProcessesOutsideWorktree(t *testing.T) {
	useRealLsofReaper(t)
	wtDir := t.TempDir()
	outsideDir := t.TempDir()
	cmd := spawnSetsidProcess(t, outsideDir)
	pid := cmd.Process.Pid
	// The spawning goroutine's Wait() reaps the process; calling Wait() again
	// here would race with it, so cleanup only needs to ensure it's dead.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
	})
	waitForPGID(t, pid)

	var logs []string
	reapWorktreeProcesses(wtDir, 42, testLogf(&logs))

	// Give the (non-)reap a moment to have taken effect, then assert the
	// process is still alive.
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("process outside the reaped worktree should have survived, got: %v; logs: %v", err, logs)
	}
}

// TestReapWorktreeProcesses_NonFatalOnEnumerationFailure confirms the
// non-fatal contract: enumeration failures (lsof missing on darwin, a
// nonexistent worktree directory) must never panic or block — the caller's
// subsequent worktree removal always proceeds regardless of reap outcome.
func TestReapWorktreeProcesses_NonFatalOnEnumerationFailure(t *testing.T) {
	useRealLsofReaper(t)
	t.Setenv("PATH", "") // hides `lsof` on darwin
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	var logs []string
	reapWorktreeProcesses(nonexistent, 7, testLogf(&logs))
	// Reaching here without panicking/hanging is the assertion.
}

// TestReapWorktreeProcesses_LsofTimeoutSkipsReap (#1805 R1): an lsof that never
// exits must not block the reaper past worktreeReapLsofTimeout. The reap is
// skipped with exactly one warning naming the worktree and the timeout, the
// stub is killed, and a process rooted under the worktree survives (the reap
// really was skipped, not partially applied).
func TestReapWorktreeProcesses_LsofTimeoutSkipsReap(t *testing.T) {
	useRealLsofReaper(t)
	pidFile := filepath.Join(t.TempDir(), "lsof.pid")
	// exec so the recorded PID is the sleeper itself, i.e. the process the
	// context kill must reach.
	stubLsof(t, fmt.Sprintf("echo $$ > %q\nexec sleep 3600", pidFile))

	prev := worktreeReapLsofTimeout
	worktreeReapLsofTimeout = 300 * time.Millisecond
	t.Cleanup(func() { worktreeReapLsofTimeout = prev })

	wtDir := t.TempDir()
	cmd := spawnSetsidProcess(t, wtDir)
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitForPGID(t, pid)

	var logs []string
	start := time.Now()
	reapWorktreeProcesses(wtDir, 9, testLogf(&logs))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("reaper blocked for %v despite a %v timeout", elapsed, worktreeReapLsofTimeout)
	}

	var timeouts int
	for _, l := range logs {
		if strings.Contains(l, "lsof timed out after 300ms") && strings.Contains(l, "skipping reap") {
			timeouts++
			resolved, _ := filepath.EvalSymlinks(wtDir)
			if !strings.Contains(l, resolved) {
				t.Errorf("timeout warning does not name the worktree %q: %s", resolved, l)
			}
		}
		if strings.Contains(l, "lsof failed") {
			t.Errorf("timeout must not be reported as a generic lsof failure: %s", l)
		}
	}
	if timeouts != 1 || len(logs) != 1 {
		t.Fatalf("want exactly one timeout warning, got %d (all logs: %v)", timeouts, logs)
	}

	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("process under the worktree was killed although the reap was skipped: %v", err)
	}

	// The stub lsof must be gone, not left sleeping for an hour.
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading stub pid: %v", err)
	}
	var stubPID int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &stubPID)
	if stubPID <= 0 {
		t.Fatalf("bad stub pid %q", data)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(stubPID, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			_ = syscall.Kill(stubPID, syscall.SIGKILL)
			t.Fatalf("stub lsof (pid %d) still alive after timeout", stubPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReapWorktreeProcesses_LsofFailureStaysDistinctFromTimeout: a fast
// non-zero exit with no output keeps the pre-existing "lsof failed" warning.
func TestReapWorktreeProcesses_LsofFailureStaysDistinctFromTimeout(t *testing.T) {
	useRealLsofReaper(t)
	stubLsof(t, "exit 1")

	var logs []string
	reapWorktreeProcesses(t.TempDir(), 3, testLogf(&logs))
	if len(logs) != 1 || !strings.Contains(logs[0], "lsof failed") || strings.Contains(logs[0], "timed out") {
		t.Fatalf("want a single 'lsof failed' warning, got %v", logs)
	}
}

// TestReapWorktreeProcesses_SeamDisabledSkipsLsof: with the seam on, lsof is
// never spawned.
func TestReapWorktreeProcesses_SeamDisabledSkipsLsof(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "lsof-ran")
	stubLsof(t, fmt.Sprintf("touch %q", sentinel))
	prev := worktreeReapLsofDisabled.Load()
	SetWorktreeReapLsofDisabledForTest(true)
	t.Cleanup(func() { SetWorktreeReapLsofDisabledForTest(prev) })

	var logs []string
	reapWorktreeProcesses(t.TempDir(), 3, testLogf(&logs))
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("lsof was invoked although the seam disables it")
	}
	if len(logs) != 0 {
		t.Fatalf("unexpected logs: %v", logs)
	}
}
