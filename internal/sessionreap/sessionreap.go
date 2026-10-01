// Package sessionreap signals every process in a worker's session — not just
// its process group — so a stopped Claude worker's `go test` / `sim.test`
// trees cannot outlive it (#1989, ADR-1989).
//
// A worker is started with setsid(2), so its PID is also its session ID (SID).
// Claude's Bash tool starts each command in a process group of its own, which
// is a different group of the SAME session: kill(-pid, sig) misses it, while
// the session ID survives reparenting to PID 1 (POSIX). Members are found by
// session ID (unix.Getsid over one process listing), never by walking parent
// links and never by `ps`'s session column (which reads 0 on macOS).
//
// The package is a leaf shared by engine and pruefer; neither may import the
// other, and log output goes through a caller-supplied Logger because the two
// use different loggers. A process that calls setsid itself has left the
// session and is out of scope (a documented gap).
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
	Poll   time.Duration          // liveness poll interval during a grace window (default 50ms)
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
