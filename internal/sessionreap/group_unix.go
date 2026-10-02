//go:build !windows

package sessionreap

import (
	"fmt"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Stored-PID process-group signalling (#1957, ADR-1957).
//
// kill(-pid, sig) signals whatever process group currently has ID pid. When pid
// was recorded earlier — a worker's PID after cmd.Wait(), a webhook subprocess
// that exited during its restart backoff — the number may have been recycled by
// an unrelated group leader. SignalGroup is the single entry point for such
// kills: it refuses catastrophic targets, then confirms the group is still the
// one we started, and skips (never widens) when it cannot.
//
// The ownership rule leans on POSIX: a PID is never reused while a process
// group with that ID still has a member. So
//
//   - leader gone, group non-empty  → the group is still the original one: allow.
//     This is the normal post-exit grandchild cleanup (the leader was reaped by
//     cmd.Wait) and must keep working;
//   - leader gone, group empty      → nothing to signal: a harmless no-op;
//   - a live process holds the PID  → it must lead its own group (pgid == pid)
//     and be the process we started: its start token equals the recorded one, or
//     (no token recorded) it is our child.
//
// The check-then-kill gap is microseconds wide and is not closed here.

// CheckPGID refuses a process-group ID that must never be a signalling target:
// 0 or 1 (kill(-0)/kill(-1) would signal the caller's own group / every
// process, and PID 1 is init) or the caller's own process group (R4). It runs
// before any ownership test.
func CheckPGID(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("%w: %d", ErrUnsafeGroup, pgid)
	}
	if own := syscall.Getpgrp(); own == pgid {
		return fmt.Errorf("%w: %d is the caller's own process group", ErrUnsafeGroup, pgid)
	}
	return nil
}

func (o Options) getpgid(pid int) (int, error) {
	if o.Getpgid != nil {
		return o.Getpgid(pid)
	}
	return unix.Getpgid(pid)
}

func (o Options) kill(pid int, sig syscall.Signal) error {
	if o.Kill != nil {
		return o.Kill(pid, sig)
	}
	return syscall.Kill(pid, sig)
}

// ppid reports pid's parent; false when it cannot be determined (fail closed).
func (o Options) ppid(pid int) (int, bool) {
	if o.Ppid != nil {
		return o.Ppid(pid)
	}
	// Not o.procs(): a PID-only List seam carries no parents and would fail closed
	// here even though the real table is readable.
	lister := listProcs
	if o.Procs != nil {
		lister = o.Procs
	}
	procs, err := lister()
	if err != nil {
		return 0, false
	}
	for _, p := range procs {
		if p.PID == pid {
			return p.PPID, p.PPID > 0
		}
	}
	return 0, false
}

// groupMembers returns the live, non-zombie PIDs whose process group is pgid.
func (o Options) groupMembers(pgid int) ([]int, error) {
	pids, err := o.list()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var out []int
	for _, pid := range pids {
		if got, gerr := o.getpgid(pid); gerr == nil && got == pgid && !o.zombie(pid) {
			out = append(out, pid)
		}
	}
	return out, nil
}

// OwnsGroup reports whether the process group named by owner.PID is still the
// one we started, and whether it has anything left to signal. owned == false
// carries the reason; empty == true (with owned == true) means the leader is
// gone and the group has no live member.
func OwnsGroup(owner Owner, o Options) (owned, empty bool, reason string) {
	pid := owner.PID
	// Existence probe: signal 0 sends nothing. EPERM means a live process we may
	// not signal still holds the PID.
	err := o.kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		// Leader gone (reaped). The group is still ours only while it has members.
		members, merr := o.groupMembers(pid)
		if merr != nil {
			return false, false, fmt.Sprintf("leader %d is gone and group membership is unreadable: %v", pid, merr)
		}
		return true, len(members) == 0, ""
	}
	// A live (or zombie, still-unreaped) process holds the PID: it must be the
	// group leader and the process we started.
	if pgid, gerr := o.getpgid(pid); gerr != nil || pgid != pid {
		return false, false, fmt.Sprintf("PID %d is live but does not lead its own process group (pgid=%d, err=%v)", pid, pgid, gerr)
	}
	if owner.Start != "" {
		if got := o.startToken(pid); got != owner.Start {
			return false, false, fmt.Sprintf("PID %d is live with start token %q, not the recorded %q (recycled PID)", pid, got, owner.Start)
		}
		return true, false, ""
	}
	if pp, ok := o.ppid(pid); !ok || pp != os.Getpid() {
		return false, false, fmt.Sprintf("PID %d is live, no start token was recorded, and its parent (%d) is not this process (recycled PID)", pid, pp)
	}
	return true, false, ""
}

// SignalGroup sends sig to the process group led by owner.PID — only after the
// R4 refusals and the R1 ownership check pass. A refused or unowned target is
// logged and returned as ErrUnsafeGroup / ErrGroupNotOwned; nothing is ever
// signalled instead. An empty group or ESRCH is a nil no-op.
func SignalGroup(owner Owner, sig syscall.Signal, o Options) error {
	if err := CheckPGID(owner.PID); err != nil {
		o.logf("kill", "group kill refused: %v\n", err)
		return err
	}
	owned, empty, reason := OwnsGroup(owner, o)
	if !owned {
		o.logf("kill", "group kill of PGID %d skipped: %s\n", owner.PID, reason)
		return fmt.Errorf("%w: %s", ErrGroupNotOwned, reason)
	}
	if empty {
		return nil
	}
	if err := o.kill(-owner.PID, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// startRecords holds start tokens for stored-PID owners that have no Tracker
// (e.g. the webhook subprocess). Entries are a few bytes and callers forget them.
var (
	startMu      sync.Mutex
	startRecords = map[int]string{}
)

// RecordStart captures pid's start token right after it was started so a later
// SignalGroup can tell it from a process that recycled the PID. Returns the
// token ("" if unreadable, in which case the parent check applies).
func RecordStart(pid int, o Options) string {
	tok := o.startToken(pid)
	startMu.Lock()
	startRecords[pid] = tok
	startMu.Unlock()
	return tok
}

// ForgetStart drops the record made by RecordStart.
func ForgetStart(pid int) {
	startMu.Lock()
	delete(startRecords, pid)
	startMu.Unlock()
}

// OwnerOf returns the Owner for a stored PID: the start token recorded by its
// Tracker (workers) or by RecordStart, if any.
func OwnerOf(pid int) Owner {
	if t := trackerFor(pid); t != nil && t.workerStart != "" {
		return Owner{PID: pid, Start: t.workerStart}
	}
	startMu.Lock()
	defer startMu.Unlock()
	return Owner{PID: pid, Start: startRecords[pid]}
}
