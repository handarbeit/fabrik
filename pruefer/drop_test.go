package pruefer

import (
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/pruefer/events"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

// TestDaemonRecordDrop_ConcurrentCallsAreExact proves the concurrency
// constraint Research flagged: unlike source.go's counters (single-writer,
// off one WebSocket read loop), eventsink.go's ReviewFromEvent runs across
// an uncapped number of concurrent goroutines, so recordDrop's dropCounts
// map must be safe under concurrent writers and its totals must come out
// exact — not just "doesn't panic under -race", but numerically correct.
func TestDaemonRecordDrop_ConcurrentCallsAreExact(t *testing.T) {
	d := &Daemon{}

	const goroutines = 50
	const perGoroutine = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				d.recordDrop(events.DropUnwatchedRepo)
			}
		}()
	}
	wg.Wait()

	want := goroutines * perGoroutine
	if got := d.DropCounts()[events.DropUnwatchedRepo]; got != want {
		t.Errorf("DropCounts()[DropUnwatchedRepo] = %d, want %d (a race would under-count)", got, want)
	}
}

// TestDaemonRecordDrop_EmitsDropEventWithCumulativeTotal guards against a
// regression to a delta-based count: DropEvent.Total must always be the
// count of everything recorded so far for that reason, not just this call
// (see recordDrop's doc comment on why — the TUI's own Emit wrapper can
// drop messages under backpressure, and only a cumulative total self-heals
// on the next delivered event).
func TestDaemonRecordDrop_EmitsDropEventWithCumulativeTotal(t *testing.T) {
	var mu sync.Mutex
	var totals []int
	d := &Daemon{Emit: func(ev ptui.Event) {
		if drop, ok := ev.(ptui.DropEvent); ok {
			mu.Lock()
			totals = append(totals, drop.Total)
			mu.Unlock()
		}
	}}

	d.recordDrop(events.DropDedupe)
	d.recordDrop(events.DropDedupe)
	d.recordDrop(events.DropDedupe)

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3}
	if len(totals) != len(want) {
		t.Fatalf("totals = %v, want %v", totals, want)
	}
	for i := range want {
		if totals[i] != want[i] {
			t.Errorf("totals[%d] = %d, want %d", i, totals[i], want[i])
		}
	}
}

// TestDaemonSignatureDriftHandler_EmitsSignatureDriftEvent guards the
// wiring between SignatureDriftHandler and the TUI: the callback it returns
// must emit a SignatureDriftEvent carrying exactly the active value it was
// called with, on both the escalation and the recovery transition.
func TestDaemonSignatureDriftHandler_EmitsSignatureDriftEvent(t *testing.T) {
	var mu sync.Mutex
	var active []bool
	d := &Daemon{Emit: func(ev ptui.Event) {
		if drift, ok := ev.(ptui.SignatureDriftEvent); ok {
			mu.Lock()
			active = append(active, drift.Active)
			mu.Unlock()
		}
	}}

	handler := d.SignatureDriftHandler()
	handler(true)
	handler(false)

	mu.Lock()
	defer mu.Unlock()
	want := []bool{true, false}
	if len(active) != len(want) {
		t.Fatalf("active = %v, want %v", active, want)
	}
	for i := range want {
		if active[i] != want[i] {
			t.Errorf("active[%d] = %v, want %v", i, active[i], want[i])
		}
	}
}
