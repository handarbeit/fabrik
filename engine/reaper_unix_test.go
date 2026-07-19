//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	t.Setenv("PATH", "") // hides `lsof` on darwin
	nonexistent := filepath.Join(t.TempDir(), "does-not-exist")

	var logs []string
	reapWorktreeProcesses(nonexistent, 7, testLogf(&logs))
	// Reaching here without panicking/hanging is the assertion.
}
