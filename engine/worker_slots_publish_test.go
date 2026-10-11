package engine

import (
	"context"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

func nextSlotsEvent(t *testing.T, ch chan tui.Event) tui.WorkerSlotsEvent {
	t.Helper()
	select {
	case raw := <-ch:
		ev, ok := raw.(tui.WorkerSlotsEvent)
		if !ok {
			t.Fatalf("unexpected event %T", raw)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no WorkerSlotsEvent")
	}
	return tui.WorkerSlotsEvent{}
}

func TestWorkerSlotsPublisher_EmitsOnChangeOnly(t *testing.T) {
	eng := reservedSlotEngine(t)
	ch := make(chan tui.Event, 16)
	eng.events = ch
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.runWorkerSlotsPublisher(ctx, 5*time.Millisecond)

	want := tui.WorkerSlotsEvent{InUse: 0, Max: cap(eng.sem), TrainInUse: 0, TrainMax: 1}
	if got := nextSlotsEvent(t, ch); got != want {
		t.Fatalf("first = %+v, want %+v", got, want)
	}
	// Unchanged readings emit nothing.
	select {
	case raw := <-ch:
		t.Fatalf("unchanged reading emitted %+v", raw)
	case <-time.After(60 * time.Millisecond):
	}
	eng.trainSem <- struct{}{}
	want.TrainInUse = 1
	if got := nextSlotsEvent(t, ch); got != want {
		t.Fatalf("after train acquire = %+v, want %+v", got, want)
	}
}

func TestWorkerSlotsPublisher_NoTrainSlotReportsZeroMax(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	ch := make(chan tui.Event, 16)
	eng.events = ch
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.runWorkerSlotsPublisher(ctx, 5*time.Millisecond)
	if got := nextSlotsEvent(t, ch); got.TrainMax != 0 || got.Max != cap(eng.sem) {
		t.Errorf("got %+v", got)
	}
}
