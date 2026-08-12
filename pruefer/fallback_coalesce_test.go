package pruefer

import (
	"context"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/pruefer/events"
)

// blockingEventSource stands in for a live EventSource: it never returns until
// ctx is cancelled, matching the documented contract, so runEventDriven's loop
// is driven purely by the fallback ticker.
type blockingEventSource struct{}

func (blockingEventSource) Run(ctx context.Context, _ events.EventSink) error {
	<-ctx.Done()
	return nil
}

// TestFallbackTickerCoalescesWithInFlightPoll is the regression test for the
// fallback ticker bypassing triggerReconciliationPoll's pollInFlight guard.
//
// triggerReconciliationPoll documents "Only one poll runs at a time", but the
// ticker case called d.poll(ctx) directly, so a fallback tick could start a
// full ListOpenPRs sweep while an install-event- or reconnect-triggered
// reconciliation poll was already running. With pollInFlight already set, no
// poll may start.
func TestFallbackTickerCoalescesWithInFlightPoll(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = nil

	d := &Daemon{
		Clients:     map[string]GitHubLister{"owner": client},
		EventSource: blockingEventSource{},
		Config: Config{
			WatchedRepos:                   []string{"owner/repo"},
			ReconciliationFallbackInterval: 10 * time.Millisecond,
		},
	}

	// Simulate a reconciliation poll already in flight, exactly as
	// triggerReconciliationPoll would have left it.
	if !d.pollInFlight.CompareAndSwap(false, true) {
		t.Fatal("pollInFlight unexpectedly already set on a fresh Daemon")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = d.runEventDriven(ctx)

	// Many ticks fired during that window. Every one must have been coalesced
	// away, so the fake never saw a listing call.
	if got := client.listOpenPRsCallCount(); got != 0 {
		t.Fatalf("fallback ticker ran %d poll(s) while a reconciliation poll was in flight; want 0 (coalesced)", got)
	}
}

// TestFallbackTickerPollsWhenIdle is the companion: with nothing in flight, the
// ticker must still drive reconciliation, or the fix above would have silently
// disabled the fallback safety net.
func TestFallbackTickerPollsWhenIdle(t *testing.T) {
	client := newFakeLister()
	client.prsByRepo["owner/repo"] = nil

	d := &Daemon{
		Clients:     map[string]GitHubLister{"owner": client},
		EventSource: blockingEventSource{},
		Config: Config{
			WatchedRepos:                   []string{"owner/repo"},
			ReconciliationFallbackInterval: 10 * time.Millisecond,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = d.runEventDriven(ctx)

	// triggerReconciliationPoll runs the poll in its own goroutine; give the
	// last one a moment to land before sampling.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if client.listOpenPRsCallCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fallback ticker never polled while idle — the reconciliation safety net is dead")
}
