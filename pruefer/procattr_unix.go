//go:build !windows

package pruefer

// setCmdProcAttr / killProcGroup / isProcessAlive remain a small copy of
// engine/procattr_unix.go (adrs/1113-pruefer-v1-architecture.md). The
// session-wide escalation and post-exit sweep are NOT copied: they live in
// internal/sessionreap, shared with the engine (#1989, adrs/1989-session-wide-worker-reap.md).

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// setCmdProcAttr starts cmd as a new session leader (setsid(2)): cmd's PID is
// then its process group ID and its session ID, so every descendant carries
// that SID however it detaches or is reparented, and internal/sessionreap can
// reap the whole session (#1989). Setsid also makes cmd a group leader, so
// killProcGroup's kill(-pid, …) is unchanged.
func setCmdProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

// killProcGroupGraceful stops the worker's whole SESSION with escalating
// signals: SIGINT → (sigintGrace) → SIGTERM → (sigtermGrace) → SIGKILL. pid
// is also the session ID because setCmdProcAttr uses Setsid. A zero
// sigintGrace skips SIGINT; a zero sigtermGrace skips SIGTERM. Shared with the
// engine via internal/sessionreap (#1989, ADR-1989).
func killProcGroupGraceful(pid, prNumber int, label, reason string, sigintGrace, sigtermGrace time.Duration) {
	if pid <= 0 {
		return
	}
	sessionreap.Escalate(pid, reason, sigintGrace, sigtermGrace, sessionReapOptions(prNumber))
}

// reapReviewSession is the post-exit session sweep: SIGKILL anything still in
// the exited worker's session (a Bash-tool command in a separate process
// group, which killProcGroup cannot reach). exitKind is "clean_exit" or
// "after_stop"; a non-zero count on a clean exit means the worker left work
// running.
func reapReviewSession(pid, prNumber int, exitKind string) int {
	if pid <= 0 {
		return 0
	}
	return sessionreap.Sweep(pid, exitKind, sessionReapOptions(prNumber))
}

func sessionReapOptions(prNumber int) sessionreap.Options {
	return sessionreap.Options{
		Log: func(tag, format string, args ...any) { logf(prNumber, tag, format, args...) },
	}
}
