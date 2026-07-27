//go:build !windows

package pruefer

// Duplicated from engine/procattr_unix.go rather than extracted into a
// shared internal package: these are small, stable OS primitives, and
// extracting them would touch engine's existing call sites for a
// security-relevant piece of code for the sake of a one-time ~80-line copy
// (see adrs/073-pruefer-v1-architecture.md).

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setCmdProcAttr starts cmd in its own process group so grandchild processes
// can be cleaned up after cmd exits.
func setCmdProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup sends SIGKILL to cmd's entire process group, cleaning up any
// grandchild processes that outlived the claude process. ESRCH (no such
// process) is silently ignored — the group may already be gone. Unexpected
// errors are logged so cleanup failures are diagnosable.
func killProcGroup(cmd *exec.Cmd, prNumber int, label string) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	logf(prNumber, "kill", "sending SIGKILL to PGID %d (grandchild cleanup)\n", pid)
	// Negative PID targets the process group (PGID == claude's PID when Setpgid is set).
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[pr#%d pruefer] killProcGroup %q: unexpected error killing process group %d: %v\n", prNumber, label, pid, err)
	}
}

// isProcessAlive returns true if the process with the given PID is alive.
// Uses signal 0 (does not kill the process; only probes existence/permissions).
// EPERM means the process exists but we lack permission — treat as alive.
// ESRCH means no such process — treat as dead.
func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// killProcGroupGraceful sends signals in escalating order to the process
// group: SIGINT → (sigintGrace) → SIGTERM → (sigtermGrace) → SIGKILL. A zero
// sigintGrace skips the SIGINT step entirely; a zero sigtermGrace skips the
// SIGTERM step (falls straight to SIGKILL). Liveness is re-probed before
// each subsequent signal; ESRCH stops escalation early.
func killProcGroupGraceful(pid, prNumber int, label, reason string, sigintGrace, sigtermGrace time.Duration) {
	if pid <= 0 {
		return
	}
	if sigintGrace > 0 {
		logf(prNumber, "kill", "sending SIGINT to PGID %d (reason=%s)\n", pid, reason)
		if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
			if err == syscall.ESRCH {
				return // group already gone
			}
			fmt.Fprintf(os.Stderr, "[pr#%d pruefer] killProcGroupGraceful %q: SIGINT error on pgid %d: %v\n", prNumber, label, pid, err)
		}
		time.Sleep(sigintGrace)
		if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
			return // group exited during SIGINT grace window
		}
	}

	if sigtermGrace > 0 {
		logf(prNumber, "kill", "sending SIGTERM to PGID %d (reason=%s)\n", pid, reason)
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			if err == syscall.ESRCH {
				return
			}
			fmt.Fprintf(os.Stderr, "[pr#%d pruefer] killProcGroupGraceful %q: SIGTERM error on pgid %d: %v\n", prNumber, label, pid, err)
		}
		time.Sleep(sigtermGrace)
		if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
			return // group exited during SIGTERM grace window
		}
	}

	logf(prNumber, "kill", "sending SIGKILL to PGID %d (reason=%s)\n", pid, reason)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[pr#%d pruefer] killProcGroupGraceful %q: SIGKILL error on pgid %d: %v\n", prNumber, label, pid, err)
	}
}
