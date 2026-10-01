//go:build !windows

package sessionreap

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// CheckSID refuses a session ID that must never be a signalling target: 0, 1,
// or the caller's own session (R4).
func CheckSID(sid int) error {
	if sid <= 1 {
		return fmt.Errorf("%w: %d", ErrUnsafeSID, sid)
	}
	if own, err := unix.Getsid(0); err == nil && own == sid {
		return fmt.Errorf("%w: %d is the caller's own session", ErrUnsafeSID, sid)
	}
	return nil
}

func (o Options) getsid(pid int) (int, error) {
	if o.Getsid != nil {
		return o.Getsid(pid)
	}
	return unix.Getsid(pid)
}

func (o Options) list() ([]int, error) {
	if o.List != nil {
		return o.List()
	}
	return listPIDs()
}

func (o Options) comm(pid int) string {
	if o.Comm != nil {
		return o.Comm(pid)
	}
	return commOf(pid)
}

// Members returns the live PIDs whose session ID is sid, including the
// session leader (pid == sid) if it is still present. One process listing and
// one Getsid per entry — no per-candidate subprocess.
func Members(sid int, o Options) ([]int, error) {
	if err := CheckSID(sid); err != nil {
		return nil, err
	}
	pids, err := o.list()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var out []int
	for _, pid := range pids {
		if got, gerr := o.getsid(pid); gerr == nil && got == sid {
			out = append(out, pid)
		}
	}
	return out, nil
}

// signalMembers sends sig to each pid immediately after re-checking that its
// session ID is still sid (R4), returning the PIDs actually signalled.
func signalMembers(sid int, pids []int, sig syscall.Signal, o Options) []int {
	var sent []int
	for _, pid := range pids {
		if got, err := o.getsid(pid); err != nil || got != sid {
			continue
		}
		if err := syscall.Kill(pid, sig); err == nil {
			sent = append(sent, pid)
		}
	}
	return sent
}

func (o Options) names(pids []int) []string {
	names := make([]string, 0, len(pids))
	for _, pid := range pids {
		names = append(names, o.comm(pid))
	}
	return names
}

// Signal sends sig to every member of session sid, leader included, and
// returns how many were signalled. On a listing error it degrades to the
// leader's process group (today's pre-#1989 behaviour) rather than widening.
func Signal(sid int, sig syscall.Signal, o Options) (int, error) {
	members, err := Members(sid, o)
	if err != nil {
		if errors.Is(err, ErrUnsafeSID) {
			o.logf("kill", "session reap refused: %v\n", err)
			return 0, err
		}
		o.logf("warn", "session reap: %v — falling back to the process group of %d\n", err, sid)
		if kerr := syscall.Kill(-sid, sig); kerr != nil {
			return 0, kerr
		}
		return 1, nil
	}
	return len(signalMembers(sid, members, sig, o)), nil
}

// Escalate runs SIGINT → sigintGrace → SIGTERM → sigtermGrace → SIGKILL over
// every member of session sid. A zero grace skips that signal; each grace
// window ends early once no member is left, and escalation stops as soon as
// the session is empty. One R5 line is logged per signal step.
func Escalate(sid int, reason string, sigintGrace, sigtermGrace time.Duration, o Options) {
	if err := CheckSID(sid); err != nil {
		o.logf("kill", "session reap refused (reason=%s): %v\n", reason, err)
		return
	}
	step := func(sig syscall.Signal, name string) (int, bool) {
		members, err := Members(sid, o)
		if err != nil {
			o.logf("warn", "session reap: %v — falling back to the process group of %d\n", err, sid)
			if kerr := syscall.Kill(-sid, sig); kerr != nil && kerr != syscall.ESRCH {
				o.logf("warn", "session reap: group %s of %d: %v\n", name, sid, kerr)
			}
			return 0, true // unknown membership: keep escalating
		}
		if len(members) == 0 {
			return 0, false
		}
		sent := signalMembers(sid, members, sig, o)
		o.logf("kill", "sending %s to session %d (reason=%s): signalled %d member(s): %s\n",
			name, sid, reason, len(sent), formatComms(o.names(sent)))
		return len(sent), true
	}
	if sigintGrace > 0 {
		if _, live := step(syscall.SIGINT, "SIGINT"); !live || o.waitEmpty(sid, sigintGrace) {
			return
		}
	}
	if sigtermGrace > 0 {
		if _, live := step(syscall.SIGTERM, "SIGTERM"); !live || o.waitEmpty(sid, sigtermGrace) {
			return
		}
	}
	step(syscall.SIGKILL, "SIGKILL")
}

// waitEmpty polls until session sid has no members or grace elapses; it
// reports whether the session emptied. A listing error is "not empty".
func (o Options) waitEmpty(sid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if m, err := Members(sid, o); err == nil && len(m) == 0 {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(o.poll(), left))
	}
}

// Sweep is the post-exit step: SIGKILL every remaining member of session sid.
// The leader is gone by now, so a live process whose PID equals sid is a
// recycled PID: the sweep then refuses outright, since members carrying that
// SID value may belong to the recycled leader's own session. It rescans once briefly for a fork that raced the
// first pass and logs a single R5 line when anything was signalled. exitKind
// ("clean_exit" / "after_stop") is only for the log: a non-zero count on a
// clean exit means the worker left work running. Returns the number signalled.
func Sweep(sid int, exitKind string, o Options) int {
	if err := CheckSID(sid); err != nil {
		o.logf("kill", "session reap refused (exit=%s): %v\n", exitKind, err)
		return 0
	}
	var names []string
	total := 0
	for pass := 0; pass < 2; pass++ {
		if pass > 0 {
			time.Sleep(o.poll() / 2)
		}
		members, err := Members(sid, o)
		if err != nil {
			o.logf("warn", "session reap sweep: %v\n", err)
			break
		}
		var targets []int
		for _, pid := range members {
			if pid == sid {
				// The leader has been reaped, so a live process with PID == sid
				// is a recycled PID that became a session leader itself: every
				// member now carrying this SID value may belong to ITS session,
				// and the SID can no longer tell the two apart. Refuse the whole
				// sweep (R4); the #1814 sweep's start-time bounds can discriminate.
				o.logf("warn", "session reap sweep refused (exit=%s): PID %d is live and a member of its own session (recycled leader PID); SID %d is ambiguous\n", exitKind, sid, sid)
				return total
			}
			targets = append(targets, pid)
		}
		// Look names up before the kill: a dead process cannot be queried.
		byPID := make(map[int]string, len(targets))
		for _, pid := range targets {
			byPID[pid] = o.comm(pid)
		}
		sent := signalMembers(sid, targets, syscall.SIGKILL, o)
		for _, pid := range sent {
			names = append(names, byPID[pid])
		}
		total += len(sent)
		if len(sent) == 0 {
			break
		}
	}
	if total > 0 {
		o.logf("kill", "session reap: signalled %d member(s) of session %d (exit=%s): %s\n",
			total, sid, exitKind, formatComms(names))
	}
	return total
}
