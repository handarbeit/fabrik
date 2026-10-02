package gate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MultiBedScheduler runs a plan's cells across two or more beds at once (#1976,
// ADR-1976). One goroutine per bed pulls that bed's next cell, and every cell
// first acquires — atomically, all or nothing — the set of GitHub identities it
// charges (identity.go). Two legs on one identity therefore never overlap: the
// later one waits, and says so. A single-bed run never uses it (R6): it keeps
// SerialScheduler and today's failure semantics.
//
// Assignment (D6/D7). Under the sparse matrix, when the plan holds a baseline
// cell (app/on — and its isolated sibling, which follows it), bed A runs exactly
// those, in plan order, and nothing else; the other beds serve a shared queue of
// the remaining cells in sparseOrder. With two beds that is "bed B runs app/off,
// pat/on, pat/off in sequence". With no baseline cell in the plan (--resume, a
// filtered or partial run) and always under E2E_MATRIX=full, every bed serves
// the shared queue. A bed serving the shared queue takes the FIRST cell whose
// whole identity set is free right now, and waits only when none is; bed A's own
// queue is strict — its head or a wait.
//
// Failure (D8). A failing leg lets every other bed's RUNNING leg finish — more
// ledger coverage — but no new cell starts on any bed. A RUN INVALID leg
// (ExitBudgetExhausted) is narrower: its bed's records are voided (postSuiteTail)
// and the cell's ENGINE identity is marked exhausted, so cells that would charge
// it are not started, while cells on other identities continue. The exit code is
// the first failure's, by time; every bed's outcome is in the closing summary.
type MultiBedScheduler struct{}

// queued is one cell waiting to run.
type queued struct {
	cell Cell
	seq  int // plan position, for the summary
}

func cellDesc(c Cell) string {
	if c.Isolated {
		return c.Label() + " (isolated)"
	}
	return c.Label()
}

// holder is who holds an identity.
type holder struct{ bed, cell string }

// multiSched is one MultiBedScheduler.Run.
type multiSched struct {
	g    *Gate
	beds []*Gate

	mu        sync.Mutex
	wake      chan struct{} // closed and replaced on every release/stop: waiters rescan
	fixed     map[int][]*queued
	shared    []*queued
	serves    map[int]bool // beds that serve the shared queue
	held      map[string]holder
	exhausted map[string]bool
	stopNew   bool
	firstErr  error
	results   map[int][]string // per bed: "<cell> — <outcome>", in run order
	waitedFor map[string]bool  // "<bed>|<seq>|<identity>": each wait is logged once
}

// assignCells splits the plan into bed A's own queue and the shared queue (D6).
func assignCells(cells []Cell, sparse bool) (bedA, shared []Cell) {
	hasBaseline := false
	for _, c := range cells {
		if c.Auth == baselineAuth && c.Train == baselineTrain {
			hasBaseline = true
		}
	}
	if !sparse || !hasBaseline {
		return nil, cells
	}
	for _, c := range cells {
		if c.Auth == baselineAuth && c.Train == baselineTrain {
			bedA = append(bedA, c)
		} else {
			shared = append(shared, c)
		}
	}
	return bedA, shared
}

// describeAssignment is the one line printed after the plan: which bed runs what.
func describeAssignment(g *Gate, cells []Cell) string {
	bedA, shared := assignCells(cells, g.Cfg.sparseMatrix())
	descs := func(cs []Cell) string {
		if len(cs) == 0 {
			return "nothing"
		}
		parts := make([]string, len(cs))
		for i, c := range cs {
			parts[i] = cellDesc(c)
		}
		return strings.Join(parts, ", ")
	}
	beds := g.beds()
	if bedA != nil {
		others := make([]string, 0, len(beds)-1)
		for _, b := range beds[1:] {
			others = append(others, b.bed.Name)
		}
		return fmt.Sprintf("bed %s: %s; bed %s (in order, next free bed): %s", beds[0].bed.Name, descs(bedA), strings.Join(others, ", "), descs(shared))
	}
	return fmt.Sprintf("every bed (next free bed, in order): %s", descs(shared))
}

func (MultiBedScheduler) Run(ctx context.Context, g *Gate, cells []Cell) error {
	s := &multiSched{
		g: g, beds: g.beds(),
		wake:   make(chan struct{}),
		fixed:  map[int][]*queued{},
		serves: map[int]bool{},
		held:   map[string]holder{}, exhausted: map[string]bool{},
		results: map[int][]string{}, waitedFor: map[string]bool{},
	}
	bedA, shared := assignCells(cells, g.Cfg.sparseMatrix())
	seq := 0
	for _, c := range bedA {
		s.fixed[0] = append(s.fixed[0], &queued{cell: c, seq: seq})
		seq++
	}
	for _, c := range shared {
		s.shared = append(s.shared, &queued{cell: c, seq: seq})
		seq++
	}
	for i := range s.beds {
		s.serves[i] = bedA == nil || i > 0
	}

	// Beds start in order, each only once the previous one has claimed its first
	// cell or registered a wait: bed A — whose baseline dominates the wall-clock —
	// always gets first claim on an identity it shares with another bed.
	var wg sync.WaitGroup
	for i := range s.beds {
		ready := make(chan struct{})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.bedLoop(ctx, i, ready)
		}(i)
		<-ready
	}
	wg.Wait()

	s.summarize(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.firstErr
}

// broadcast wakes every waiting bed. Called with s.mu held.
func (s *multiSched) broadcast() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// pending is one wait to report after s.mu is released.
type pending struct {
	bed      string
	cell     Cell
	identity string
}

// next picks this bed's next runnable cell and acquires its identity set, or
// reports what to wait on, or that the bed is done.
func (s *multiSched) next(ctx context.Context, i int) (q *queued, set []Identity, wait <-chan struct{}, done bool, hooks []pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.beds[i]
	if s.stopNew || ctx.Err() != nil {
		return nil, nil, nil, true, nil
	}
	exhausted := func(set []Identity) bool {
		for _, id := range set {
			if s.exhausted[id.Key] {
				return true
			}
		}
		return false
	}
	// blockedBy is the first identity of set someone else holds ("" if none).
	blockedBy := func(set []Identity) string {
		for _, id := range set {
			if _, busy := s.held[id.Key]; busy {
				return id.Key
			}
		}
		return ""
	}
	acquire := func(q *queued, set []Identity) {
		for _, id := range set {
			s.held[id.Key] = holder{bed: b.bed.Name, cell: cellDesc(q.cell)}
		}
	}
	noteWait := func(q *queued, key string) {
		k := fmt.Sprintf("%s|%d|%s", b.bed.Name, q.seq, key)
		if s.waitedFor[k] {
			return
		}
		s.waitedFor[k] = true
		h := s.held[key]
		b.outf("== waiting: %s on bed %s needs %s, held by bed %s (%s) ==\n", cellDesc(q.cell), b.bed.Name, key, h.bed, h.cell)
		hooks = append(hooks, pending{bed: b.bed.Name, cell: q.cell, identity: key})
	}

	// Bed A's own queue is strict: its head, or a wait.
	for len(s.fixed[i]) > 0 {
		head := s.fixed[i][0]
		hs := b.identitySet(head.cell.Auth)
		if exhausted(hs) {
			s.fixed[i] = s.fixed[i][1:]
			s.results[i] = append(s.results[i], fmt.Sprintf("%s — not started (%s)", cellDesc(head.cell), s.exhaustedReason(hs)))
			continue
		}
		if key := blockedBy(hs); key != "" {
			noteWait(head, key)
			return nil, nil, s.wake, false, hooks
		}
		s.fixed[i] = s.fixed[i][1:]
		acquire(head, hs)
		return head, hs, nil, false, hooks
	}
	if !s.serves[i] {
		return nil, nil, nil, true, nil
	}

	// The shared queue: the first cell whose whole set is free right now. Waits
	// are logged only when the bed actually waits, i.e. nothing was free.
	type blocked struct {
		q   *queued
		key string
	}
	var waits []blocked
	for k, cq := range s.shared {
		cs := b.identitySet(cq.cell.Auth)
		if exhausted(cs) {
			continue // never on this bed; another bed may still take it
		}
		if key := blockedBy(cs); key != "" {
			waits = append(waits, blocked{cq, key})
			continue
		}
		s.shared = append(s.shared[:k:k], s.shared[k+1:]...)
		acquire(cq, cs)
		return cq, cs, nil, false, hooks
	}
	if len(waits) == 0 {
		return nil, nil, nil, true, hooks // nothing this bed could ever run
	}
	for _, w := range waits {
		noteWait(w.q, w.key)
	}
	return nil, nil, s.wake, false, hooks
}

// exhaustedReason names the exhausted identities of set. Called with s.mu held.
func (s *multiSched) exhaustedReason(set []Identity) string {
	var keys []string
	for _, id := range set {
		if s.exhausted[id.Key] {
			keys = append(keys, id.Key)
		}
	}
	return "identity " + strings.Join(keys, ", ") + " budget exhausted (RUN INVALID)"
}

// finish releases a cell's identities and applies D8 to its result.
func (s *multiSched) finish(ctx context.Context, i int, q *queued, set []Identity, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.broadcast()
	for _, id := range set {
		delete(s.held, id.Key)
	}
	b := s.beds[i]
	outcome := "passed"
	var ee *ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		outcome = "cancelled"
		s.stopNew = true
	case errors.As(err, &ee) && ee.Code == ExitBudgetExhausted:
		engine, _ := b.engineIdentity(q.cell.Auth)
		s.exhausted[engine.Key] = true
		outcome = fmt.Sprintf("exit %d (RUN INVALID: %s budget exhausted; this cell's records voided)", ee.Code, engine.Key)
	case errors.As(err, &ee):
		outcome = fmt.Sprintf("exit %d", ee.Code)
		s.stopNew = true
	default:
		outcome = fmt.Sprintf("error: %v", err)
		s.stopNew = true
	}
	s.results[i] = append(s.results[i], cellDesc(q.cell)+" — "+outcome)
	if err != nil && ctx.Err() == nil && s.firstErr == nil {
		s.firstErr = err // D8: the first failure by time decides the exit code
	}
}

// bedLoop runs bed i's cells until it has none it can run.
func (s *multiSched) bedLoop(ctx context.Context, i int, ready chan struct{}) {
	b := s.beds[i]
	var once sync.Once
	signal := func() { once.Do(func() { close(ready) }) }
	defer signal()
	defer b.flushOutput()
	lastAuth := ""
	for {
		q, set, wait, done, hooks := s.next(ctx, i)
		for _, h := range hooks {
			if s.g.schedWaitHook != nil {
				s.g.schedWaitHook(h.bed, h.cell, h.identity)
			}
		}
		if done {
			return
		}
		signal()
		if q == nil {
			select {
			case <-wait:
			case <-ctx.Done():
				return
			}
			continue
		}
		if q.cell.Auth != lastAuth {
			b.outf("== auth leg: %s ==\n", q.cell.Auth)
			lastAuth = q.cell.Auth
		}
		s.runOne(ctx, i, q, set)
	}
}

// runOne runs one leg and releases its identities on every exit path — a panic
// included, which is re-raised once the other beds can no longer block on it.
func (s *multiSched) runOne(ctx context.Context, i int, q *queued, set []Identity) {
	var err error
	finished := false
	defer func() {
		if !finished {
			s.finish(ctx, i, q, set, fmt.Errorf("leg %s panicked", cellDesc(q.cell)))
		}
	}()
	err = s.beds[i].runLeg(ctx, q.cell)
	finished = true
	s.finish(ctx, i, q, set, err)
}

// summarize prints every bed's outcomes and every cell that never started.
func (s *multiSched) summarize(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var left []string
	reason := "not started"
	switch {
	case ctx.Err() != nil:
		reason = "not started (cancelled)"
	case s.stopNew:
		reason = "not started (a leg failed — no new cell starts after a failure)"
	}
	for i, qs := range s.fixed {
		for _, q := range qs {
			s.results[i] = append(s.results[i], cellDesc(q.cell)+" — "+reason)
		}
	}
	for _, q := range s.shared {
		r := reason
		if !s.stopNew && ctx.Err() == nil {
			// Left over only because every bed that could take it found an
			// identity of its set exhausted.
			seen := map[string]bool{}
			var keys []string
			for i, b := range s.beds {
				if !s.serves[i] {
					continue
				}
				for _, id := range b.identitySet(q.cell.Auth) {
					if s.exhausted[id.Key] && !seen[id.Key] {
						seen[id.Key] = true
						keys = append(keys, id.Key)
					}
				}
			}
			r = "not started (identity " + strings.Join(keys, ", ") + " budget exhausted (RUN INVALID))"
		}
		left = append(left, cellDesc(q.cell)+" — "+r)
	}
	g := s.g
	g.outln("== multi-bed summary ==")
	for i, b := range s.beds {
		res := s.results[i]
		if len(res) == 0 {
			res = []string{"no cells"}
		}
		g.outf("   bed %s (%s): %s\n", b.bed.Name, b.bed.Dir, strings.Join(res, "; "))
	}
	sort.Strings(left)
	for _, l := range left {
		g.outf("   %s\n", l)
	}
	if len(s.exhausted) > 0 {
		var keys []string
		for k := range s.exhausted {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		g.outf("   exhausted identities: %s\n", strings.Join(keys, ", "))
	}
}
