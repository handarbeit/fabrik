package gate

import (
	"syscall"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// Procs is the gate's seam onto the host's processes for the bed-engine teardown
// (#2245, ADR-2245): liveness, signalling, start tokens and the engine's
// instance lock. pidAlive/termPID are direct syscalls outside the Commander, so
// without this seam the teardown could only be tested against real children.
// Implementations are stateless and safe for concurrent use.
type Procs interface {
	// Alive reports whether a process we may signal holds pid.
	Alive(pid int) bool
	// Signal sends sig to the single process pid.
	Signal(pid int, sig syscall.Signal) error
	// Token is pid's start-time token ("" when unreadable).
	Token(pid int) string
	// SignalGroup sends sig to the process group led by owner.PID, only when
	// the PID and start token still match (ADR-1957).
	SignalGroup(owner sessionreap.Owner, sig syscall.Signal) error
	// LockHeld reports whether a live engine holds the bed's per-directory
	// instance flock (<bed>/.fabrik/fabrik.lock). False when it cannot tell.
	LockHeld(bed string) bool
}

// osProcs is the real Procs.
type osProcs struct{}

func (osProcs) Alive(pid int) bool { return pidAlive(pid) }

func (osProcs) Token(pid int) string { return sessionreap.StartToken(pid) }

func (osProcs) SignalGroup(owner sessionreap.Owner, sig syscall.Signal) error {
	return sessionreap.SignalGroup(owner, sig, sessionreap.Options{})
}
