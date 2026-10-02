package gate

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// schedHarness drives MultiBedScheduler over two beds whose legs are scripted:
// a leg reports that it started, then blocks until the test finishes it. It
// tracks how many legs hold each identity at once. Synchronisation is by
// channel — the only timeout is a failsafe against a hung test.
type schedHarness struct {
	t *testing.T
	g *Gate

	mu        sync.Mutex
	finishers map[string]chan error // "<bed> <cell>" -> its result
	active    map[string]int
	maxActive map[string]int
	ran       []string

	started  chan string
	waits    chan string
	finished chan string
}

const failsafe = 10 * time.Second

// newSchedHarness: bed A logs in as loginA with App 77, bed B as loginB with App
// 88. Equal logins make every cell of the two beds share the harness identity.
func newSchedHarness(t *testing.T, loginA, loginB string, matrix string) *schedHarness {
	t.Helper()
	g, _, _, _ := twoBedGate(t)
	g.Cfg.Matrix = matrix
	h := &schedHarness{
		t: t, g: g,
		finishers: map[string]chan error{}, active: map[string]int{}, maxActive: map[string]int{},
		started: make(chan string, 32), waits: make(chan string, 32), finished: make(chan string, 32),
	}
	g.schedWaitHook = func(bed string, c Cell, id string) { h.waits <- bed + " " + cellDesc(c) + " " + id }
	for i, b := range g.beds() {
		b.schedWaitHook = g.schedWaitHook
		b.login = []string{loginA, loginB}[i]
		b.bed.AppInstallationID = []string{"77", "88"}[i]
		b := b
		b.runLegFn = func(ctx context.Context, c Cell) error { return h.leg(ctx, b, c) }
	}
	return h
}

func (h *schedHarness) finisher(key string) chan error {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.finishers[key]
	if !ok {
		ch = make(chan error, 1)
		h.finishers[key] = ch
	}
	return ch
}

func (h *schedHarness) leg(ctx context.Context, b *Gate, c Cell) error {
	key := b.bed.Name + " " + cellDesc(c)
	set := b.identitySet(c.Auth)
	h.mu.Lock()
	h.ran = append(h.ran, key)
	for _, id := range set {
		h.active[id.Key]++
		if h.active[id.Key] > h.maxActive[id.Key] {
			h.maxActive[id.Key] = h.active[id.Key]
		}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		for _, id := range set {
			h.active[id.Key]--
		}
		h.mu.Unlock()
	}()
	h.started <- key
	select {
	case err := <-h.finisher(key):
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// expectStart waits for the next leg to start and checks it is want.
func (h *schedHarness) expectStart(want string) {
	h.t.Helper()
	select {
	case got := <-h.started:
		if got != want {
			h.t.Fatalf("started %q, want %q", got, want)
		}
	case <-time.After(failsafe):
		h.t.Fatalf("%q never started", want)
	}
}

// expectStarts is expectStart for legs that start concurrently, in any order.
func (h *schedHarness) expectStarts(want ...string) {
	h.t.Helper()
	pending := map[string]bool{}
	for _, w := range want {
		pending[w] = true
	}
	for range want {
		select {
		case got := <-h.started:
			if !pending[got] {
				h.t.Fatalf("started %q, want one of %v", got, want)
			}
			delete(pending, got)
		case <-time.After(failsafe):
			h.t.Fatalf("never started: %v", pending)
		}
	}
}

// expectWait waits for the scheduler to log a wait and checks it is want.
func (h *schedHarness) expectWait(want string) {
	h.t.Helper()
	select {
	case got := <-h.waits:
		if got != want {
			h.t.Fatalf("waited %q, want %q", got, want)
		}
	case <-time.After(failsafe):
		h.t.Fatalf("no wait %q", want)
	}
}

func (h *schedHarness) noStart() {
	h.t.Helper()
	select {
	case got := <-h.started:
		h.t.Fatalf("%q started, but nothing may", got)
	default:
	}
}

// finish ends a running leg and waits until the scheduler has applied its
// result, so the next step sees it.
func (h *schedHarness) finish(key string, err error) {
	h.t.Helper()
	h.finisher(key) <- err
	select {
	case got := <-h.finished:
		if got != key {
			h.t.Fatalf("finished %q, want %q", got, key)
		}
	case <-time.After(failsafe):
		h.t.Fatalf("%q's result was never applied", key)
	}
}

// run starts the scheduler; the returned channel yields its result.
func (h *schedHarness) run(ctx context.Context, cells []Cell) (*multiSched, <-chan error) {
	s := newMultiSched(h.g, cells)
	s.afterFinish = func(bed string, c Cell) { h.finished <- bed + " " + cellDesc(c) }
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	return s, done
}

func (h *schedHarness) result(done <-chan error) error {
	h.t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(failsafe):
		h.t.Fatal("the scheduler never returned")
		return nil
	}
}

func (h *schedHarness) out() string { return h.g.Out.(interface{ String() string }).String() }

var (
	cAppOn    = Cell{Auth: "app", Train: "on"}
	cAppOnIso = Cell{Auth: "app", Train: "on", Isolated: true}
	cAppOff   = Cell{Auth: "app", Train: "off"}
	cPatOn    = Cell{Auth: "pat", Train: "on"}
	cPatOff   = Cell{Auth: "pat", Train: "off"}
	sparseAll = []Cell{cAppOn, cAppOnIso, cAppOff, cPatOn, cPatOff}
)

func TestAssignCells(t *testing.T) {
	a, shared := assignCells(sparseAll, true)
	if len(a) != 2 || a[0].Label() != "app/on" || a[0].Isolated || !a[1].Isolated {
		t.Errorf("bed A: %v", a)
	}
	if len(shared) != 3 || shared[0].Label() != "app/off" || shared[1].Label() != "pat/on" || shared[2].Label() != "pat/off" {
		t.Errorf("shared: %v", shared)
	}
	if a, shared := assignCells([]Cell{cAppOff, cPatOn}, true); a != nil || len(shared) != 2 {
		t.Error("no baseline in the plan: every bed serves the shared queue")
	}
	if a, shared := assignCells(sparseAll, false); a != nil || len(shared) != 5 {
		t.Error("E2E_MATRIX=full: every bed serves the shared queue")
	}
}

// D6/D7: the baseline (main, then isolated) runs on bed A while bed B runs the
// other cells in sparse order — concurrently, since no identity is shared.
func TestMultiBedDefaultAssignmentRunsConcurrently(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	_, done := h.run(context.Background(), sparseAll)
	h.expectStarts("A app/on", "B app/off") // both running at once: disjoint identities
	h.finish("B app/off", nil)
	h.expectStart("B pat/on")
	h.finish("A app/on", nil)
	h.expectStart("A app/on (isolated)")
	h.finish("B pat/on", nil)
	h.expectStart("B pat/off")
	h.finish("B pat/off", nil)
	h.finish("A app/on (isolated)", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	out := h.out()
	if strings.Contains(out, "== waiting:") {
		t.Errorf("disjoint identities never wait:\n%s", out)
	}
	for _, want := range []string{
		"[bed A] == auth leg: app ==",
		"[bed B] == auth leg: pat ==",
		"== multi-bed summary ==",
		"bed A (" + h.g.Cfg.BedDirs[0] + "): app/on — passed; app/on (isolated) — passed",
		"bed B (" + h.g.Cfg.BedDirs[1] + "): app/off — passed; pat/on — passed; pat/off — passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// R3 / D4: two cells sharing an identity (here the harness login both beds'
// tokens resolve to) never overlap; the later one waits, and the wait is logged.
// Bed A gets first claim.
func TestMultiBedSerializesASharedIdentity(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	_, done := h.run(context.Background(), sparseAll)
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand")
	h.noStart()
	if !strings.Contains(h.out(), "[bed B] == waiting: app/off on bed B needs user:arbeithand, held by bed A (app/on) ==") {
		t.Errorf("the wait must be logged naming the identity and its holder:\n%s", h.out())
	}
	// Drive the rest: finish whatever starts. Never two at once on the login.
	h.finish("A app/on", nil)
	for n := 0; n < 4; n++ {
		select {
		case key := <-h.started:
			h.mu.Lock()
			concurrent := h.maxActive["user:arbeithand"]
			h.mu.Unlock()
			if concurrent > 1 {
				t.Fatalf("user:arbeithand held by %d legs at once", concurrent)
			}
			h.finish(key, nil)
		case <-time.After(failsafe):
			t.Fatalf("stalled after %d more legs; ran %v", n, h.ran)
		}
	}
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if h.maxActive["user:arbeithand"] != 1 || len(h.ran) != 5 {
		t.Errorf("max concurrent=%d ran=%v", h.maxActive["user:arbeithand"], h.ran)
	}
}

// D6's greedy fallback: no baseline in the plan (a --resume or partial run), so
// every bed serves one queue in order.
func TestMultiBedGreedyFallbackWithoutBaseline(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	_, done := h.run(context.Background(), []Cell{cAppOff, cPatOn, cPatOff})
	h.expectStarts("A app/off", "B pat/on")
	h.finish("B pat/on", nil)
	h.expectStart("B pat/off") // bed B is free first, so it takes the next cell
	h.finish("A app/off", nil)
	h.finish("B pat/off", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// E2E_MATRIX=full: two PAT cells on beds whose tokens are the same user (D3:
// a PAT identity is its login) serialize instead of overlapping.
func TestMultiBedFullMatrixSerializesTwoPatCells(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixFull)
	_, done := h.run(context.Background(), []Cell{cPatOff, cPatOn})
	h.expectStart("A pat/off")
	h.expectWait("B pat/on user:arbeithand")
	h.noStart()
	h.finish("A pat/off", nil)
	select {
	case key := <-h.started:
		h.finish(key, nil)
	case <-time.After(failsafe):
		t.Fatal("pat/on never started")
	}
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if h.maxActive["user:arbeithand"] != 1 {
		t.Errorf("the two PAT cells overlapped")
	}
}

// A bed serving the shared queue takes the first cell whose identity set is free
// now rather than idling behind a blocked one.
func TestMultiBedFreeBedSkipsABlockedCell(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixFull)
	s := newMultiSched(h.g, []Cell{cAppOff, cPatOn})
	s.held["app:88"] = holder{bed: "X", cell: "elsewhere"} // bed B's App is busy
	q, set, wait, done, _ := s.next(context.Background(), 1)
	if done || wait != nil || q == nil || q.cell.Label() != "pat/on" || len(set) != 1 || set[0].Key != "user:bob" {
		t.Fatalf("bed B must skip the blocked app/off for pat/on: q=%v set=%v wait=%v done=%v", q, set, wait != nil, done)
	}
	// With pat/on taken, bed B now has only a blocked cell: it waits.
	if _, _, wait, done, hooks := s.next(context.Background(), 1); done || wait == nil || len(hooks) != 1 || hooks[0].identity != "app:88" {
		t.Errorf("only the blocked app/off is left, so bed B waits on its App: wait=%v done=%v hooks=%v", wait != nil, done, hooks)
	}
}

// D8: a failure lets the other bed's RUNNING leg finish, but no new cell starts
// on either bed.
func TestMultiBedFailureStopsNewCells(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	_, done := h.run(context.Background(), sparseAll)
	h.expectStarts("A app/on", "B app/off")
	h.finish("B app/off", &ExitError{Code: 1})
	h.noStart()
	h.finish("A app/on", nil) // still allowed to finish
	err := h.result(done)
	if exitCode(err) != 1 {
		t.Fatalf("got %v", err)
	}
	if len(h.ran) != 2 {
		t.Errorf("no new cell may start after a failure: %v", h.ran)
	}
	out := h.out()
	for _, want := range []string{"app/on — passed; app/on (isolated) — not started (a leg failed", "app/off — exit 1", "pat/on — not started (a leg failed", "pat/off — not started (a leg failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// D8: RUN INVALID exhausts only that cell's ENGINE identity. Cells charging it
// are dropped; every other cell — on both beds — still runs; exit 3.
func TestMultiBedRunInvalidDropsOnlyTheExhaustedIdentity(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	cAppOffIso := Cell{Auth: "app", Train: "off", Isolated: true} // a second cell on bed B's App
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOnIso, cAppOff, cPatOn, cAppOffIso})
	h.expectStarts("A app/on", "B app/off")
	h.finish("B app/off", &ExitError{Code: ExitBudgetExhausted})
	h.expectStart("B pat/on") // user:bob is not exhausted: B continues
	h.finish("A app/on", nil)
	h.expectStart("A app/on (isolated)") // bed A is untouched
	h.finish("B pat/on", nil)
	h.finish("A app/on (isolated)", nil)
	err := h.result(done)
	if exitCode(err) != ExitBudgetExhausted {
		t.Fatalf("got %v", err)
	}
	out := h.out()
	if !strings.Contains(out, "app/off (isolated) — not started (identity app:88 budget exhausted") {
		t.Errorf("the cell on the exhausted App must be reported as not started:\n%s", out)
	}
	if !strings.Contains(out, "exhausted identities: app:88") {
		t.Errorf("summary:\n%s", out)
	}
}

// D8: the exit code is the first failure's, by time.
func TestMultiBedFirstFailureByTimeWins(t *testing.T) {
	for _, tc := range []struct {
		name        string
		first, then string
		firstErr    error
		thenErr     error
		want        int
	}{
		{"a leg failure before a RUN INVALID", "A app/on", "B app/off", &ExitError{Code: 1}, &ExitError{Code: ExitBudgetExhausted}, 1},
		{"a RUN INVALID before a leg failure", "B app/off", "A app/on", &ExitError{Code: ExitBudgetExhausted}, &ExitError{Code: 1}, ExitBudgetExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSchedHarness(t, "alice", "bob", MatrixSparse)
			_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
			h.expectStarts("A app/on", "B app/off")
			h.finish(tc.first, tc.firstErr)
			h.finish(tc.then, tc.thenErr)
			if got := exitCode(h.result(done)); got != tc.want {
				t.Errorf("exit %d, want %d", got, tc.want)
			}
		})
	}
}

// A cancel returns ctx.Err() with every identity released and nothing new started.
func TestMultiBedCancelReleasesEveryIdentity(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	ctx, cancel := context.WithCancel(context.Background())
	s, done := h.run(ctx, sparseAll)
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand")
	cancel()
	if err := h.result(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.held) != 0 {
		t.Errorf("identities still held after cancel: %v", s.held)
	}
	if len(h.ran) != 1 {
		t.Errorf("ran %v", h.ran)
	}
}

func TestDescribeAssignment(t *testing.T) {
	g, _, _, _ := twoBedGate(t)
	g.Cfg.Matrix = MatrixSparse
	if got := describeAssignment(g, sparseAll); got != "bed A: app/on, app/on (isolated); bed B (in order, next free bed): app/off, pat/on, pat/off" {
		t.Errorf("got %q", got)
	}
	if got := describeAssignment(g, []Cell{cPatOn}); got != "every bed (next free bed, in order): pat/on" {
		t.Errorf("got %q", got)
	}
}

// Bed A's waiting strict-queue head reserves its identities, so a shared-queue
// bed that keeps freeing and re-acquiring a shared identity can never starve
// the baseline.
func TestMultiBedReservationKeepsBedAFirst(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	s := newMultiSched(h.g, sparseAll)
	ctx := context.Background()
	s.held["user:arbeithand"] = holder{bed: "B", cell: "app/off"} // bed B's leg is running
	if q, _, wait, done, _ := s.next(ctx, 0); q != nil || wait == nil || done {
		t.Fatal("bed A's head must wait while bed B holds the login")
	}
	if s.reserved["user:arbeithand"] != 0 || s.reserved["app:77"] != 0 {
		t.Fatalf("bed A's waiting head must reserve its set: %v", s.reserved)
	}
	delete(s.held, "user:arbeithand") // bed B's leg finished
	q, _, wait, done, hooks := s.next(ctx, 1)
	if q != nil || wait == nil || done || len(hooks) == 0 || hooks[0].identity != "user:arbeithand" {
		t.Fatalf("bed B must not take the identity bed A is waiting for: q=%v wait=%v done=%v hooks=%v", q, wait != nil, done, hooks)
	}
	if !strings.Contains(h.out(), "[bed B] == waiting: pat/on on bed B needs user:arbeithand, reserved for bed A's next cell ==") {
		t.Errorf("out:\n%s", h.out())
	}
	q, _, _, _, _ = s.next(ctx, 0)
	if q == nil || q.cell.Label() != "app/on" || len(s.reserved) != 0 {
		t.Errorf("bed A then takes its head and drops the reservation: q=%v reserved=%v", q, s.reserved)
	}
}

// A bed that waits for an identity (or has nothing left) stops its engine, which
// would otherwise keep polling with an identity another bed's leg may hold.
func TestMultiBedStopsAnIdleBedsEngine(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	engine := exec.Command("sleep", "30")
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { engine.Wait(); close(exited) }()
	defer engine.Process.Kill()
	mustWrite(t, h.g.Cfg.BedDirs[1]+"/.fabrik/fabrik.lock", strconv.Itoa(engine.Process.Pid)+"\n")

	_, done := h.run(context.Background(), sparseAll)
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand")
	select {
	case <-exited:
	case <-time.After(failsafe):
		t.Fatal("the waiting bed's engine was not stopped")
	}
	h.finish("A app/on", nil)
	for n := 0; n < 4; n++ {
		select {
		case key := <-h.started:
			h.finish(key, nil)
		case <-time.After(failsafe):
			t.Fatalf("stalled; ran %v", h.ran)
		}
	}
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out(), "[bed B] == stopping this bed's engine while it waits for an identity") {
		t.Errorf("out:\n%s", h.out())
	}
}

// RUN INVALID on a bed with no identity to scope it to fails safe: nothing new
// starts (unreachable on a real run, where CheckBedTopology refuses a bed
// without a token).
func TestMultiBedRunInvalidWithoutAnIdentityStopsNewCells(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	b := h.g.beds()[1]
	b.Cfg.BedToken, b.login = "", ""
	s := newMultiSched(h.g, sparseAll)
	s.finish(context.Background(), 1, &queued{cell: cPatOn}, nil, &ExitError{Code: ExitBudgetExhausted})
	if !s.stopNew || len(s.exhausted) != 0 || exitCode(s.firstErr) != ExitBudgetExhausted {
		t.Errorf("stopNew=%v exhausted=%v firstErr=%v", s.stopNew, s.exhausted, s.firstErr)
	}
}

// A bed's engine is stopped BEFORE its leg's identities are released: once they
// are, another bed's leg may start on one of them, and this engine must no
// longer be polling with it (#1684's shape, between two beds).
func TestMultiBedStopsTheEngineBeforeReleasingALegsIdentities(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand")

	// The leg's restart step started bed A's engine.
	engine := exec.Command("sleep", "30")
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { engine.Wait(); close(exited) }()
	defer engine.Process.Kill()
	mustWrite(t, h.g.Cfg.BedDirs[0]+"/.fabrik/fabrik.lock", strconv.Itoa(engine.Process.Pid)+"\n")

	h.finish("A app/on", nil)
	h.expectStart("B app/off")
	// Bed B could start only after bed A released user:arbeithand, so the stop
	// must already be in the output.
	if !strings.Contains(h.out(), "[bed A] == stopping this bed's engine while its leg's identities are released") {
		t.Errorf("bed A's engine was not stopped before its identities were released; out:\n%s", h.out())
	}
	select {
	case <-exited:
	case <-time.After(failsafe):
		t.Fatal("bed A's engine was not stopped")
	}
	h.finish("B app/off", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// Dropping a reservation wakes every waiter: a bed blocked only by bed A's
// reservation must rescan once bed A gives it up (here, because bed A's head
// became unrunnable), not sleep until some unrelated leg finishes.
func TestMultiBedDroppingAReservationWakesWaiters(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	s := newMultiSched(h.g, []Cell{cAppOn, cPatOn})
	ctx := context.Background()
	s.held["user:arbeithand"] = holder{bed: "B", cell: "app/off"}
	if q, _, wait, _, _ := s.next(ctx, 0); q != nil || wait == nil {
		t.Fatal("bed A's head must wait and reserve while bed B holds the login")
	}
	delete(s.held, "user:arbeithand")
	_, _, waitB, _, _ := s.next(ctx, 1)
	if waitB == nil {
		t.Fatal("bed B must wait on bed A's reservation")
	}
	s.exhausted["app:77"] = true // bed A's head can no longer run
	if q, _, _, done, _ := s.next(ctx, 0); q != nil || !done {
		t.Fatalf("bed A drops its exhausted head and is done: q=%v done=%v", q, done)
	}
	select {
	case <-waitB:
	default:
		t.Fatal("bed B was not woken when bed A dropped its reservation")
	}
	if q, _, _, _, _ := s.next(ctx, 1); q == nil || q.cell.Label() != "pat/on" {
		t.Errorf("bed B then takes pat/on: q=%v", q)
	}
}
