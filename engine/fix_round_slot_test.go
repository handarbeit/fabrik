package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tui"
)

// Tests for #2490 (ADR 2490): the optional reserved fix-round worker slots, on top of
// max_concurrent and usable only by review, CI-fix and rebase reinvokes.
// NewWithDeps engines have no reserved slots unless the test enables them.

func fixSlotEngine(t *testing.T, n int) *Engine {
	t.Helper()
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.EnableFixRoundSlotsForTest(n)
	if cap(eng.fixSem) != n {
		t.Fatalf("cap(fixSem) = %d, want %d", cap(eng.fixSem), n)
	}
	return eng
}

func TestNewFixRoundSlot(t *testing.T) {
	if newFixRoundSlot(0) != nil || newFixRoundSlot(-1) != nil {
		t.Error("a count <= 0 must yield a nil channel")
	}
	if got := cap(newFixRoundSlot(3)); got != 3 {
		t.Errorf("cap = %d, want 3", got)
	}
}

func TestAcquireFixRoundSlot_ReservedRunsImmediatelyWhenGeneralPoolSaturated(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	saturateSem(eng)

	release, reserved, err := eng.acquireFixRoundSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reserved {
		t.Error("want the reserved slot reported")
	}
	if len(eng.fixSem) != 1 || len(eng.sem) != cap(eng.sem) {
		t.Errorf("occupancy fix=%d general=%d", len(eng.fixSem), len(eng.sem))
	}
	// Idempotent, pool-specific release: must not drain a general slot.
	release()
	release()
	if len(eng.fixSem) != 0 || len(eng.sem) != cap(eng.sem) {
		t.Errorf("after release fix=%d general=%d", len(eng.fixSem), len(eng.sem))
	}
}

func TestAcquireFixRoundSlot_PrefersReservedOverFreeGeneralSlot(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	release, reserved, err := eng.acquireFixRoundSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !reserved || len(eng.fixSem) != 1 || len(eng.sem) != 0 {
		t.Errorf("reserved=%v fix=%d general=%d, want reserved first", reserved, len(eng.fixSem), len(eng.sem))
	}
}

func TestAcquireFixRoundSlot_FallsBackToGeneralWhenReservedBusy(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	eng.fixSem <- struct{}{}
	release, reserved, err := eng.acquireFixRoundSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reserved || len(eng.sem) != 1 {
		t.Errorf("reserved=%v general=%d, want an ordinary slot", reserved, len(eng.sem))
	}
	release()
	if len(eng.sem) != 0 || len(eng.fixSem) != 1 {
		t.Errorf("release returned the wrong pool: general=%d fix=%d", len(eng.sem), len(eng.fixSem))
	}
}

func TestAcquireFixRoundSlot_WaitsForEitherPool(t *testing.T) {
	for _, free := range []string{"reserved", "general"} {
		t.Run(free, func(t *testing.T) {
			eng := fixSlotEngine(t, 1)
			saturateSem(eng)
			eng.fixSem <- struct{}{}

			type result struct {
				reserved bool
				err      error
			}
			done := make(chan result, 1)
			go func() {
				rel, r, err := eng.acquireFixRoundSlot(context.Background())
				if rel != nil {
					defer rel()
				}
				done <- result{r, err}
			}()
			select {
			case <-done:
				t.Fatal("acquired while both pools were full")
			case <-time.After(50 * time.Millisecond):
			}
			if free == "reserved" {
				<-eng.fixSem
			} else {
				<-eng.sem
			}
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatal(r.err)
				}
				if r.reserved != (free == "reserved") {
					t.Errorf("reserved = %v after freeing the %s pool", r.reserved, free)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("did not acquire after a slot freed")
			}
		})
	}
}

func TestAcquireFixRoundSlot_CancelledWaitHoldsNothing(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	saturateSem(eng)
	eng.fixSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := eng.acquireFixRoundSlot(ctx)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not end the wait")
	}
	if len(eng.fixSem) != 1 || len(eng.sem) != cap(eng.sem) {
		t.Errorf("a cancelled wait changed occupancy: fix=%d general=%d", len(eng.fixSem), len(eng.sem))
	}
}

func TestAcquireFixRoundSlot_InertWhenZero(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	if eng.fixSem != nil {
		t.Fatal("NewWithDeps must leave the reserved slots nil")
	}
	release, reserved, err := eng.acquireFixRoundSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reserved || len(eng.sem) != 1 {
		t.Errorf("reserved=%v general=%d, want one ordinary slot", reserved, len(eng.sem))
	}
	release()

	// With every ordinary slot busy there is no reserved capacity to use: it waits.
	saturateSem(eng)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := eng.acquireFixRoundSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a wait that times out", err)
	}
}

func TestAcquireFixRoundSlot_NeutralisedReservationWaits(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	eng.SetFixRoundReservationDisabledForTest(true)
	saturateSem(eng)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := eng.acquireFixRoundSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the round to wait with the reservation neutralised", err)
	}
}

func TestAcquireFixRoundSlot_DrainAbandons(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	saturateSem(eng)
	eng.fixSem <- struct{}{}
	eng.beginDrain()
	_, _, err := eng.acquireFixRoundSlot(context.Background())
	if !errors.Is(err, errDrainingNotAttempted) {
		t.Errorf("err = %v, want errDrainingNotAttempted", err)
	}
}

func TestAcquireFixRoundSlot_CountAboveOne(t *testing.T) {
	eng := fixSlotEngine(t, 2)
	saturateSem(eng)
	r1, a, _ := eng.acquireFixRoundSlot(context.Background())
	r2, b, _ := eng.acquireFixRoundSlot(context.Background())
	if !a || !b || len(eng.fixSem) != 2 {
		t.Errorf("both rounds should hold reserved slots: %v %v fix=%d", a, b, len(eng.fixSem))
	}
	r1()
	r2()
}

// The reserved slots are fix-round-only: only the acquire helper (reinvoke.go) and
// the construction/usage helpers may reference the channel.
func TestFixSemReferencedOnlyWhereAllowed(t *testing.T) {
	allowed := map[string]bool{
		"engine.go":               true,
		"reinvoke.go":             true,
		"localapi.go":             true,
		"worker_slots_publish.go": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "fixSem") || strings.Contains(string(b), "fixRoundSlotUsage") {
			t.Errorf("%s references the fix-round slots; they are reinvoke-only", name)
		}
	}
	// Stage dispatch and comment processing must never touch them, nor the helper.
	for _, name := range []string{"poll.go", "stages.go", "item.go", "comments.go", "merge_train.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"fixSem", "fixRoundSlotUsage", "acquireFixRoundSlot"} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s uses %s", name, s)
			}
		}
	}
	// reinvoke.go: the only caller of the helper is dispatchReinvoke.
	b, err := os.ReadFile("reinvoke.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "e.acquireFixRoundSlot("); n != 1 {
		t.Errorf("reinvoke.go calls acquireFixRoundSlot %d times, want exactly 1", n)
	}
}

func TestHealth_ReservedFixSlotsReportedApartFromInUse(t *testing.T) {
	eng := fixSlotEngine(t, 2)
	b := eng.LocalAPIBackend()
	h, err := b.Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Workers.FixMax.Valid || h.Workers.FixMax.V != 2 || !h.Workers.FixInUse.Valid || h.Workers.FixInUse.V != 0 {
		t.Errorf("idle workers = %+v", h.Workers)
	}

	// A running fix round on the reserved slot: the channel is occupied and the
	// Store's worker handle says it holds a reserved slot.
	eng.fixSem <- struct{}{}
	eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 7, StageName: "Review", StartedAt: time.Now(), Queued: true})
	eng.store.Apply(itemstate.WorkerSlotAcquired{Repo: "owner/repo", Number: 7, At: time.Now(), Reserved: true})
	h, err = b.Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Workers.FixInUse.V != 1 || h.Workers.InUse != 0 || h.Workers.Queued != 0 {
		t.Errorf("busy workers = %+v (a reserved holder must not count in in_use)", h.Workers)
	}

	// The same worker on an ordinary slot does count in in_use.
	eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 8, StageName: "Review", StartedAt: time.Now(), Queued: true})
	eng.store.Apply(itemstate.WorkerSlotAcquired{Repo: "owner/repo", Number: 8, At: time.Now()})
	h, _ = b.Health(localapi.HealthParams{})
	if h.Workers.InUse != 1 {
		t.Errorf("in_use = %d, want 1 for the ordinary-slot worker", h.Workers.InUse)
	}

	off := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	h, err = off.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Workers.FixMax.Valid || h.Workers.FixMax.V != 0 {
		t.Errorf("disabled: fix_max = %+v, want a known 0", h.Workers.FixMax)
	}
}

func TestWorkerSlotsPublisher_ReportsFixSlots(t *testing.T) {
	eng := fixSlotEngine(t, 1)
	ch := make(chan tui.Event, 16)
	eng.events = ch
	eng.fixSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.runWorkerSlotsPublisher(ctx, 10*time.Millisecond)
	for _, ev := range collectEvents(ch, 100*time.Millisecond) {
		if se, ok := ev.(tui.WorkerSlotsEvent); ok {
			if se.FixInUse != 1 || se.FixMax != 1 || se.InUse != 0 {
				t.Errorf("event = %+v", se)
			}
			return
		}
	}
	t.Fatal("no WorkerSlotsEvent published")
}
