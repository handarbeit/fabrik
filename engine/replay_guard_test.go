package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	"github.com/handarbeit/fabrik/internal/appwebhook"
)

// Replayer leak guard (#2447). A delivery replayer outlives its test unless the
// test cancels its context and waits for the goroutines to exit; a leaked one
// keeps logging into later tests, where it races captureStdout. The guard
// records every replayer built during a test and fails that test if one is
// still running when its cleanups are done.
//
// The guard only checks. The cancelling is the starting helper's job
// (startIngestion), so forgetting it is visible instead of being repaired here.

// replayStopWait bounds how long the guard waits for a replayer to exit.
const replayStopWait = 5 * time.Second

type replayGuard struct {
	mu sync.Mutex
	rs []*appwebhook.Replayer
}

// errorfer is the part of testing.TB the guard reports through, so its own test
// can observe a failure without failing.
type errorfer interface {
	Errorf(format string, args ...any)
}

// track records r so check waits for it.
func (g *replayGuard) track(r *appwebhook.Replayer) {
	if r == nil {
		return
	}
	g.mu.Lock()
	g.rs = append(g.rs, r)
	g.mu.Unlock()
}

// check fails t for every tracked replayer that has not exited within d.
func (g *replayGuard) check(t errorfer, d time.Duration) {
	g.mu.Lock()
	rs := append([]*appwebhook.Replayer(nil), g.rs...)
	g.mu.Unlock()
	for i, r := range rs {
		if !r.WaitStopped(d) {
			t.Errorf("delivery replayer left running: replayer #%d started by this test did not exit within %s after cleanup; cancel its context and wait for it in t.Cleanup (startIngestion does this)", i+1, d)
		}
	}
}

// guardReplayers makes every replayer eng builds (direct or tailscale start,
// restarts included) tracked by a guard that fails t if one is still running
// when t's cleanups finish. Register it before the cleanup that cancels the
// replayer's context: cleanups run last-registered first, so the cancel runs
// first and the check last.
func guardReplayers(t *testing.T, eng *Engine) *replayGuard {
	t.Helper()
	g := &replayGuard{}
	eng.replayBuilt = g.track
	t.Cleanup(func() { g.check(t, replayStopWait) })
	return g
}

// startIngestion is eng.setUpIngestion with a context the test cancels at the
// end, guarded against a replayer left running. The returned cleanup is
// setUpIngestion's own.
func startIngestion(t *testing.T, eng *Engine, cache *boardcache.CacheImpl) func() {
	t.Helper()
	guardReplayers(t, eng)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return eng.setUpIngestion(ctx, cache)
}

// trackReplayer guards a replayer a test builds directly (not through an
// Engine start path, so replayBuilt never sees it). Register it before the
// cleanup that cancels the replayer's context.
func trackReplayer(t *testing.T, r *appwebhook.Replayer) {
	t.Helper()
	g := &replayGuard{}
	g.track(r)
	t.Cleanup(func() { g.check(t, replayStopWait) })
}
