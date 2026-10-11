package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tui"
)

// Tests for #2115 (ADR 2115): the reserved merge-train worker slot, on top of
// max_concurrent. NewWithDeps engines have no reserved slot unless the test enables it,
// so the #2046 slot tests keep observing a single pool.

func reservedSlotEngine(t *testing.T) *Engine {
	t.Helper()
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.EnableTrainReservedSlotForTest()
	if cap(eng.trainSem) != 1 {
		t.Fatalf("cap(trainSem) = %d, want 1", cap(eng.trainSem))
	}
	return eng
}

func TestTrainSlotCapacity(t *testing.T) {
	if got := trainSlotCapacity(Config{MergeTrain: "on"}); got != 1 {
		t.Errorf("on = %d, want 1", got)
	}
	for _, mt := range []string{"off", ""} {
		if got := trainSlotCapacity(Config{MergeTrain: mt}); got != 0 {
			t.Errorf("%q = %d, want 0", mt, got)
		}
		if newTrainSlot(Config{MergeTrain: mt}) != nil {
			t.Errorf("%q: slot channel must be nil", mt)
		}
	}
}

func TestAcquireTrainSlot_ReservedSlotRunsImmediatelyWhenGeneralPoolSaturated(t *testing.T) {
	eng := reservedSlotEngine(t)
	ch := make(chan tui.Event, 64)
	eng.events = ch
	saturateSem(eng)

	release, err := eng.acquireTrainSlot(context.Background(), "owner/repo", makeTrainItem(3, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(eng.trainSem); got != 1 {
		t.Errorf("reserved occupancy = %d, want 1", got)
	}
	if got := len(eng.sem); got != cap(eng.sem) {
		t.Errorf("general occupancy = %d, want unchanged %d", got, cap(eng.sem))
	}
	if n := countSlotWaitLogs(collectEvents(ch, 20*time.Millisecond)); n != 0 {
		t.Errorf("reserved slot acquire logged %d wait message(s), want none", n)
	}
	// Idempotent, channel-specific release: must not drain a general slot.
	release()
	release()
	if got := len(eng.trainSem); got != 0 {
		t.Errorf("reserved occupancy after release = %d, want 0", got)
	}
	if got := len(eng.sem); got != cap(eng.sem) {
		t.Errorf("general occupancy after release = %d, want %d", got, cap(eng.sem))
	}
}

func TestAcquireTrainSlot_PrefersReservedOverFreeGeneralSlot(t *testing.T) {
	eng := reservedSlotEngine(t)
	release, err := eng.acquireTrainSlot(context.Background(), "owner/repo", makeTrainItem(3, "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if len(eng.trainSem) != 1 || len(eng.sem) != 0 {
		t.Errorf("trainSem=%d sem=%d, want the reserved slot taken and the general pool untouched", len(eng.trainSem), len(eng.sem))
	}
}

func TestAcquireTrainSlot_SecondPartitionFallsBackToFreeGeneralSlotThenWaits(t *testing.T) {
	eng := reservedSlotEngine(t)
	ch := make(chan tui.Event, 64)
	eng.events = ch

	relA, err := eng.acquireTrainSlot(context.Background(), "owner/a", makeTrainItem(1, "x"))
	if err != nil {
		t.Fatal(err)
	}
	// Second concurrent partition: reserved busy, a general slot is free.
	relB, err := eng.acquireTrainSlot(context.Background(), "owner/b", makeTrainItem(2, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.sem) != 1 {
		t.Fatalf("general occupancy = %d, want 1 (fallback)", len(eng.sem))
	}
	if n := countSlotWaitLogs(collectEvents(ch, 20*time.Millisecond)); n != 0 {
		t.Errorf("fallback logged %d wait(s), want none", n)
	}
	relB()
	if len(eng.sem) != 0 || len(eng.trainSem) != 1 {
		t.Errorf("release of the fallback slot freed the wrong pool: sem=%d trainSem=%d", len(eng.sem), len(eng.trainSem))
	}

	// Both pools full: a real wait, resolved when the reserved slot frees.
	saturateSem(eng)
	got := make(chan error, 1)
	var relC func()
	go func() {
		var err error
		relC, err = eng.acquireTrainSlot(context.Background(), "owner/c", makeTrainItem(3, "x"))
		got <- err
	}()
	deadline := time.After(5 * time.Second)
	var events []tui.Event
	for countSlotWaitLogs(events) == 0 {
		select {
		case ev := <-ch:
			events = append(events, ev)
		case <-deadline:
			t.Fatal("no wait message with both pools full")
		}
	}
	relA()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not acquire after the reserved slot freed")
	}
	relC()
	drainSem(eng)
	if len(eng.trainSem) != 0 {
		t.Errorf("reserved occupancy = %d, want 0", len(eng.trainSem))
	}
}

func TestAcquireTrainSlot_CancelWhileWaitingLeaksNoSlot(t *testing.T) {
	eng := reservedSlotEngine(t)
	ch := make(chan tui.Event, 64)
	eng.events = ch
	eng.trainSem <- struct{}{}
	saturateSem(eng)

	ctx, cancel := context.WithCancel(context.Background())
	fired, stop := cancelOnSlotWait(ch, cancel)
	defer stop()
	release, err := eng.acquireTrainSlot(ctx, "owner/repo", makeTrainItem(3, "x"))
	<-fired
	if err == nil || release != nil {
		t.Fatalf("release=%v err=%v, want nil release and an error", release != nil, err)
	}
	if !errors.Is(err, context.Canceled) || !trainCancelled(ctx, err) {
		t.Errorf("err = %v, want a cancellation wrapping context.Canceled", err)
	}
	if len(eng.trainSem) != 1 || len(eng.sem) != cap(eng.sem) {
		t.Errorf("occupancy changed by a cancelled wait: trainSem=%d sem=%d", len(eng.trainSem), len(eng.sem))
	}
}

func TestAcquireTrainSlot_NoReservedSlotKeepsSinglePool(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	if eng.trainSem != nil {
		t.Fatal("NewWithDeps must not create a reserved slot by default")
	}
	if inUse, c := eng.trainSlotUsage(); inUse != 0 || c != 0 {
		t.Errorf("usage = %d/%d, want 0/0", inUse, c)
	}
	release, err := eng.acquireTrainSlot(context.Background(), "owner/repo", makeTrainItem(3, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.sem) != 1 {
		t.Errorf("general occupancy = %d, want 1", len(eng.sem))
	}
	release()
}

func TestEnableTrainReservedSlotForTest_NoopWhenMergeTrainOff(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.cfg.MergeTrain = "off"
	eng.EnableTrainReservedSlotForTest()
	if eng.trainSem != nil {
		t.Error("reserved slot created with merge_train off")
	}
}

// Stage workers, reinvokes and comment processing must never reference the reserved
// slot; only acquireTrainSlotEp (merge_train.go) and the construction/usage helpers
// (engine.go) and the local API health read may.
func TestTrainSemReferencedOnlyWhereAllowed(t *testing.T) {
	allowed := map[string]bool{
		"engine.go":                         true,
		"merge_train.go":                    true,
		"localapi.go":                       true,
		"worker_slots_publish.go":           true,
		"merge_train_reserved_slot_test.go": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") && name != "merge_train_reserved_slot_test.go" {
			continue
		}
		if allowed[name] {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "trainSem") {
			t.Errorf("%s references trainSem; the reserved slot is merge-train-only", name)
		}
	}
	// Stage dispatch and reinvoke must additionally never use the helpers.
	for _, name := range []string{"poll.go", "reinvoke.go", "stages.go", "item.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "trainSlotUsage") || strings.Contains(string(b), "trainSem") {
			t.Errorf("%s touches the reserved train slot", name)
		}
	}
}

func TestHealth_ReportsReservedTrainSlot(t *testing.T) {
	eng := reservedSlotEngine(t)
	b := eng.LocalAPIBackend()
	h, err := b.Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Workers.TrainMax.Valid || h.Workers.TrainMax.V != 1 || !h.Workers.TrainInUse.Valid || h.Workers.TrainInUse.V != 0 {
		t.Errorf("idle workers = %+v", h.Workers)
	}
	eng.trainSem <- struct{}{}
	h, err = b.Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Workers.TrainInUse.V != 1 || h.Workers.InUse != 0 {
		t.Errorf("busy workers = %+v (reserved slot must not count in in_use)", h.Workers)
	}

	off := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	h, err = off.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Workers.TrainMax.Valid || h.Workers.TrainMax.V != 0 {
		t.Errorf("no-slot workers = %+v, want a known zero", h.Workers)
	}
}
