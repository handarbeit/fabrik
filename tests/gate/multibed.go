package gate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MultiBedScheduler runs a plan's cells across two or more beds at once (#1976,
// ADR-1976). One goroutine per bed pulls that bed's next cell, and every cell
// first acquires — atomically, all or nothing — the set of GitHub identities it
// charges (identity.go). Two legs on one identity therefore never overlap: the
// later one waits, and says so. A single-bed run never uses it (R6): it keeps
// SerialScheduler and today's failure semantics.
//
// Assignment (D6). Under the sparse matrix, when the plan holds a baseline
// cell (app/on), bed A runs exactly that cell and nothing else; the other beds serve a shared queue of
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

func cellDesc(c Cell) string { return c.Label() }

// holder is who holds an identity.
type holder struct{ bed, cell string }

// use is how a cell holds one identity: an exclusive engine (an App
// installation, or a pat engine with sharing disabled), a pat engine that shares
// its user with other beds' harness use (Identity.SharesHarness), or harness.
type use int

const (
	useHarness use = iota
	useSharedEngine
	useExclusive
)

func useOf(id Identity) use {
	switch {
	case id.Role != RoleEngine:
		return useHarness
	case id.SharesHarness:
		return useSharedEngine
	default:
		return useExclusive
	}
}

// conflicts says whether two uses of one identity by different beds cannot
// overlap: an exclusive use excludes everything, and two engines never poll as
// one user. Every other pair shares, above the budget floor.
func conflicts(a, b use) bool {
	return a == useExclusive || b == useExclusive || (a == useSharedEngine && b == useSharedEngine)
}

// idHold is everyone holding one identity (#2263): an EXCLUSIVE engine holder
// (nobody else holds the key in any role), or at most one SHARED engine holder (a
// pat engine on the bed token's user) plus any number of HARNESS holders, all
// above the budget floor.
type idHold struct {
	excl   *holder
	eng    *holder
	shared []holder
}

// resv is a strict-queue head's reservation of an identity, in the use the head
// will hold it: it blocks another bed's use only where the two conflict, so
// harness sharing is not stopped by it.
type resv struct {
	bed int
	use use
}

// reading is one budget probe of a shared identity, valid only for the epoch it
// was taken in: the epoch moves on every acquire and release, so a decision
// always rests on a reading taken after the last state change and one reading
// admits at most one join.
type reading struct {
	remaining, limit int
	resetAt          string
	err              error
	epoch            uint64
}

// probeReq is one identity whose budget next() needs read, outside s.mu.
type probeReq struct {
	id    Identity
	epoch uint64
}

// Wait reasons (pending.reason, schedWaitHook).
const (
	waitExclusive = "exclusive"
	waitReserved  = "reserved"
	waitFloor     = "floor"
)

// multiSched is one MultiBedScheduler.Run.
type multiSched struct {
	g    *Gate
	beds []*Gate

	mu     sync.Mutex
	wake   chan struct{} // closed and replaced on every release/stop: waiters rescan
	fixed  map[int][]*queued
	shared []*queued
	serves map[int]bool // beds that serve the shared queue
	held   map[string]*idHold
	// reserved maps an identity to the bed whose strict-queue head is waiting
	// for it: a shared-queue bed may not take it first, so bed A's baseline is
	// never starved by a bed that keeps re-acquiring a shared identity. The
	// reservation carries the head's role (resv).
	reserved  map[string]resv
	exhausted map[string]bool
	stopNew   bool
	firstErr  error
	results   map[int][]string // per bed: "<cell> — <outcome>", in run order
	waitedFor map[string]bool  // "<bed>|<seq>|<identity>|<reason>|<detail>": each distinct wait is logged once

	// budget is the latest probe per shared identity; epoch versions it (reading).
	budget map[string]reading
	epoch  uint64

	// afterFinish, if set, observes each leg once its result has been applied
	// (tests sequence completions on it).
	afterFinish func(bed string, c Cell)
}

// assignCells splits the plan into bed A's own queue and the shared queue (D6).
func assignCells(cells []Cell, sparse bool) (bedA, shared []Cell) {
	hasBaseline := false
	for _, c := range cells {
		if c.isBaseline() {
			hasBaseline = true
		}
	}
	if !sparse || !hasBaseline {
		return nil, cells
	}
	for _, c := range cells {
		if c.isBaseline() {
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
	return newMultiSched(g, cells).run(ctx)
}

// newMultiSched queues the plan per D6.
func newMultiSched(g *Gate, cells []Cell) *multiSched {
	s := &multiSched{
		g: g, beds: g.beds(),
		wake:   make(chan struct{}),
		fixed:  map[int][]*queued{},
		serves: map[int]bool{},
		held:   map[string]*idHold{}, reserved: map[string]resv{}, exhausted: map[string]bool{},
		results: map[int][]string{}, waitedFor: map[string]bool{}, budget: map[string]reading{},
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
	return s
}

func (s *multiSched) run(ctx context.Context) error {
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
	reason   string
}

// decision is what next() tells a bed's loop: a cell to run (q, set), identities
// to probe first (probe), something to wait on (wait; reprobe says the wait is on
// the budget floor, so the loop also wakes on a timer), or that the bed is done.
type decision struct {
	q       *queued
	set     []Identity
	wait    <-chan struct{}
	done    bool
	hooks   []pending
	probe   []probeReq
	reprobe bool
}

// verdict is whether one cell's identity set can be acquired right now.
type verdict struct {
	ok     bool
	key    string // the identity it is blocked on
	reason string // waitExclusive, waitReserved or waitFloor
	text   string // the wait line's predicate
	probe  []probeReq
}

// floorOn says whether the shared-identity budget floor applies at all: it needs
// a floor, and a way to read a budget.
func (s *multiSched) floorOn() bool {
	return s.g.Cfg.SharedIdentityFloor > 0 && s.g.Identity != nil
}

func (s *multiSched) reprobeInterval() time.Duration {
	if d := s.g.Cfg.SharedIdentityReprobe; d > 0 {
		return d
	}
	return 5 * time.Minute
}

// check decides whether bed i can take set now. Called with s.mu held; it does
// no I/O — an identity whose budget must be read first comes back as probe.
func (s *multiSched) check(i int, set []Identity) verdict {
	var needFloor []Identity
	for _, id := range set {
		u := useOf(id)
		h := s.held[id.Key]
		if h != nil {
			if h.excl != nil {
				return verdict{key: id.Key, reason: waitExclusive, text: s.heldText(id, *h.excl, useExclusive)}
			}
			if h.eng != nil && conflicts(u, useSharedEngine) {
				return verdict{key: id.Key, reason: waitExclusive, text: s.heldText(id, *h.eng, useSharedEngine)}
			}
			if u == useExclusive && len(h.shared) > 0 {
				return verdict{key: id.Key, reason: waitExclusive, text: s.heldText(id, h.shared[0], useHarness)}
			}
		}
		if r, ok := s.reserved[id.Key]; ok && r.bed != i && conflicts(u, r.use) {
			return verdict{key: id.Key, reason: waitReserved, text: fmt.Sprintf("needs %s%s, reserved for bed %s's next cell", id.Key, usesLabel(u), s.beds[r.bed].bed.Name)}
		}
		if u != useExclusive && h != nil && (h.eng != nil || len(h.shared) > 0) && s.floorOn() {
			needFloor = append(needFloor, id)
		}
	}
	var probes []probeReq
	for _, id := range needFloor {
		r, ok := s.budget[id.Key]
		if !ok || r.epoch != s.epoch {
			probes = append(probes, probeReq{id: id, epoch: s.epoch})
		}
	}
	if len(probes) > 0 {
		return verdict{probe: probes}
	}
	for _, id := range needFloor {
		r := s.budget[id.Key]
		h := s.held[id.Key]
		other, otherUse := holder{}, useHarness
		if h.eng != nil {
			other, otherUse = *h.eng, useSharedEngine
		} else {
			other = h.shared[0]
		}
		floor := s.g.Cfg.SharedIdentityFloor
		switch {
		case r.err != nil:
			return verdict{key: id.Key, reason: waitFloor, text: fmt.Sprintf("shares %s (%s) with bed %s (%s, %s), but its budget could not be read (%v); re-probing when a leg finishes or in %s", id.Key, useName(useOf(id)), other.bed, other.cell, useName(otherUse), r.err, s.reprobeInterval())}
		case r.remaining*100 < r.limit*floor:
			return verdict{key: id.Key, reason: waitFloor, text: fmt.Sprintf("shares %s (%s) with bed %s (%s, %s), but its budget is below the floor: %d of %d remaining (floor %d%%), resets %s; re-probing when a leg finishes or in %s", id.Key, useName(useOf(id)), other.bed, other.cell, useName(otherUse), r.remaining, r.limit, floor, r.resetAt, s.reprobeInterval())}
		}
	}
	return verdict{ok: true}
}

func useName(u use) string {
	switch u {
	case useExclusive:
		return "engine"
	case useSharedEngine:
		return "pat engine"
	default:
		return "harness"
	}
}

func usesLabel(u use) string {
	if u == useExclusive {
		return " exclusively"
	}
	return " (" + useName(u) + ")"
}

// heldText is the wait line's predicate for an identity another bed holds in
// use by.
func (s *multiSched) heldText(id Identity, by holder, byUse use) string {
	if useOf(id) == useExclusive {
		role := ""
		switch byUse {
		case useHarness:
			role = " as harness"
		case useSharedEngine:
			role = " as pat engine"
		}
		return fmt.Sprintf("needs %s exclusively, held by bed %s (%s)%s", id.Key, by.bed, by.cell, role)
	}
	how := "exclusively"
	if byUse == useSharedEngine {
		how = "as pat engine"
	}
	return fmt.Sprintf("needs %s%s, held %s by bed %s (%s)", id.Key, usesLabel(useOf(id)), how, by.bed, by.cell)
}

// acquire takes every identity of set for bed i's cell q, each in its own role.
// Called with s.mu held.
func (s *multiSched) acquire(i int, q *queued, set []Identity) {
	me := holder{bed: s.beds[i].bed.Name, cell: cellDesc(q.cell)}
	for _, id := range set {
		h := s.held[id.Key]
		if h == nil {
			h = &idHold{}
			s.held[id.Key] = h
		}
		switch useOf(id) {
		case useExclusive:
			h.excl = &me
		case useSharedEngine:
			h.eng = &me
		default:
			h.shared = append(h.shared, me)
		}
	}
	s.epoch++
}

// release drops exactly the holds bed i's cell acquired, each in its own role.
// Called with s.mu held.
func (s *multiSched) release(i int, set []Identity) {
	name := s.beds[i].bed.Name
	for _, id := range set {
		h := s.held[id.Key]
		if h == nil {
			continue
		}
		switch useOf(id) {
		case useExclusive:
			if h.excl != nil && h.excl.bed == name {
				h.excl = nil
			}
		case useSharedEngine:
			if h.eng != nil && h.eng.bed == name {
				h.eng = nil
			}
		default:
			for k, sh := range h.shared {
				if sh.bed == name {
					h.shared = append(h.shared[:k:k], h.shared[k+1:]...)
					break
				}
			}
		}
		if h.excl == nil && h.eng == nil && len(h.shared) == 0 {
			delete(s.held, id.Key)
		}
	}
	s.epoch++
}

// next picks this bed's next runnable cell and acquires its identity set, or
// reports what to probe or wait on, or that the bed is done.
func (s *multiSched) next(ctx context.Context, i int) decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.beds[i]
	var hooks []pending
	if s.stopNew || ctx.Err() != nil {
		return decision{done: true}
	}
	exhausted := func(set []Identity) bool {
		for _, id := range set {
			if s.exhausted[id.Key] {
				return true
			}
		}
		return false
	}
	// unreserve drops this bed's reservations, waking every waiter if it dropped
	// any: a bed blocked only by a reservation must rescan, or it would sleep
	// until some unrelated leg finished.
	unreserve := func() {
		dropped := false
		for k, r := range s.reserved {
			if r.bed == i {
				delete(s.reserved, k)
				dropped = true
			}
		}
		if dropped {
			s.broadcast()
		}
	}
	noteWait := func(q *queued, v verdict) {
		k := fmt.Sprintf("%s|%d|%s|%s|%s", b.bed.Name, q.seq, v.key, v.reason, v.text)
		if s.waitedFor[k] {
			return
		}
		s.waitedFor[k] = true
		b.outf("== waiting: %s on bed %s %s ==\n", cellDesc(q.cell), b.bed.Name, v.text)
		hooks = append(hooks, pending{bed: b.bed.Name, cell: q.cell, identity: v.key, reason: v.reason})
	}

	// Bed A's own queue is strict: its head, or a wait.
	for len(s.fixed[i]) > 0 {
		head := s.fixed[i][0]
		hs := b.identitySet(head.cell.Auth)
		if exhausted(hs) {
			unreserve()
			s.fixed[i] = s.fixed[i][1:]
			s.results[i] = append(s.results[i], fmt.Sprintf("%s — not started (%s)", cellDesc(head.cell), s.exhaustedReason(hs)))
			continue
		}
		v := s.check(i, hs)
		if len(v.probe) > 0 {
			return decision{probe: v.probe, hooks: hooks}
		}
		if !v.ok {
			for _, id := range hs {
				s.reserved[id.Key] = resv{bed: i, use: useOf(id)}
			}
			noteWait(head, v)
			return decision{wait: s.wake, hooks: hooks, reprobe: v.reason == waitFloor}
		}
		unreserve()
		s.fixed[i] = s.fixed[i][1:]
		s.acquire(i, head, hs)
		return decision{q: head, set: hs, hooks: hooks}
	}
	unreserve()
	if !s.serves[i] {
		return decision{done: true}
	}

	// The shared queue: the first cell whose whole set is free right now. Waits
	// are logged only when the bed actually waits, i.e. nothing was free.
	type blocked struct {
		q *queued
		v verdict
	}
	var waits []blocked
	for k, cq := range s.shared {
		if cq.cell.Webhook() && !b.bedWebhookCapable() {
			continue // a webhook cell runs only on a webhook-capable bed (#2426); another bed may take it
		}
		cs := b.identitySet(cq.cell.Auth)
		if exhausted(cs) {
			continue // never on this bed; another bed may still take it
		}
		v := s.check(i, cs)
		if len(v.probe) > 0 {
			return decision{probe: v.probe, hooks: hooks}
		}
		if !v.ok {
			waits = append(waits, blocked{cq, v})
			continue
		}
		s.shared = append(s.shared[:k:k], s.shared[k+1:]...)
		s.acquire(i, cq, cs)
		return decision{q: cq, set: cs, hooks: hooks}
	}
	if len(waits) == 0 {
		return decision{done: true, hooks: hooks} // nothing this bed could ever run
	}
	reprobe := false
	for _, w := range waits {
		noteWait(w.q, w.v)
		reprobe = reprobe || w.v.reason == waitFloor
	}
	return decision{wait: s.wake, hooks: hooks, reprobe: reprobe}
}

// probeShared reads the budget of each identity next() asked about, outside
// s.mu, and records it under the epoch it was requested in. An unreadable budget
// is recorded as such: it counts as below the floor for a second holder, never as
// a failure.
func (s *multiSched) probeShared(ctx context.Context, reqs []probeReq) {
	seen := map[string]bool{}
	for _, req := range reqs {
		if seen[req.id.Key] {
			continue
		}
		seen[req.id.Key] = true
		pctx, cancel := ctx, context.CancelFunc(func() {})
		if t := s.g.Cfg.GHAPITimeout; t > 0 {
			pctx, cancel = context.WithTimeout(ctx, t)
		}
		var r reading
		if req.id.token == "" {
			r.err = fmt.Errorf("no token for %s", req.id.Key)
		} else {
			r.remaining, r.limit, r.resetAt, r.err = s.g.Identity.Budget(pctx, req.id.token)
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		r.epoch = req.epoch
		s.mu.Lock()
		s.budget[req.id.Key] = r
		s.mu.Unlock()
	}
}

// expireReadings invalidates every probe, so the next scan re-reads the budgets
// (the re-probe timer).
func (s *multiSched) expireReadings() {
	s.mu.Lock()
	s.epoch++
	s.mu.Unlock()
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
	s.release(i, set)
	b := s.beds[i]
	outcome := "passed"
	var ee *ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		outcome = "cancelled"
		s.stopNew = true
	case errors.As(err, &ee) && ee.Code == ExitBudgetExhausted:
		outcome = fmt.Sprintf("exit %d (RUN INVALID; this cell's records voided)", ee.Code)
		if engine, ok := b.engineIdentity(q.cell.Auth); ok {
			s.exhausted[engine.Key] = true
			outcome = fmt.Sprintf("exit %d (RUN INVALID: %s budget exhausted; this cell's records voided)", ee.Code, engine.Key)
		} else {
			// No identity to scope it to (no token: CheckBedTopology makes this
			// unreachable on a real run) — fail safe and start nothing new.
			s.stopNew = true
		}
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
	idle := false // this bed's engine was stopped and no leg has restarted it since
	for {
		d := s.next(ctx, i)
		for _, h := range d.hooks {
			if s.g.schedWaitHook != nil {
				s.g.schedWaitHook(h.bed, h.cell, h.identity, h.reason)
			}
		}
		if d.done {
			if ctx.Err() == nil && !idle {
				b.stopIdleEngine("it has no more cells to run")
			}
			return
		}
		signal()
		if len(d.probe) > 0 {
			s.probeShared(ctx, d.probe)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		q, set := d.q, d.set
		if q == nil {
			// A waiting bed's engine would keep polling with an identity another
			// bed's leg may be holding: stop it until this bed's next leg.
			if !idle {
				b.stopIdleEngine("it waits for an identity")
				idle = true
			}
			// A bed waiting on the budget floor also wakes on a timer: the budget
			// recovers with time, not only when a leg finishes.
			var timer <-chan time.Time
			var t *time.Timer
			if d.reprobe {
				t = time.NewTimer(s.reprobeInterval())
				timer = t.C
			}
			select {
			case <-d.wait:
			case <-timer:
				s.expireReadings()
			case <-ctx.Done():
				if t != nil {
					t.Stop()
				}
				return
			}
			if t != nil {
				t.Stop()
			}
			continue
		}
		if q.cell.Auth != lastAuth {
			b.outf("== auth leg: %s ==\n", q.cell.Auth)
			lastAuth = q.cell.Auth
		}
		s.runOne(ctx, i, q, set)
		// runOne stopped the engine the leg's restart step had started; on a
		// cancel it did not, and the loop ends at the next pass anyway.
		idle = ctx.Err() == nil
	}
}

// runOne runs one leg and releases its identities on every exit path: a leg
// that panics still releases them (and stops new cells) before the panic
// propagates, so no other bed is left waiting on an identity nobody holds.
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
	// Stop the engine BEFORE finish releases the identities: otherwise another
	// bed's leg could start on one of them while this engine still polls with it
	// (#1684's shape, between two beds). The next leg's restart starts it again.
	if ctx.Err() == nil {
		s.beds[i].stopIdleEngine("its leg's identities are released")
	}
	s.finish(ctx, i, q, set, err)
	if s.afterFinish != nil {
		s.afterFinish(s.beds[i].bed.Name, q.cell)
	}
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
