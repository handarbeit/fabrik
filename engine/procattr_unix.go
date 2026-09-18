//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setCmdProcAttr starts cmd as a new session leader (setsid(2)) rather than
// merely a new process group. This is #1798's load-bearing fix: setsid()
// makes cmd's PID both its process group ID (PGID) and its session ID (SID),
// and POSIX guarantees SID is assigned once at fork and never changes on
// reparenting — only an explicit setsid() call in a descendant changes it.
// So every process cmd transitively forks carries this same SID for its
// entire life, however many shells deep, however it detaches (backgrounding,
// nohup, disown) and however fast its immediate parent exits — even after
// the kernel reparents it to init (PPID 1), which happens on a sub-second
// timescale for nohup/disown and defeats any PPID-chain-walk-based reaper.
// engine/descendant_reap_unix.go's trackWorkerDescendants exploits this by
// matching live processes' SID against cmd's own PID.
//
// killProcGroup/killProcGroupGraceful's kill(-pid, sig) group-kill is
// unaffected: setsid() also makes the caller its own process-group leader
// (PGID == own PID), exactly as Setpgid: true already provided, so the
// existing PGID-scoped kill needs no changes. The worker runs fully headless
// (stdin is a string reader, not a tty), so losing a controlling terminal via
// setsid() has no behavioral effect.
func setCmdProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// killProcGroup sends SIGKILL to cmd's entire process group, cleaning up any
// grandchild processes that outlived the Claude process. ESRCH (no such process)
// is silently ignored — the group may already be gone. Unexpected errors are
// logged to stderr so cleanup failures are diagnosable.
func killProcGroup(cmd *exec.Cmd, issueNumber int, label string) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	claudeLog(issueNumber, "kill", "sending SIGKILL to PGID %d (grandchild cleanup)\n", pid)
	// Negative PID targets the process group (PGID == Claude's PID when Setpgid is set).
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[#%d engine] killProcGroup %q: unexpected error killing process group %d: %v\n", issueNumber, label, pid, err)
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

// killProcGroupGraceful sends signals in escalating order to the process group:
// SIGINT → (sigintGrace) → SIGTERM → (sigtermGrace) → SIGKILL.
// A zero sigintGrace skips the SIGINT step entirely (e.g. when stage yaml has sigint: 0s).
// A zero sigtermGrace skips the SIGTERM step (falls straight to SIGKILL).
// Liveness is re-probed before each subsequent signal; ESRCH stops escalation.
// This gives well-behaved child processes (e.g. test runners posting Commit Statuses)
// a chance to flush and exit cleanly before the heavier signals land.
func killProcGroupGraceful(pid, issueNumber int, label, reason string, sigintGrace, sigtermGrace time.Duration) {
	if pid <= 0 {
		return
	}
	if sigintGrace > 0 {
		claudeLog(issueNumber, "kill", "sending SIGINT to PGID %d (reason=%s)\n", pid, reason)
		if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
			if err == syscall.ESRCH {
				return // group already gone
			}
			fmt.Fprintf(os.Stderr, "[#%d engine] killProcGroupGraceful %q: SIGINT error on pgid %d: %v\n", issueNumber, label, pid, err)
		}
		time.Sleep(sigintGrace)
		if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
			return // group exited during SIGINT grace window
		}
	}

	if sigtermGrace > 0 {
		claudeLog(issueNumber, "kill", "sending SIGTERM to PGID %d (reason=%s)\n", pid, reason)
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			if err == syscall.ESRCH {
				return
			}
			fmt.Fprintf(os.Stderr, "[#%d engine] killProcGroupGraceful %q: SIGTERM error on pgid %d: %v\n", issueNumber, label, pid, err)
		}
		time.Sleep(sigtermGrace)
		if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
			return // group exited during SIGTERM grace window
		}
	}

	claudeLog(issueNumber, "kill", "sending SIGKILL to PGID %d (reason=%s)\n", pid, reason)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		fmt.Fprintf(os.Stderr, "[#%d engine] killProcGroupGraceful %q: SIGKILL error on pgid %d: %v\n", issueNumber, label, pid, err)
	}
}
