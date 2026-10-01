//go:build !windows

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
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
// grandchild processes that outlived the Claude process. It is group-scoped
// only (the webhook subprocess also uses it); session members outside the
// group are reaped by reapWorkerSession. ESRCH (no such process)
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

// sessionEscalateFn and sessionSweepFn are the session-wide reap primitives
// (#1989, internal/sessionreap). Package-level seams so tests can swap in the
// pre-#1989 group-only behaviour to prove the session reap is load-bearing.
var (
	sessionEscalateFn = sessionreap.Escalate
	sessionSweepFn    = sessionreap.Sweep
)

// sessionReapOptions binds the engine's issue-scoped logger to sessionreap.
func sessionReapOptions(issueNumber int) sessionreap.Options {
	return sessionreap.Options{
		Log: func(tag, format string, args ...any) { claudeLog(issueNumber, tag, format, args...) },
	}
}

// killProcGroupGraceful stops a worker's whole SESSION with escalating
// signals: SIGINT → (sigintGrace) → SIGTERM → (sigtermGrace) → SIGKILL.
// pid is the worker's PID, which is also its session ID because
// setCmdProcAttr starts it with Setsid. The target is every process whose
// session ID is pid, not just its process group: Claude's Bash tool puts each
// command in a process group of its own within the worker's session, which
// kill(-pid, sig) misses (#1989). A zero sigintGrace skips SIGINT; a zero
// sigtermGrace skips SIGTERM. A grace window ends early once the session is
// empty, and escalation stops when nothing is left. This gives well-behaved
// children (e.g. test runners posting Commit Statuses) a chance to flush and
// exit cleanly before the heavier signals land.
func killProcGroupGraceful(pid, issueNumber int, label, reason string, sigintGrace, sigtermGrace time.Duration) {
	if pid <= 0 {
		return
	}
	sessionEscalateFn(pid, reason, sigintGrace, sigtermGrace, sessionReapOptions(issueNumber))
}

// reapWorkerSession is the post-exit session sweep (#1989): after the worker
// has exited and been waited on, SIGKILL anything still in its session. A
// non-zero count with exitKind "clean_exit" means the worker left work
// running. Runs ahead of #1798's reapTrackedDescendants and #1814's registry-
// independent sweep, which remain as the backstop.
func reapWorkerSession(pid, issueNumber int, exitKind string) int {
	if pid <= 0 {
		return 0
	}
	return sessionSweepFn(pid, exitKind, sessionReapOptions(issueNumber))
}
