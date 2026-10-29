package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// blockingHook is a hook-config read the test controls: it signals entered, then
// blocks until gate closes. honourCtx makes it notice ctx ending (and then still
// waits for gate before returning, so a test can see whether Stop waits for it);
// otherwise it ignores ctx and returns url once gate closes.
type blockingHook struct {
	entered    chan struct{}
	gate       chan struct{}
	calls      atomic.Int32
	honourCtx  bool
	url        string
	enterOnce  sync.Once
	releaseOne sync.Once
}

func newBlockingHook(url string, honourCtx bool) *blockingHook {
	return &blockingHook{entered: make(chan struct{}), gate: make(chan struct{}), url: url, honourCtx: honourCtx}
}

func (b *blockingHook) release() { b.releaseOne.Do(func() { close(b.gate) }) }

func (b *blockingHook) read(ctx context.Context) (string, error) {
	b.calls.Add(1)
	b.enterOnce.Do(func() { close(b.entered) })
	if b.honourCtx {
		select {
		case <-ctx.Done():
			<-b.gate
			return "", ctx.Err()
		case <-b.gate:
			return b.url, nil
		}
	}
	<-b.gate
	return b.url, nil
}

// newStopHarness returns a harness whose receiver URL differs from the App's hook
// URL, with the cached hook URL already read, and then swaps in hook for every
// later read: the next arrival starts an arrival refresh that blocks in hook.
func newStopHarness(t *testing.T, hook *blockingHook) *livenessHarness {
	t.Helper()
	h := newLivenessHarness(t, 0)
	h.r.cfg.OwnURL = func() string { return livenessHookURL }
	h.r.evaluate(context.Background(), true) // caches the hook URL
	h.r.cfg.OwnURL = func() string { return "https://mine.example/webhook" }
	h.r.cfg.HookURL = hook.read
	t.Cleanup(hook.release)
	return h
}

func (h *livenessHarness) resolvedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.resolved)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func (h *livenessHarness) refreshInFlight() bool {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	return h.r.refreshing
}

// stopScenario stops a controller while an arrival refresh is blocked in the hook
// read. It reports whether Stop was still waiting once the refresh was cancelled
// and before the read was allowed to return.
func stopScenario(t *testing.T, tracking bool) (stopWaited bool) {
	t.Helper()
	hook := newBlockingHook(livenessHookURL, true)
	h := newStopHarness(t, hook)
	h.r.setRefreshTrackingDisabledForTest(!tracking)
	reportsBefore, resolvedBefore := h.reports(), h.resolvedCount()
	liveBefore := h.liveness()

	h.r.Arrival("real")
	waitClosed(t, hook.entered, "the refresh to reach the hook-config read")

	stopped := make(chan struct{})
	go func() { h.r.Stop(); close(stopped) }()
	select {
	case <-stopped:
		stopWaited = false
	case <-time.After(300 * time.Millisecond):
		stopWaited = true
	}
	hook.release()
	waitClosed(t, stopped, "Stop to return")
	deadline := time.Now().Add(5 * time.Second)
	for h.refreshInFlight() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.refreshInFlight() {
		t.Fatal("the refresh goroutine never exited")
	}
	if tracking {
		if h.liveness() != liveBefore || h.reports() != reportsBefore || h.resolvedCount() != resolvedBefore {
			t.Fatalf("a verdict or panel write happened after Stop: route %q, reports %d, resolved %d",
				h.liveness(), h.reports(), h.resolvedCount())
		}
	}
	return stopWaited
}

// Stop returns only after the in-flight arrival refresh has exited, and the
// refresh makes no verdict or panel write (#2480).
func TestRouteLiveness_StopWaitsForArrivalRefresh(t *testing.T) {
	if !stopScenario(t, true) {
		t.Fatal("Stop returned while the arrival refresh was still running")
	}
}

// Non-vacuity: with the tracking neutralised, Stop does not wait, so the scenario
// above would fail.
func TestRouteLiveness_StopWaitsForArrivalRefresh_NonVacuity(t *testing.T) {
	if stopScenario(t, false) {
		t.Fatal("with the tracking disabled Stop still waited: the scenario cannot tell tracked from untracked")
	}
}

// A read that ignores the context and succeeds after Stop was requested still
// yields no verdict and no panel write.
func TestRouteLiveness_LateSuccessAfterStopMakesNoVerdict(t *testing.T) {
	hook := newBlockingHook(livenessHookURL, false)
	h := newStopHarness(t, hook)
	reportsBefore, resolvedBefore := h.reports(), h.resolvedCount()
	liveBefore := h.liveness()
	h.r.Arrival("real")
	waitClosed(t, hook.entered, "the refresh to reach the hook-config read")
	stopped := make(chan struct{})
	go func() { h.r.Stop(); close(stopped) }()
	hook.release() // the read now succeeds, after Stop cancelled the context
	waitClosed(t, stopped, "Stop to return")
	if h.liveness() != liveBefore || h.reports() != reportsBefore || h.resolvedCount() != resolvedBefore {
		t.Fatalf("a late successful read produced a verdict: route %q", h.liveness())
	}
	if h.liveness() == localapi.RouteReachable {
		t.Fatal("route decided reachable after Stop")
	}
}

// A stopped controller starts no refresh and does not consume the gap.
func TestRouteLiveness_NoRefreshAfterStop(t *testing.T) {
	hook := newBlockingHook(livenessHookURL, true)
	h := newStopHarness(t, hook)
	h.r.Stop()
	h.r.Arrival("real")
	if n := hook.calls.Load(); n != 0 {
		t.Fatalf("a stopped controller read the hook config %d times", n)
	}
	h.r.mu.Lock()
	gap, refreshing := h.r.lastArrivalRefresh, h.r.refreshing
	h.r.mu.Unlock()
	if !gap.IsZero() || refreshing {
		t.Fatalf("a stopped controller started a refresh (lastArrivalRefresh %v, refreshing %v)", gap, refreshing)
	}
}

// Stop is idempotent, concurrent-safe and nil-safe.
func TestRouteLiveness_StopIsIdempotentAndConcurrent(t *testing.T) {
	var nilR *routeLiveness
	nilR.Stop()
	hook := newBlockingHook(livenessHookURL, true)
	h := newStopHarness(t, hook)
	h.r.Arrival("real")
	waitClosed(t, hook.entered, "the refresh to reach the hook-config read")
	hook.release()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.r.Stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitClosed(t, done, "concurrent Stop calls to return")
	h.r.Stop()
}

// Arrivals racing with Stop either start a tracked refresh that Stop waits for or
// start none; nothing is left running (run under -race).
func TestRouteLiveness_ArrivalsRacingStop(t *testing.T) {
	for i := 0; i < 50; i++ {
		hook := newBlockingHook(livenessHookURL, true)
		hook.release() // reads return at once
		h := newStopHarness(t, hook)
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 5; k++ {
					h.r.Arrival("real")
					h.advance(livenessArrivalRefreshGap + time.Second)
				}
			}()
		}
		h.r.Stop()
		wg.Wait()
		h.r.Stop()
		if h.refreshInFlight() {
			t.Fatal("a refresh was left running after Stop")
		}
	}
}

// The stop function startRouteLiveness returns waits for an in-flight arrival
// refresh (cancelling it first) and is safe to call twice.
func TestStartRouteLiveness_StopWaitsForArrivalRefresh(t *testing.T) {
	isolateWarnings(t)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	hook := newBlockingHook(livenessHookURL, true)
	var armed atomic.Bool
	h := newLivenessHarness(t, 0)
	idle := make(chan struct{})
	var idleOnce sync.Once
	h.r.cfg.Wait = func(ctx context.Context, _ time.Duration) bool {
		idleOnce.Do(func() { close(idle) })
		<-ctx.Done()
		return false
	}
	h.r.cfg.HookURL = func(ctx context.Context) (string, error) {
		if !armed.Load() {
			return livenessHookURL, nil
		}
		return hook.read(ctx)
	}
	h.r.cfg.OwnURL = func() string { return "https://mine.example/webhook" }
	stop := eng.startRouteLiveness(context.Background(), h.r)
	t.Cleanup(hook.release)
	waitClosed(t, idle, "the start-up evaluation to finish")

	armed.Store(true)
	h.r.Arrival("real")
	waitClosed(t, hook.entered, "the refresh to reach the hook-config read")
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while the arrival refresh was still running")
	case <-time.After(300 * time.Millisecond):
	}
	hook.release()
	waitClosed(t, stopped, "stop to return")
	if h.refreshInFlight() {
		t.Fatal("the refresh goroutine outlived stop")
	}
	stop()
}
