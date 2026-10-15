package gate

import (
	"context"
	"strings"
	"sync"
	"time"
)

// The runner-side reaper for a sim test binary that outlives its own ceiling
// (#2247, ADR-2247; amends ADR-2226). testlife's self-kill is a Go timer inside
// the process it is meant to kill, so it cannot fire when the process is stuck in
// its exit path — seen on 2026-10-13, when two -race sim.test binaries printed
// "exiting" at their 40-minute ceiling and then spun at ~97% CPU for about an
// hour. The gate owns the session the sim runs in, so it watches that session
// from outside.
//
// The rule is ADR-2128's: positive evidence only. A binary is signalled only if
//   - it is a member of the sim's own session (sessionreap.Members, which refuses
//     PID 0/1 and the caller's own session),
//   - its command name ends in ".test",
//   - it has been seen, with the SAME start token, for at least ceiling + grace
//     (first-seen is at most one poll after its real start, so the kill can only
//     be late, never early — and the in-process ceiling always gets the first
//     chance), and
//   - its start token still matches immediately before the signal (a recycled
//     PID is a different token and is left alone).
//
// It signals a single PID with SIGKILL, never a group, and logs every kill.

// simReaperOps are the process-table operations, injectable so the rule is tested
// with a fake table.
type simReaperOps struct {
	Members    func(sid int) ([]int, error)
	StartToken func(pid int) string
	Comm       func(pid int) string
	Kill       func(pid int) error
}

type simProc struct {
	pid int
	tok string
}

type simReaper struct {
	ops   simReaperOps
	now   func() time.Time
	limit time.Duration // ceiling + grace
	logf  func(format string, args ...any)

	mu     sync.Mutex
	seen   map[simProc]time.Time
	reaped []int
}

func newSimReaper(ops simReaperOps, now func() time.Time, limit time.Duration, logf func(string, ...any)) *simReaper {
	return &simReaper{ops: ops, now: now, limit: limit, logf: logf, seen: map[simProc]time.Time{}}
}

// check is one pass over session sid.
func (r *simReaper) check(sid int) {
	members, err := r.ops.Members(sid)
	if err != nil {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	live := map[simProc]bool{}
	for _, pid := range members {
		if !strings.HasSuffix(r.ops.Comm(pid), ".test") {
			continue
		}
		tok := r.ops.StartToken(pid)
		if tok == "" {
			continue // identity unreadable: keep running, never kill
		}
		key := simProc{pid, tok}
		live[key] = true
		first, ok := r.seen[key]
		if !ok {
			r.seen[key] = now
			continue
		}
		if now.Sub(first) < r.limit {
			continue
		}
		if r.ops.StartToken(pid) != tok {
			continue // the PID was recycled between the two reads
		}
		if err := r.ops.Kill(pid); err != nil {
			r.logf("sim reaper: could not kill pid %d: %v\n", pid, err)
			continue
		}
		r.logf("sim reaper: pid %d (%s) outlived its ceiling by more than the grace (seen for %s) — SIGKILLed\n",
			pid, r.ops.Comm(pid), now.Sub(first).Round(time.Second))
		r.reaped = append(r.reaped, pid)
		delete(r.seen, key)
		delete(live, key)
	}
	for k := range r.seen {
		if !live[k] {
			delete(r.seen, k)
		}
	}
}

func (r *simReaper) killed() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.reaped...)
}

// run polls until ctx is done.
func (r *simReaper) run(ctx context.Context, sid int, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.check(sid)
		}
	}
}
