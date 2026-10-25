package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
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
	rs []*deliveryReplayer
}

// errorfer is the part of testing.TB the guard reports through, so its own test
// can observe a failure without failing.
type errorfer interface {
	Errorf(format string, args ...any)
}

// track records r so check waits for it.
func (g *replayGuard) track(r *deliveryReplayer) {
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
	rs := append([]*deliveryReplayer(nil), g.rs...)
	g.mu.Unlock()
	for i, r := range rs {
		if !r.waitStopped(d) {
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

// Registered first via the helpers, so a test that builds a replayer directly
// (newDeliveryReplayer) tracks it explicitly.
func trackReplayer(t *testing.T, r *deliveryReplayer) {
	t.Helper()
	g := &replayGuard{}
	g.track(r)
	t.Cleanup(func() { g.check(t, replayStopWait) })
}

type recordedErrors struct{ msgs []string }

func (r *recordedErrors) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func TestReplayGuard_FailsOnALeakedReplayer(t *testing.T) {
	r := newDeliveryReplayer(&fakeHookAPI{}, &fakeReplayLedger{seen: map[string]bool{}}, nil, nil, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	r.goTracked(func() { r.Run(ctx) })
	g := &replayGuard{}
	g.track(r)

	var rec recordedErrors
	g.check(&rec, 50*time.Millisecond)
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "delivery replayer left running") {
		t.Fatalf("leaked replayer not reported: %q", rec.msgs)
	}

	cancel()
	rec = recordedErrors{}
	g.check(&rec, replayStopWait)
	if len(rec.msgs) != 0 {
		t.Fatalf("stopped replayer reported: %q", rec.msgs)
	}
}

func TestReplayGuard_QuietWithoutReplayers(t *testing.T) {
	g := &replayGuard{}
	g.track(nil)
	var rec recordedErrors
	g.check(&rec, 50*time.Millisecond)
	// A built but never started replayer has nothing to wait for.
	g.track(newDeliveryReplayer(&fakeHookAPI{}, &fakeReplayLedger{seen: map[string]bool{}}, nil, nil, time.Now))
	g.check(&rec, 50*time.Millisecond)
	if len(rec.msgs) != 0 {
		t.Fatalf("unexpected failure: %q", rec.msgs)
	}
}

// waitStopped covers all three goroutines: Run, the armed follow-up sleeper and
// the start-grace sleeper.
func TestReplayer_WaitStoppedCoversEveryGoroutine(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	r := newDeliveryReplayer(&fakeHookAPI{}, &fakeReplayLedger{seen: map[string]bool{}}, nil, nil, time.Now)
	r.wait = func(ctx context.Context, d time.Duration) bool { <-ctx.Done(); return false }
	ctx, cancel := context.WithCancel(context.Background())
	e.startDeliveryReplayAfter(ctx, r, time.Hour) // Run + grace sleeper
	r.armFollowUp(ctx)                            // follow-up sleeper
	if r.waitStopped(50 * time.Millisecond) {
		t.Fatal("waitStopped true while goroutines are live")
	}
	cancel()
	if !r.waitStopped(replayStopWait) {
		t.Fatal("waitStopped false after cancel")
	}
}
