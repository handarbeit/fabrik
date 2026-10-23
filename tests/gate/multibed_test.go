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

	lastStarted string

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
	g.schedWaitHook = func(bed string, c Cell, id, reason string) {
		h.waits <- bed + " " + cellDesc(c) + " " + id + " " + reason
	}
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

// expectStartOneOf is expectStart for a leg either bed may take; the key that
// started is left in lastStarted.
func (h *schedHarness) expectStartOneOf(want ...string) {
	h.t.Helper()
	select {
	case got := <-h.started:
		for _, w := range want {
			if got == w {
				h.lastStarted = got
				return
			}
		}
		h.t.Fatalf("started %q, want one of %v", got, want)
	case <-time.After(failsafe):
		h.t.Fatalf("none of %v started", want)
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
	cAppOff   = Cell{Auth: "app", Train: "off"}
	cPatOn    = Cell{Auth: "pat", Train: "on"}
	cPatOff   = Cell{Auth: "pat", Train: "off"}
	sparseAll = []Cell{cAppOn, cAppOff, cPatOn, cPatOff}
)

func TestAssignCells(t *testing.T) {
	a, shared := assignCells(sparseAll, true)
	if len(a) != 1 || a[0].Label() != "app/on" {
		t.Errorf("bed A: %v", a)
	}
	if len(shared) != 3 || shared[0].Label() != "app/off" || shared[1].Label() != "pat/on" || shared[2].Label() != "pat/off" {
		t.Errorf("shared: %v", shared)
	}
	if a, shared := assignCells([]Cell{cAppOff, cPatOn}, true); a != nil || len(shared) != 2 {
		t.Error("no baseline in the plan: every bed serves the shared queue")
	}
	if a, shared := assignCells(sparseAll, false); a != nil || len(shared) != 4 {
		t.Error("E2E_MATRIX=full: every bed serves the shared queue")
	}
}

// D6: the baseline (app/on) runs on bed A while bed B runs the
// other cells in sparse order — concurrently, since no identity is shared.
func TestMultiBedDefaultAssignmentRunsConcurrently(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	_, done := h.run(context.Background(), sparseAll)
	h.expectStarts("A app/on", "B app/off") // both running at once: disjoint identities
	h.finish("B app/off", nil)
	h.expectStart("B pat/on")
	h.finish("A app/on", nil)
	h.finish("B pat/on", nil)
	h.expectStart("B pat/off")
	h.finish("B pat/off", nil)
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
		"bed A (" + h.g.Cfg.BedDirs[0] + "): app/on — passed\n",
		"bed B (" + h.g.Cfg.BedDirs[1] + "): app/off — passed; pat/on — passed; pat/off — passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// #2263 R1: a harness use of an identity is shared. Both beds' tokens resolve to
// the same login, so bed B's app/off (engine: its own App; harness: user:arbeithand)
// runs concurrently with bed A's app/on instead of waiting for its whole leg. So
// do bed B's pat cells, whose ENGINE identity is that user (#1990).
func TestMultiBedSharesTheHarnessUserBetweenAppCells(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	_, done := h.run(context.Background(), sparseAll)
	h.expectStarts("A app/on", "B app/off")
	h.mu.Lock()
	shared := h.maxActive["user:arbeithand"]
	h.mu.Unlock()
	if shared != 2 {
		t.Fatalf("both app legs must hold user:arbeithand as harness at once, got %d", shared)
	}
	if strings.Contains(h.out(), "== waiting:") {
		t.Errorf("sharing a harness user never waits:\n%s", h.out())
	}
	h.finish("B app/off", nil)
	// B's pat/on uses user:arbeithand as its ENGINE, which shares with A's
	// harness use (#1990): it starts while A's app/on is still running.
	h.expectStart("B pat/on")
	h.finish("B pat/on", nil)
	h.expectStart("B pat/off")
	if strings.Contains(h.out(), "== waiting:") {
		t.Errorf("a pat engine shares the user with the other bed's harness; nothing waits:\n%s", h.out())
	}
	h.finish("A app/on", nil)
	h.finish("B pat/off", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// Non-vacuity of the above: with the role split neutralised (every identity
// exclusive) bed B's app/off waits for bed A's whole leg, as before #2263.
func TestMultiBedSharedHarnessUserIsVacuousWithoutTheRoleSplit(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	h.g.roleSplitDisabledForTest = true
	_, done := h.run(context.Background(), sparseAll)
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand exclusive")
	h.noStart()
	h.finish("A app/on", nil)
	for n := 0; n < 3; n++ {
		select {
		case key := <-h.started:
			h.finish(key, nil)
		case <-time.After(failsafe):
			t.Fatalf("stalled after %d more legs; ran %v", n, h.ran)
		}
	}
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if h.maxActive["user:arbeithand"] != 1 {
		t.Errorf("without the split the login is held by one leg at a time, got %d", h.maxActive["user:arbeithand"])
	}
}

// #1990 (amends #2263 R1): a pat cell's engine identity is the bed token's user.
// It shares that user with another bed's harness use, both ways, above the budget
// floor; but two engines never poll as one user, so a second pat cell waits.
func TestMultiBedPatEngineSharesTheHarnessUser(t *testing.T) {
	t.Run("a pat cell runs while another bed holds the user as harness", func(t *testing.T) {
		h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
		_, done := h.run(context.Background(), []Cell{cAppOn, cPatOn})
		h.expectStarts("A app/on", "B pat/on")
		h.finish("B pat/on", nil)
		h.finish("A app/on", nil)
		if err := h.result(done); err != nil {
			t.Fatal(err)
		}
		if h.maxActive["user:arbeithand"] != 2 {
			t.Errorf("the pat engine and the other bed's harness must hold the user at once, got %d", h.maxActive["user:arbeithand"])
		}
	})
	t.Run("an app cell runs while a pat cell holds the user as engine", func(t *testing.T) {
		h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixFull)
		_, done := h.run(context.Background(), []Cell{cPatOn, cAppOff})
		h.expectStarts("A pat/on", "B app/off")
		h.finish("A pat/on", nil)
		h.finish("B app/off", nil)
		if err := h.result(done); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a second pat cell waits: two engines never share a user", func(t *testing.T) {
		h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixFull)
		_, done := h.run(context.Background(), []Cell{cPatOn, cPatOff})
		h.expectStart("A pat/on")
		h.expectWait("B pat/off user:arbeithand exclusive")
		h.noStart()
		if !strings.Contains(h.out(), "[bed B] == waiting: pat/off on bed B needs user:arbeithand (pat engine), held as pat engine by bed A (pat/on) ==") {
			t.Errorf("the wait must name the engine holder:\n%s", h.out())
		}
		h.finish("A pat/on", nil)
		h.expectStartOneOf("A pat/off", "B pat/off")
		h.finish(h.lastStarted, nil)
		if err := h.result(done); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("an app installation stays exclusive", func(t *testing.T) {
		// Both beds on one installation (the scheduler's view; the bed-topology
		// preflight refuses this for real): an App engine identity is never shared.
		h := newSchedHarness(t, "arbeithand", "other", MatrixFull)
		for _, b := range h.g.beds() {
			b.bed.AppInstallationID = "77"
		}
		_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
		h.expectStart("A app/on")
		h.expectWait("B app/off app:77 exclusive")
		h.finish("A app/on", nil)
		h.expectStartOneOf("A app/off", "B app/off")
		h.finish(h.lastStarted, nil)
		if err := h.result(done); err != nil {
			t.Fatal(err)
		}
	})
}

// Non-vacuity of the above: with pat-engine sharing neutralised (the #2263 rule)
// a pat cell waits for the other bed's whole harness leg, and the wait says so.
func TestMultiBedPatEngineSharingIsVacuousWithoutIt(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	h.g.patEngineShareDisabledForTest = true
	_, done := h.run(context.Background(), []Cell{cAppOn, cPatOn})
	h.expectStart("A app/on")
	h.expectWait("B pat/on user:arbeithand exclusive")
	h.noStart()
	if !strings.Contains(h.out(), "[bed B] == waiting: pat/on on bed B needs user:arbeithand exclusively, held by bed A (app/on) as harness ==") {
		t.Errorf("out:\n%s", h.out())
	}
	h.finish("A app/on", nil)
	h.expectStart("B pat/on")
	h.finish("B pat/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
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
	h.expectWait("B pat/on user:arbeithand exclusive")
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
	s.held["app:88"] = &idHold{excl: &holder{bed: "X", cell: "elsewhere"}} // bed B's App is busy
	d := s.next(context.Background(), 1)
	if d.done || d.wait != nil || d.q == nil || d.q.cell.Label() != "pat/on" || len(d.set) != 1 || d.set[0].Key != "user:bob" {
		t.Fatalf("bed B must skip the blocked app/off for pat/on: q=%v set=%v wait=%v done=%v", d.q, d.set, d.wait != nil, d.done)
	}
	// With pat/on taken, bed B now has only a blocked cell: it waits.
	if d := s.next(context.Background(), 1); d.done || d.wait == nil || len(d.hooks) != 1 || d.hooks[0].identity != "app:88" || d.hooks[0].reason != waitExclusive {
		t.Errorf("only the blocked app/off is left, so bed B waits on its App: wait=%v done=%v hooks=%v", d.wait != nil, d.done, d.hooks)
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
	for _, want := range []string{"app/on — passed", "app/off — exit 1", "pat/on — not started (a leg failed", "pat/off — not started (a leg failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// D8: RUN INVALID exhausts only that cell's ENGINE identity. Cells charging it
// are dropped; every other cell — on both beds — still runs; exit 3.
func TestMultiBedRunInvalidDropsOnlyTheExhaustedIdentity(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	_, done := h.run(context.Background(), sparseAll)
	h.expectStarts("A app/on", "B app/off")
	h.finish("B app/off", nil)
	h.expectStart("B pat/on")
	h.finish("B pat/on", &ExitError{Code: ExitBudgetExhausted}) // exhausts user:bob
	h.noStart()                                                 // pat/off charges user:bob: dropped
	h.finish("A app/on", nil)                                   // bed A's identities are untouched
	err := h.result(done)
	if exitCode(err) != ExitBudgetExhausted {
		t.Fatalf("got %v", err)
	}
	if len(h.ran) != 3 {
		t.Errorf("ran %v", h.ran)
	}
	out := h.out()
	for _, want := range []string{"app/on — passed", "pat/off — not started (identity user:bob budget exhausted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "exhausted identities: user:bob") {
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
	h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
	ctx, cancel := context.WithCancel(context.Background())
	s, done := h.run(ctx, []Cell{cAppOn, cPatOn})
	h.expectStart("A app/on")
	h.expectWait("B pat/on user:arbeithand exclusive")
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
	if got := describeAssignment(g, sparseAll); got != "bed A: app/on; bed B (in order, next free bed): app/off, pat/on, pat/off" {
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
	h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
	s := newMultiSched(h.g, []Cell{cAppOn, cPatOn})
	ctx := context.Background()
	s.held["user:arbeithand"] = &idHold{excl: &holder{bed: "B", cell: "pat/off"}} // bed B's pat leg is running
	if d := s.next(ctx, 0); d.q != nil || d.wait == nil || d.done {
		t.Fatal("bed A's head must wait while bed B holds the login exclusively")
	}
	if r := s.reserved["user:arbeithand"]; r != (resv{bed: 0, use: useHarness}) || s.reserved["app:77"] != (resv{bed: 0, use: useExclusive}) {
		t.Fatalf("bed A's waiting head must reserve its set, each in its role: %v", s.reserved)
	}
	delete(s.held, "user:arbeithand") // bed B's leg finished
	d := s.next(ctx, 1)
	if d.q != nil || d.wait == nil || d.done || len(d.hooks) == 0 || d.hooks[0].identity != "user:arbeithand" || d.hooks[0].reason != waitReserved {
		t.Fatalf("bed B must not take the identity bed A is waiting for: q=%v wait=%v done=%v hooks=%v", d.q, d.wait != nil, d.done, d.hooks)
	}
	if !strings.Contains(h.out(), "[bed B] == waiting: pat/on on bed B needs user:arbeithand exclusively, reserved for bed A's next cell ==") {
		t.Errorf("out:\n%s", h.out())
	}
	d = s.next(ctx, 0)
	if d.q == nil || d.q.cell.Label() != "app/on" || len(s.reserved) != 0 {
		t.Errorf("bed A then takes its head and drops the reservation: q=%v reserved=%v", d.q, s.reserved)
	}
}

// #2263: a reservation carries its role. A strict-queue head that needs the
// harness user only as HARNESS blocks another bed's engine use of it but not
// another bed's harness join — otherwise sharing would stop whenever bed A waits.
// An ENGINE reservation blocks both.
func TestMultiBedReservationRoleDecidesWhoItBlocks(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	ctx := context.Background()

	s := newMultiSched(h.g, []Cell{cAppOn, cAppOff, cPatOn})
	s.held["app:77"] = &idHold{excl: &holder{bed: "X", cell: "elsewhere"}} // bed A's App is busy: its head waits
	if d := s.next(ctx, 0); d.q != nil || d.wait == nil {
		t.Fatal("bed A's head must wait on its App")
	}
	if r := s.reserved["user:arbeithand"]; r.use == useExclusive {
		t.Fatalf("bed A needs the login only as harness: %v", r)
	}
	if d := s.next(ctx, 1); d.q == nil || d.q.cell.Label() != "app/off" {
		t.Errorf("bed B's app/off shares the login as harness despite bed A's harness reservation: %+v", d)
	}

	// An engine reservation (not reachable from the real assignment, where bed A's
	// baseline is app/on, but the rule is the registry's) blocks a harness use too.
	s = newMultiSched(h.g, []Cell{cAppOff})
	s.reserved["user:arbeithand"] = resv{bed: 0, use: useExclusive}
	if d := s.next(ctx, 1); d.q != nil || d.wait == nil || len(d.hooks) == 0 || d.hooks[0].reason != waitReserved {
		t.Errorf("an engine reservation blocks bed B's harness use too: %+v", d)
	}
}

// A bed that waits for an identity (or has nothing left) stops its engine, which
// would otherwise keep polling with an identity another bed's leg may hold.
func TestMultiBedStopsAnIdleBedsEngine(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
	engine := exec.Command("sleep", "30")
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { engine.Wait(); close(exited) }()
	defer engine.Process.Kill()
	mustWrite(t, h.g.Cfg.BedDirs[1]+"/.fabrik/fabrik.lock", strconv.Itoa(engine.Process.Pid)+"\n")

	_, done := h.run(context.Background(), []Cell{cAppOn, cPatOn})
	h.expectStart("A app/on")
	h.expectWait("B pat/on user:arbeithand exclusive")
	select {
	case <-exited:
	case <-time.After(failsafe):
		t.Fatal("the waiting bed's engine was not stopped")
	}
	h.finish("A app/on", nil)
	for n := 0; n < 1; n++ {
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
	h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
	_, done := h.run(context.Background(), []Cell{cAppOn, cPatOn})
	h.expectStart("A app/on")
	h.expectWait("B pat/on user:arbeithand exclusive")

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
	h.expectStart("B pat/on")
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
	h.finish("B pat/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// Dropping a reservation wakes every waiter: a bed blocked only by bed A's
// reservation must rescan once bed A gives it up (here, because bed A's head
// became unrunnable), not sleep until some unrelated leg finishes.
func TestMultiBedDroppingAReservationWakesWaiters(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
	s := newMultiSched(h.g, []Cell{cAppOn, cPatOn})
	ctx := context.Background()
	s.held["user:arbeithand"] = &idHold{excl: &holder{bed: "B", cell: "pat/off"}}
	if d := s.next(ctx, 0); d.q != nil || d.wait == nil {
		t.Fatal("bed A's head must wait and reserve while bed B holds the login")
	}
	delete(s.held, "user:arbeithand")
	waitB := s.next(ctx, 1).wait
	if waitB == nil {
		t.Fatal("bed B must wait on bed A's reservation")
	}
	s.exhausted["app:77"] = true // bed A's head can no longer run
	if d := s.next(ctx, 0); d.q != nil || !d.done {
		t.Fatalf("bed A drops its exhausted head and is done: q=%v done=%v", d.q, d.done)
	}
	select {
	case <-waitB:
	default:
		t.Fatal("bed B was not woken when bed A dropped its reservation")
	}
	if d := s.next(ctx, 1); d.q == nil || d.q.cell.Label() != "pat/on" {
		t.Errorf("bed B then takes pat/on: q=%v", d.q)
	}
}

// floorHarness is a two-bed sparse run where both beds' tokens are the same
// user, with the budget floor on and a fake budget probe. Bed A's token is
// token-a, bed B's token-b: the joiner (bed B) is the one probed.
func newFloorHarness(t *testing.T, fi *fakeIdentity) *schedHarness {
	t.Helper()
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	h.g.Identity = fi
	h.g.Cfg.SharedIdentityFloor = 40
	h.g.Cfg.SharedIdentityReprobe = 20 * time.Millisecond
	return h
}

// #2263 R2: a second holder of a shared harness identity starts only if the
// identity's remaining budget is at least the floor. Below it, the cell waits and
// says why — the identity, what remains of the limit and when it resets — and
// starts once the budget recovers.
func TestMultiBedBelowFloorBlocksASecondHolderUntilItRecovers(t *testing.T) {
	fi := &fakeIdentity{budgets: map[string]int{"token-b": 1500}} // 30% of 5000: below the 40% floor
	h := newFloorHarness(t, fi)
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand floor")
	h.noStart()
	for _, want := range []string{
		"[bed B] == waiting: app/off on bed B shares user:arbeithand (harness) with bed A (app/on, harness), but its budget is below the floor: 1500 of 5000 remaining (floor 40%), resets 2026-10-02T10:15:00Z;",
	} {
		if !strings.Contains(h.out(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out())
		}
	}
	// The budget recovers; the bed's re-probe timer finds it and the cell starts
	// beside bed A's.
	fi.mu.Lock()
	fi.budgets["token-b"] = 4000
	fi.mu.Unlock()
	h.expectStart("B app/off")
	if h.maxActive["user:arbeithand"] != 2 {
		t.Errorf("the recovered cell must share the user with bed A's: %d", h.maxActive["user:arbeithand"])
	}
	h.finish("B app/off", nil)
	h.finish("A app/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	// An unchanged below-floor reading across re-probes is logged once, not each time.
	if n := strings.Count(h.out(), "below the floor"); n != 1 {
		t.Errorf("the same wait must be logged once, got %d:\n%s", n, h.out())
	}
}

// The floor is what holds the cell back: without a floor the same budget shares.
func TestMultiBedBelowFloorIsVacuousWithoutTheFloor(t *testing.T) {
	fi := &fakeIdentity{budgets: map[string]int{"token-b": 1500}}
	h := newFloorHarness(t, fi)
	h.g.Cfg.SharedIdentityFloor = 0
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStarts("A app/on", "B app/off")
	h.finish("B app/off", nil)
	h.finish("A app/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if len(fi.probes) != 0 {
		t.Errorf("a disabled floor probes nothing: %v", fi.probes)
	}
}

// The floor is a share of the REAL limit, not of 5,000.
func TestMultiBedFloorUsesTheRealLimit(t *testing.T) {
	// 6000 of 15000 = 40%: exactly at the floor, so it shares.
	fi := &fakeIdentity{budgets: map[string]int{"token-b": 6000}, limits: map[string]int{"token-b": 15000}}
	h := newFloorHarness(t, fi)
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStarts("A app/on", "B app/off")
	h.finish("B app/off", nil)
	h.finish("A app/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	// 5999 of 15000 is below it, though far above 40% of 5000.
	fi = &fakeIdentity{budgets: map[string]int{"token-b": 5999}, limits: map[string]int{"token-b": 15000}}
	h = newFloorHarness(t, fi)
	_, done = h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand floor")
	fi.mu.Lock()
	fi.budgets["token-b"] = 9000
	fi.mu.Unlock()
	h.expectStart("B app/off")
	h.finish("B app/off", nil)
	h.finish("A app/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// R2: an unreadable budget is "below floor" for a second holder — never a
// failure — and the wait says it could not be read.
func TestMultiBedUnreadableBudgetBlocksASecondHolderWithoutFailing(t *testing.T) {
	fi := &fakeIdentity{budgetErr: errors.New("gh exited 1: HTTP 502")}
	h := newFloorHarness(t, fi)
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand floor")
	h.noStart()
	if !strings.Contains(h.out(), "but its budget could not be read (gh exited 1: HTTP 502)") {
		t.Errorf("out:\n%s", h.out())
	}
	fi.mu.Lock()
	fi.budgetErr = nil
	fi.budgets = map[string]int{"token-b": 4000}
	fi.mu.Unlock()
	h.expectStart("B app/off")
	h.finish("B app/off", nil)
	h.finish("A app/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// R2: the first holder of an identity needs no probe, so a one-bed-shaped run is
// unchanged; a waiting bed's engine is stopped on a floor wait like on any other.
func TestMultiBedFirstHolderIsNeverProbed(t *testing.T) {
	fi := &fakeIdentity{budgets: map[string]int{"token-a": 1, "token-b": 1}}
	h := newFloorHarness(t, fi)
	_, done := h.run(context.Background(), []Cell{cAppOn})
	h.expectStart("A app/on")
	h.finish("A app/on", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if len(fi.probes) != 0 {
		t.Errorf("a sole holder is never probed: %v", fi.probes)
	}
}

// A leg finishing re-probes at once, without waiting for the timer: the finished
// leg's release moves the epoch, so the stale reading is not reused.
func TestMultiBedLegFinishReprobesWithoutTheTimer(t *testing.T) {
	fi := &fakeIdentity{budgets: map[string]int{"token-b": 1500}}
	h := newFloorHarness(t, fi)
	h.g.Cfg.SharedIdentityReprobe = time.Hour
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand floor")
	fi.mu.Lock()
	fi.budgets["token-b"] = 4000
	fi.mu.Unlock()
	h.noStart() // nothing has changed for the scheduler yet
	h.finish("A app/on", nil)
	h.expectStart("B app/off") // sole holder now: not even a probe is needed
	h.finish("B app/off", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
}

// R3: RUN INVALID on an identity two beds share stops every cell that charges it,
// in either role — here bed A's pat cell exhausts the login, and bed B's app cell,
// which would only have used it as harness, is not started. (Control: without the
// RUN INVALID that app cell runs.)
func TestMultiBedRunInvalidOnASharedIdentityStopsCellsInEitherRole(t *testing.T) {
	cells := []Cell{cPatOn, cAppOff}
	t.Run("exhausted", func(t *testing.T) {
		h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixFull)
		h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
		_, done := h.run(context.Background(), cells)
		h.expectStart("A pat/on")
		h.expectWait("B app/off user:arbeithand exclusive")
		h.finish("A pat/on", &ExitError{Code: ExitBudgetExhausted})
		err := h.result(done)
		if exitCode(err) != ExitBudgetExhausted {
			t.Fatalf("got %v", err)
		}
		if len(h.ran) != 1 {
			t.Errorf("the harness-role cell must not start on an exhausted login: %v", h.ran)
		}
		if !strings.Contains(h.out(), "app/off — not started (identity user:arbeithand budget exhausted (RUN INVALID))") {
			t.Errorf("out:\n%s", h.out())
		}
	})
	t.Run("control: no exhaustion, the app cell runs", func(t *testing.T) {
		h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixFull)
		h.g.patEngineShareDisabledForTest = true // needs an exclusive conflict: a pat engine gives one under the #2263 rule
		_, done := h.run(context.Background(), cells)
		h.expectStart("A pat/on")
		h.expectWait("B app/off user:arbeithand exclusive")
		h.finish("A pat/on", nil)
		h.expectStartOneOf("A app/off", "B app/off")
		h.finish(h.lastStarted, nil)
		if err := h.result(done); err != nil {
			t.Fatal(err)
		}
	})
}

// R3, the other direction: an exhausted login stops later cells on EITHER bed that
// would have shared it as harness, while an exhausted App does not touch the
// login's other users.
func TestMultiBedExhaustedAppDoesNotStopTheSharedLoginsOtherCells(t *testing.T) {
	h := newSchedHarness(t, "arbeithand", "arbeithand", MatrixSparse)
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStarts("A app/on", "B app/off")
	h.finish("A app/on", &ExitError{Code: ExitBudgetExhausted}) // exhausts app:77 only
	h.finish("B app/off", nil)                                  // bed B's cell, on the login and app:88, is unaffected
	if exitCode(h.result(done)) != ExitBudgetExhausted {
		t.Fatal("exit code is the first failure's")
	}
	if !strings.Contains(h.out(), "exhausted identities: app:77\n") {
		t.Errorf("out:\n%s", h.out())
	}
}

// Non-vacuity of the floor tests: with the role split neutralised the second
// cell is held back by exclusivity, not by the floor, and nothing is probed.
func TestMultiBedFloorIsVacuousWithoutTheRoleSplit(t *testing.T) {
	fi := &fakeIdentity{budgets: map[string]int{"token-b": 1500}}
	h := newFloorHarness(t, fi)
	h.g.roleSplitDisabledForTest = true
	_, done := h.run(context.Background(), []Cell{cAppOn, cAppOff})
	h.expectStart("A app/on")
	h.expectWait("B app/off user:arbeithand exclusive")
	h.finish("A app/on", nil)
	h.expectStart("B app/off")
	h.finish("B app/off", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if len(fi.probes) != 0 || strings.Contains(h.out(), "below the floor") {
		t.Errorf("without the split there is no sharing to probe: probes=%v\n%s", fi.probes, h.out())
	}
}

// #2426 (FR-004): the webhook cell is never the baseline, and it runs only on a
// webhook-capable bed.
func TestAssignCellsKeepsTheWebhookCellOffBedAsBaseline(t *testing.T) {
	cWebhook := Cell{Auth: "app", Train: "on", Events: EventsWebhook}
	if cWebhook.isBaseline() {
		t.Fatal("app/on/webhook must not be taken for the baseline")
	}
	a, shared := assignCells([]Cell{{Auth: "app", Train: "on"}, cAppOff, cWebhook}, true)
	if len(a) != 1 || a[0].Label() != "app/on" || len(shared) != 2 || shared[1].Label() != "app/on/webhook" {
		t.Errorf("bed A %v, shared %v", a, shared)
	}
	// A plan with a webhook cell but no poll baseline has no bed-A queue.
	if a, shared := assignCells([]Cell{cAppOff, cWebhook}, true); a != nil || len(shared) != 2 {
		t.Errorf("without the poll baseline every bed serves the shared queue: %v %v", a, shared)
	}
}

func TestMultiBedAssignsAWebhookCellOnlyToACapableBed(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	// Bed A is a poll-only bed; bed B is provisioned for webhooks.
	mustWrite(t, h.g.Cfg.BedDirs[0]+"/.env", "FABRIK_TOKEN=a\n")
	mustWrite(t, h.g.Cfg.BedDirs[1]+"/.env", "FABRIK_TOKEN=b\nFABRIK_TAILSCALE_HOSTNAME=bed-b\nFABRIK_TS_AUTHKEY=k\nFABRIK_GITHUB_WEBHOOK_SECRET=s\n")
	cells := []Cell{{Auth: "app", Train: "on"}, {Auth: "app", Train: "on", Events: EventsWebhook}}
	_, done := h.run(context.Background(), cells)
	// Bed A takes its baseline; the webhook cell goes to B although A could ask first.
	h.expectStarts("A app/on", "B app/on/webhook")
	h.finish("A app/on", nil)
	h.finish("B app/on/webhook", nil)
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.ran {
		if c == "A app/on/webhook" {
			t.Fatalf("a webhook cell ran on the poll-only bed: %v", h.ran)
		}
	}
}

func TestMultiBedNoCapableBedNeverRunsAWebhookCellPollOnly(t *testing.T) {
	h := newSchedHarness(t, "alice", "bob", MatrixSparse)
	mustWrite(t, h.g.Cfg.BedDirs[0]+"/.env", "FABRIK_TOKEN=a\n")
	mustWrite(t, h.g.Cfg.BedDirs[1]+"/.env", "FABRIK_TOKEN=b\n")
	_, done := h.run(context.Background(), []Cell{{Auth: "app", Train: "on", Events: EventsWebhook}})
	if err := h.result(done); err != nil {
		t.Fatal(err)
	}
	if len(h.ran) != 0 {
		t.Errorf("the scheduler must not run a webhook cell on a bed that cannot receive deliveries: %v", h.ran)
	}
}
