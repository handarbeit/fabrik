//go:build !windows

package pruefer

// setCmdProcAttr / killProcGroup / isProcessAlive remain a small copy of
// engine/procattr_unix.go (adrs/1113-pruefer-v1-architecture.md). The
// session-wide escalation and post-exit sweep are NOT copied: they live in
// internal/sessionreap, shared with the engine (#1989, adrs/1989-session-wide-worker-reap.md).

import (
	"context"
	"errors"
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
// grandchild processes that outlived the claude process. The signal goes through
// sessionreap.SignalGroup, which refuses catastrophic targets and skips a stored
// PID whose group is no longer ours (#1957, ADR-1957). ESRCH is silently ignored.
// Unexpected errors are logged so cleanup failures are diagnosable.
func killProcGroup(cmd *exec.Cmd, prNumber int, label string) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// SignalGroup refuses -1/0/1 and the caller's own group (R4) and skips a
	// stored PID that no longer names the group we started (R1, #1957). Both
	// cases are logged there; nothing is ever signalled instead.
	logf(prNumber, "kill", "sending SIGKILL to PGID %d (grandchild cleanup)\n", pid)
	err := sessionreap.SignalGroup(sessionreap.OwnerOf(pid), syscall.SIGKILL, sessionReapOptions(prNumber))
	if err != nil && !errors.Is(err, sessionreap.ErrUnsafeGroup) && !errors.Is(err, sessionreap.ErrGroupNotOwned) {
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

// reviewSessionSampleInterval is how often a running review's process tree is
// sampled for the command sessions its Bash-tool shells create. Test-overridable.
var reviewSessionSampleInterval = 2 * time.Second

// trackReviewSessions registers a command-session tracker for the review
// worker pid and samples it until ctx is done. The Bash tool runs every command
// in a session of its own (the shell calls setsid), which the worker's SID does
// not reach; sampling while the parent chain exists is what lets the post-exit
// sweep find them. The returned func unregisters the tracker — call it after
// reapReviewSession.
func trackReviewSessions(ctx context.Context, pid, prNumber int) (stop func()) {
	tr := sessionreap.Track(pid, sessionReapOptions(prNumber), nil)
	go tr.Run(ctx, reviewSessionSampleInterval)
	return tr.Close
}
