package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
)

// reconnectEngine builds an engine whose reconcileLoop's ticker never fires
// (a very long interval) and whose light reconcile counts board fetches, so every
// counted fetch is attributable to a requested catch-up.
func reconnectEngine(t *testing.T) (*Engine, *boardcache.CacheImpl, *atomic.Int32) {
	t.Helper()
	t1 := time.Now().Truncate(time.Second)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.ReconcileInterval = time.Hour
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Plan", UpdatedAt: t1}},
	})
	eng.readClient = cache
	var fetches atomic.Int32
	client.fetchProjectBoardFn = func(_, _ string, _ int, _ string) (*gh.ProjectBoard, error) {
		fetches.Add(1)
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items:     []gh.ProjectItem{{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Plan", UpdatedAt: t1, MilestoneKnown: true}},
		}, nil
	}
	return eng, cache, &fetches
}

func waitForFetches(t *testing.T, fetches *atomic.Int32, want int32, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for fetches.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("board fetches = %d, want >= %d within %v", fetches.Load(), want, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReconcileLoop_ReconnectRequestRunsCatchUp(t *testing.T) {
	eng, cache, fetches := reconnectEngine(t)
	old := reconcileRequestMinGap
	reconcileRequestMinGap = 0
	t.Cleanup(func() { reconcileRequestMinGap = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.reconcileLoop(ctx, cache, nil)

	eng.requestReconcile()
	waitForFetches(t, fetches, 1, 2*time.Second)
}

// A flapping connection requests many catch-ups; they coalesce into at most one
// run per minimum gap instead of one reconcile per flap.
func TestReconcileLoop_FlappingReconnectCoalesces(t *testing.T) {
	eng, cache, fetches := reconnectEngine(t)
	old := reconcileRequestMinGap
	reconcileRequestMinGap = 300 * time.Millisecond
	t.Cleanup(func() { reconcileRequestMinGap = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.reconcileLoop(ctx, cache, nil)

	eng.requestReconcile()
	waitForFetches(t, fetches, 1, 2*time.Second)
	for i := 0; i < 20; i++ {
		eng.requestReconcile()
		time.Sleep(2 * time.Millisecond)
	}
	// Inside the gap nothing more has run yet ...
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches inside the gap = %d, want 1 (flaps must not each reconcile)", got)
	}
	// ... and the burst is covered by exactly one delayed catch-up.
	waitForFetches(t, fetches, 2, 2*time.Second)
	time.Sleep(450 * time.Millisecond)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches after the burst = %d, want 2 (one coalesced catch-up)", got)
	}
}

func TestRequestReconcile_NeverBlocks(t *testing.T) {
	eng, _, _ := reconnectEngine(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			eng.requestReconcile() // no loop is draining; the channel holds one
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("requestReconcile blocked with no reconcile loop running")
	}
}

// Hookdeck: the first connect is not a reconnect; a connect after a disconnect is.
func TestHookdeckManager_ReconnectFiresCatchUpOnlyAfterFirstConnect(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)
	var fired atomic.Int32
	hm.SetReconnectHandler(func() { fired.Add(1) })

	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if fired.Load() != 0 {
		t.Fatalf("first connect fired %d catch-ups, want 0", fired.Load())
	}
	hm.handleHealth(events.HealthEvent{State: events.HealthReconnecting})
	if fired.Load() != 0 {
		t.Fatalf("a disconnect fired %d catch-ups, want 0 (the catch-up is on reconnect)", fired.Load())
	}
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if fired.Load() != 1 {
		t.Fatalf("reconnect fired %d catch-ups, want 1", fired.Load())
	}
	hm.handleHealth(events.HealthEvent{State: events.HealthReconnecting})
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if fired.Load() != 2 {
		t.Fatalf("second reconnect: %d catch-ups total, want 2", fired.Load())
	}
}
