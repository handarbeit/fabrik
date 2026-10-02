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

func (o Options) zombie(pid int) bool {
	if o.Zombie != nil {
		return o.Zombie(pid)
	}
	return isZombie(pid)
}

func (o Options) comm(pid int) string {
	if o.Comm != nil {
		return o.Comm(pid)
	}
	return commOf(pid)
}

// Members returns the live PIDs whose session ID is sid, including the
// session leader (pid == sid) if it is still present. One process listing and
// one Getsid per entry — no per-candidate subprocess. A zombie (dead but not
// yet reaped by its parent) still answers Getsid but can neither run nor be
// signalled to any effect, so it is not a member: counting it would hold a
// grace window open and inflate the R5 "left work running" count.
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
		if got, gerr := o.getsid(pid); gerr == nil && got == sid && !o.zombie(pid) {
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
// every member of every session the worker owns: its own SID plus the command
// sessions its Bash-tool shells created (see the package doc). The set is
// re-resolved before each signal while the worker is still provably ours, so a
// command started during a grace window is still reached. A zero grace skips
// that signal; each grace window ends early once no member is left, and
// escalation stops as soon as every session is empty. One R5 line is logged
// per signal step, naming the command sessions when there are any.
func Escalate(sid int, reason string, sigintGrace, sigtermGrace time.Duration, o Options) {
	if err := CheckSID(sid); err != nil {
		o.logf("kill", "session reap refused (reason=%s): %v\n", reason, err)
		return
	}
	e := &escalation{sid: sid, reason: reason, o: o, watch: map[int]*leaderWatch{}, dropped: map[int]bool{}}
	// Discovery needs the start token captured when the worker was started, i.e.
	// a registered Tracker. Without one there is nothing to prove the live
	// holder of PID sid is still the worker (it may have been reaped and its PID
	// recycled), so only the worker's own SID is acted on — never a guess.
	if t := trackerFor(sid); t != nil {
		e.workerStart = t.workerStart
	}
	if sigintGrace > 0 {
		if _, live := e.step(syscall.SIGINT, "SIGINT"); !live || e.waitEmpty(sigintGrace) {
			return
		}
	}
	if sigtermGrace > 0 {
		if _, live := e.step(syscall.SIGTERM, "SIGTERM"); !live || e.waitEmpty(sigtermGrace) {
			return
		}
	}
	e.step(syscall.SIGKILL, "SIGKILL")
}

type escalation struct {
	sid         int
	reason      string
	o           Options
	workerStart string
	targets     []Session
	watch       map[int]*leaderWatch
	dropped     map[int]bool // sessions abandoned because their leader PID was recycled
}

// observe feeds one membership listing of session sid to its leaderWatch and
// abandons the session if its leader PID was recycled.
func (e *escalation) observe(sid int, members []int) bool {
	w := e.watch[sid]
	if w == nil {
		w = &leaderWatch{sid: sid}
		e.watch[sid] = w
	}
	if !w.observe(members) {
		return false
	}
	if !e.dropped[sid] {
		e.dropped[sid] = true
		e.o.logf("kill", "session reap stopped for session %d (reason=%s): PID %d reappeared as a session leader after the leader was gone (recycled PID); SID %d is ambiguous\n", sid, e.reason, sid, sid)
	}
	return true
}

// resolve refreshes the target set. Sessions found at an earlier step are kept
// (re-validated) even when this step can no longer rediscover them — the worker
// that anchored the parent chain may have exited during the grace window, and a
// command session it started must still be escalated through SIGTERM/SIGKILL.
func (e *escalation) resolve() {
	fresh := e.o.targets(e.sid, e.workerStart, true)
	have := make(map[int]bool, len(fresh))
	for _, s := range fresh {
		have[s.SID] = true
	}
	for _, s := range e.targets {
		if s.SID == e.sid || have[s.SID] {
			continue
		}
		if ValidSession(s, e.o) {
			fresh = append(fresh, s)
			have[s.SID] = true
		}
	}
	e.targets = fresh
}

func (e *escalation) step(sig syscall.Signal, name string) (int, bool) {
	o := e.o
	e.resolve()
	total, live := 0, false
	var names []string
	var cmdSIDs []int
	for _, s := range e.targets {
		if e.dropped[s.SID] {
			continue
		}
		members, err := Members(s.SID, o)
		if err != nil {
			if s.SID == e.sid {
				o.logf("warn", "session reap: %v — falling back to the process group of %d\n", err, s.SID)
				if kerr := syscall.Kill(-s.SID, sig); kerr != nil && kerr != syscall.ESRCH {
					o.logf("warn", "session reap: group %s of %d: %v\n", name, s.SID, kerr)
				}
				live = true // unknown membership: keep escalating
			}
			continue
		}
		if e.observe(s.SID, members) || len(members) == 0 {
			continue
		}
		live = true
		// Look names up before the signal: a process killed by it cannot be queried.
		byPID := make(map[int]string, len(members))
		for _, pid := range members {
			byPID[pid] = o.comm(pid)
		}
		sent := signalMembers(s.SID, members, sig, o)
		total += len(sent)
		for _, pid := range sent {
			names = append(names, byPID[pid])
		}
		if s.SID != e.sid && len(sent) > 0 {
			cmdSIDs = append(cmdSIDs, s.SID)
		}
	}
	if !live {
		return 0, false
	}
	o.logf("kill", "sending %s to session %d%s (reason=%s): signalled %d member(s): %s\n",
		name, e.sid, commandSessionNote(cmdSIDs), e.reason, total, formatComms(names))
	return total, true
}

// commandSessionNote renders " + command session(s) [a b]" for the R5 line.
func commandSessionNote(sids []int) string {
	if len(sids) == 0 {
		return ""
	}
	return fmt.Sprintf(" + %d command session(s) %v", len(sids), sids)
}

// waitEmpty polls until no target session has members or grace elapses; it
// reports whether escalation should stop — every session emptied (or was
// abandoned as recycled). A listing error is "not empty".
func (e *escalation) waitEmpty(grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		empty := true
		for _, s := range e.targets {
			if e.dropped[s.SID] {
				continue
			}
			m, err := Members(s.SID, e.o)
			if err != nil {
				empty = false
				continue
			}
			if e.observe(s.SID, m) {
				continue
			}
			if len(m) > 0 {
				empty = false
			}
		}
		if empty {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(e.o.poll(), left))
	}
}

// leaderWatch guards Escalate against a recycled leader PID. A session leader
// is reaped (cmd.Wait for the worker, the Bash tool for a command shell) while
// Escalate may still be inside a grace window, freeing its PID. Once any
// listing has shown the session without its leader, a later listing that shows
// PID == sid again is a different process that became a session leader — its
// members carry the same SID value but are not ours, so that session must be
// abandoned rather than signalled.
type leaderWatch struct {
	sid        int
	leaderGone bool
	recycled   bool
}

// observe records one membership listing and reports whether it shows a
// recycled leader PID.
func (w *leaderWatch) observe(members []int) bool {
	hasLeader := false
	for _, pid := range members {
		if pid == w.sid {
			hasLeader = true
			break
		}
	}
	switch {
	case hasLeader && w.leaderGone:
		w.recycled = true
	case !hasLeader && len(members) > 0:
		w.leaderGone = true
	}
	return w.recycled
}

// Sweep is the post-exit step: SIGKILL every remaining member of the worker's
// sessions — its own SID and the command sessions the Tracker sampled while it
// ran. The leader is gone by now, so for the worker's own SID a live process
// whose PID equals sid is a recycled PID: that SID is then refused outright,
// since members carrying it may belong to the recycled leader's own session. A
// command session is acted on only while ValidSession holds (re-checked every
// pass); its leader, if still alive with the sampled start token, is itself a
// target — typically the orphaned Bash-tool shell. No discovery runs here: with
// the worker reaped the parent chain is gone and its PID may be recycled, so
// only sessions sampled while it lived are used. It rescans once briefly for a
// fork that raced the first pass and logs a single R5 line when anything was
// signalled. exitKind ("clean_exit" / "after_stop") is only for the log: a
// non-zero count on a clean exit means the worker left work running. Returns
// the number signalled.
func Sweep(sid int, exitKind string, o Options) int {
	if err := CheckSID(sid); err != nil {
		o.logf("kill", "session reap refused (exit=%s): %v\n", exitKind, err)
		return 0
	}
	targets := o.targets(sid, "", false)
	var names []string
	var cmdSIDs []int
	inCmd := map[int]bool{}
	total := 0
	refused := false
	for pass := 0; pass < 2; pass++ {
		if pass > 0 {
			time.Sleep(o.poll() / 2)
		}
		signalledThisPass := 0
		for _, s := range targets {
			if s.SID != sid && !ValidSession(s, o) {
				continue
			}
			members, err := Members(s.SID, o)
			if err != nil {
				o.logf("warn", "session reap sweep: %v\n", err)
				continue
			}
			var tg []int
			skip := false
			for _, pid := range members {
				if pid == s.SID && s.SID == sid {
					// The worker's leader has been reaped, so a live process with
					// PID == sid is a recycled PID that became a session leader
					// itself: every member now carrying this SID value may belong to
					// ITS session, and the SID can no longer tell the two apart.
					// Refuse this SID (R4); the #1814 sweep's start-time bounds can
					// discriminate.
					if !refused {
						refused = true
						o.logf("warn", "session reap sweep refused (exit=%s): PID %d is live and a member of its own session (recycled leader PID); SID %d is ambiguous\n", exitKind, sid, sid)
					}
					skip = true
					break
				}
				tg = append(tg, pid)
			}
			if skip {
				continue
			}
			// Look names up before the kill: a dead process cannot be queried.
			byPID := make(map[int]string, len(tg))
			for _, pid := range tg {
				byPID[pid] = o.comm(pid)
			}
			sent := signalMembers(s.SID, tg, syscall.SIGKILL, o)
			for _, pid := range sent {
				names = append(names, byPID[pid])
			}
			if s.SID != sid && len(sent) > 0 && !inCmd[s.SID] {
				inCmd[s.SID] = true
				cmdSIDs = append(cmdSIDs, s.SID)
			}
			signalledThisPass += len(sent)
			total += len(sent)
		}
		if signalledThisPass == 0 {
			break
		}
	}
	if total > 0 {
		o.logf("kill", "session reap: signalled %d member(s) of session %d%s (exit=%s): %s\n",
			total, sid, commandSessionNote(cmdSIDs), exitKind, formatComms(names))
	}
	return total
}
