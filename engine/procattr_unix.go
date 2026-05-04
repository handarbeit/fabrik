//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setCmdProcAttr starts cmd in its own process group so grandchild processes
// (e.g. tail -f from the Monitor tool) can be cleaned up after cmd exits.
func setCmdProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup sends SIGKILL to cmd's entire process group, cleaning up any
// grandchild processes that outlived the Claude process. ESRCH (no such process)
// is silently ignored — the group may already be gone. Unexpected errors are
// logged to stderr so cleanup failures are diagnosable.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Negative PID targets the process group (PGID == Claude's PID when Setpgid is set).
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[engine] killProcGroup: unexpected error killing process group %d: %v\n", cmd.Process.Pid, err)
	}
}

// isProcessAlive returns true if the process with the given PID is alive.
// Uses signal 0 (does not kill the process; only probes existence/permissions).
func isProcessAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// killProcGroupGraceful sends SIGTERM to the process group, waits 10 seconds for
// a graceful shutdown, then sends SIGKILL. This reaps hung background children
// (dangling pytest, tail -f, polling loops) in addition to the Claude CLI itself.
// Returns immediately if the process group is already gone (ESRCH) — avoids the
// PID-reuse hazard where sleeping 10s then SIGKILLing could hit an unrelated group
// that recycled the same PGID.
func killProcGroupGraceful(pid, issueNumber int, label string) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return // group already gone; nothing to clean up
		}
		fmt.Fprintf(os.Stderr, "[#%d engine] killProcGroupGraceful %q: SIGTERM error on pgid %d: %v\n", issueNumber, label, pid, err)
	}
	time.Sleep(10 * time.Second)
	// Re-probe before SIGKILL: if the group exited during the grace period, return
	// rather than risk hitting a recycled PGID.
	if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[#%d engine] killProcGroupGraceful %q: SIGKILL error on pgid %d: %v\n", issueNumber, label, pid, err)
	}
}
