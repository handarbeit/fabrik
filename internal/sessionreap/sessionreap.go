// Package sessionreap signals every process in the sessions a Claude worker
// owns — not just its process group — so a stopped worker's `go test` /
// `sim.test` trees cannot outlive it (#1989, ADR-1989).
//
// A worker is started with setsid(2), so its PID is also its session ID (SID).
// That SID alone is NOT enough, though: Claude's Bash tool starts every command
// through a shell that calls setsid() itself, so each command runs in a session
// of its own whose leader is a child of the worker. Observed live: the worker
// is a session leader (PID == PGID == SID) while its `zsh -c …` command shell,
// and the vitest/tsc/go-test tree below it, carry the shell's PID as SID. The
// worker's SID reaches none of them.
//
// The target set is therefore {worker SID} ∪ {command-session SIDs}. Command
// sessions are discovered from the parent chain while the worker is alive —
// every descendant whose session ID is not the worker's names one — and are
// sampled during the invocation (Tracker) because once the worker exits its
// children are reparented to PID 1 and the chain is gone. Members of any target
// session are found by session ID (unix.Getsid over one process listing), never
// by `ps`'s session column (which reads 0 on macOS); membership survives
// reparenting. A command session's SID is only trusted while its leader PID is
// provably the process that was sampled (start-time token), because a recycled
// PID that became a session leader would otherwise alias an unrelated session.
//
// The package is a leaf shared by engine and pruefer; neither may import the
// other, and log output goes through a caller-supplied Logger because the two
// use different loggers. A command that starts and orphans itself between two
// samples, after which the worker exits, is the residual gap (ADR 063's
// worktree-cwd reaper is the cover).
package sessionreap

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrUnsafeSID is returned (and logged) for a session ID that must never be
// signalled: 0, 1, or the caller's own session (R4).
var ErrUnsafeSID = errors.New("unsafe session id")

// Logger writes one tagged line. The caller binds the issue/PR number.
type Logger func(tag, format string, args ...any)

// Options carries the logger and test seams. The zero value uses the real
// process table and unix.Getsid, and discards log output.
type Options struct {
	Log    Logger
	List   func() ([]int, error)  // PIDs of every live process
	Getsid func(int) (int, error) // session ID of one PID
	Comm   func(int) string       // best-effort command name for the R5 line
	Zombie func(int) bool         // reports a dead-but-unreaped process (default: platform check)
	Procs  func() ([]Proc, error) // process table with parents; default platform lister. Falls back to List (no parent info, so no command-session discovery)
	Start  func(pid int) string   // process start-time token; "" if unknown (default: platform)

	// WorkerSIDOnly restricts every operation to the worker's own SID — the
	// pre-command-session behaviour. Test seam: a twin that runs with it set
	// proves the command-session extension is load-bearing.
	WorkerSIDOnly bool
	Poll          time.Duration // liveness poll interval during a grace window (default 50ms)
}

// Proc is one row of the process table.
type Proc struct{ PID, PPID int }

// Session is a command session discovered under a worker: the session a Bash-tool
// command's shell created for itself with setsid().
type Session struct {
	SID int
	// Start is the leader's start-time token captured while it was live; ""
	// when the leader was already gone at discovery. A live process holding
	// PID == SID later is trusted as that leader only if its token still
	// equals Start.
	Start string
}

const (
	maxLoggedComms = 5
	maxCommLen     = 40
)

func (o Options) logf(tag, format string, args ...any) {
	if o.Log != nil {
		o.Log(tag, format, args...)
	}
}

func (o Options) poll() time.Duration {
	if o.Poll > 0 {
		return o.Poll
	}
	return 50 * time.Millisecond
}

// formatComms renders the R5 member summary: at most maxLoggedComms names,
// each truncated, with a "(+k more)" tail.
func formatComms(names []string) string {
	var parts []string
	for i, n := range names {
		if i == maxLoggedComms {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(names)-maxLoggedComms))
			break
		}
		n = strings.TrimSpace(n)
		if n == "" {
			n = "?"
		}
		if len(n) > maxCommLen {
			n = n[:maxCommLen] + "…"
		}
		parts = append(parts, n)
	}
	return strings.Join(parts, ", ")
}
