//go:build !windows

package sessionreap

import (
	"context"
	"sort"
	"sync"
	"syscall"
	"time"
)

func (o Options) procs() ([]Proc, error) {
	if o.Procs != nil {
		return o.Procs()
	}
	if o.List != nil {
		pids, err := o.List()
		if err != nil {
			return nil, err
		}
		out := make([]Proc, 0, len(pids))
		for _, pid := range pids {
			out = append(out, Proc{PID: pid, PPID: -1}) // parent unknown: nothing to discover
		}
		return out, nil
	}
	return listProcs()
}

func (o Options) startToken(pid int) string {
	if o.Start != nil {
		return o.Start(pid)
	}
	return startToken(pid)
}

// leaderAlive reports whether a live, non-zombie process holds pid.
func (o Options) leaderAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return false
	}
	return !o.zombie(pid)
}

// Discover returns the command sessions under worker: the distinct session IDs,
// other than the worker's own, carried by the worker's descendants in the
// parent chain. It must run while the worker is alive — once it exits its
// children are reparented to PID 1 and the chain is gone (hence Tracker).
//
// Safe by construction: a descendant either inherits the worker's SID or is the
// leader of a session some descendant created, so any other SID it carries was
// created below the worker, never by an unrelated process. Each SID still goes
// through CheckSID (0, 1 and the caller's own session are refused).
func Discover(worker int, o Options) ([]Session, error) {
	if o.WorkerSIDOnly || worker <= 1 || (o.Procs == nil && o.List != nil) {
		return nil, nil // restricted, unsafe, or a PID-only seam that carries no parents
	}
	procs, err := o.procs()
	if err != nil {
		return nil, err
	}
	children := make(map[int][]int, len(procs))
	for _, p := range procs {
		if p.PPID > 0 {
			children[p.PPID] = append(children[p.PPID], p.PID)
		}
	}
	seen := map[int]bool{worker: true}
	var queue []int
	queue = append(queue, children[worker]...)
	found := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		queue = append(queue, children[pid]...)
		sid, err := o.getsid(pid)
		if err != nil || sid == worker || CheckSID(sid) != nil {
			continue
		}
		found[sid] = true
	}
	out := make([]Session, 0, len(found))
	for sid := range found {
		out = append(out, Session{SID: sid, Start: o.leaderStart(sid)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out, nil
}

// leaderStart is the start token of the live leader of session sid, or "" when
// the leader is gone (the session then survives only through its members).
func (o Options) leaderStart(sid int) string {
	if !o.leaderAlive(sid) {
		return ""
	}
	return o.startToken(sid)
}

// ValidSession reports whether s's SID can still be trusted to name the session
// that was sampled. If no live process holds PID == SID the leader is gone and
// any member carrying the SID belongs to the old session (POSIX does not reuse
// a PID that still names a live session). If a live process holds it, it is the
// sampled leader only when its start token equals the recorded one; a recorded
// "" (leader already gone at sampling) or an unreadable token fails closed.
func ValidSession(s Session, o Options) bool {
	if CheckSID(s.SID) != nil {
		return false
	}
	if !o.leaderAlive(s.SID) {
		return true
	}
	return s.Start != "" && o.startToken(s.SID) == s.Start
}

// Tracker accumulates the command sessions observed under one worker while it
// runs, and is consulted by Escalate and Sweep through the registry.
type Tracker struct {
	worker int
	o      Options
	onNew  func([]Session)
	// workerStart is the worker's own start token, captured right after it was
	// started, when its PID belongs to exactly one process. Discovery from a PID
	// is only done while that token still matches: once the worker has been
	// reaped its PID may be a recycled, unrelated process whose descendants must
	// never be mistaken for ours.
	workerStart string

	mu   sync.Mutex
	seen map[int]Session
}

var (
	trackersMu sync.Mutex
	trackers   = map[int]*Tracker{}
)

// Track registers a Tracker for worker (its PID, which is its SID). onNew, if
// non-nil, receives each batch of newly observed sessions (the engine persists
// them). Close unregisters it.
func Track(worker int, o Options, onNew func([]Session)) *Tracker {
	t := &Tracker{worker: worker, o: o, onNew: onNew, seen: map[int]Session{}, workerStart: o.startToken(worker)}
	trackersMu.Lock()
	trackers[worker] = t
	trackersMu.Unlock()
	return t
}

func trackerFor(worker int) *Tracker {
	trackersMu.Lock()
	defer trackersMu.Unlock()
	return trackers[worker]
}

// Close unregisters the tracker; later Escalate/Sweep calls for this worker
// fall back to what Discover can still see.
func (t *Tracker) Close() {
	trackersMu.Lock()
	if trackers[t.worker] == t {
		delete(trackers, t.worker)
	}
	trackersMu.Unlock()
}

// add records sessions not seen before and returns them.
func (t *Tracker) add(ss []Session) []Session {
	t.mu.Lock()
	var fresh []Session
	for _, s := range ss {
		if _, ok := t.seen[s.SID]; !ok {
			t.seen[s.SID] = s
			fresh = append(fresh, s)
		}
	}
	t.mu.Unlock()
	if len(fresh) > 0 && t.onNew != nil {
		t.onNew(fresh)
	}
	return fresh
}

// Sample discovers the worker's command sessions now (one process listing) and
// returns the newly observed ones.
func (t *Tracker) Sample() []Session {
	if !t.o.workerIsOurs(t.worker, t.workerStart) {
		return nil
	}
	ss, err := Discover(t.worker, t.o)
	if err != nil {
		return nil
	}
	return t.add(ss)
}

// Sessions returns every command session observed so far.
func (t *Tracker) Sessions() []Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Session, 0, len(t.seen))
	for _, s := range t.seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out
}

// Run samples immediately and then every interval until ctx is done.
func (t *Tracker) Run(ctx context.Context, interval time.Duration) {
	t.Sample()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.Sample()
		}
	}
}

// workerIsOurs reports whether the live process holding pid is the worker whose
// start token was recorded — never true for an unreaped-then-recycled PID, and
// never true when no token could be captured (fail closed).
func (o Options) workerIsOurs(pid int, workerStart string) bool {
	return workerStart != "" && o.leaderAlive(pid) && o.startToken(pid) == workerStart
}

// targets resolves the sessions to act on for worker sid: the worker's own SID
// first, then every command session that is still valid — those the tracker
// sampled during the invocation plus, when discover is set and the worker is
// still provably ours, a fresh discovery (which also feeds the tracker).
// Sessions that fail validation are logged and dropped. discover must be false
// once the worker has exited: its PID may then be a recycled process.
func (o Options) targets(sid int, workerStart string, discover bool) []Session {
	out := []Session{{SID: sid}}
	if o.WorkerSIDOnly {
		return out
	}
	t := trackerFor(sid)
	var cand []Session
	if discover && o.workerIsOurs(sid, workerStart) {
		if fresh, err := Discover(sid, o); err == nil {
			if t != nil {
				t.add(fresh)
			}
			cand = append(cand, fresh...)
		}
	}
	if t != nil {
		cand = append(cand, t.Sessions()...)
	}
	have := map[int]bool{sid: true}
	for _, s := range cand {
		if have[s.SID] {
			continue
		}
		have[s.SID] = true
		if !ValidSession(s, o) {
			o.logf("warn", "session reap: command session %d dropped — its leader PID is held by a different process (recycled PID)\n", s.SID)
			continue
		}
		out = append(out, s)
	}
	return out
}

// Reap SIGKILLs every member — leader included — of a persisted command
// session, for the janitor path where no worker is alive to ask. It returns how
// many were signalled and how many members remain afterwards. A session that
// fails ValidSession is treated as gone (remaining 0): its SID no longer
// identifies what was sampled, so nothing is signalled.
func Reap(s Session, o Options) (signalled, remaining int) {
	if !ValidSession(s, o) {
		return 0, 0
	}
	members, err := Members(s.SID, o)
	if err != nil {
		return 0, 1 // unknown: keep the record
	}
	sent := signalMembers(s.SID, members, syscall.SIGKILL, o)
	return len(sent), len(members) - len(sent)
}

// Empty reports whether session s has no live members (or no longer names the
// sampled session). A listing error is "not empty".
func Empty(s Session, o Options) bool {
	if !ValidSession(s, o) {
		return true
	}
	m, err := Members(s.SID, o)
	return err == nil && len(m) == 0
}
