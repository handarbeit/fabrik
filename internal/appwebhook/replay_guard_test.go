package appwebhook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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
	rs []*Replayer
}

// errorfer is the part of testing.TB the guard reports through, so its own test
// can observe a failure without failing.
type errorfer interface {
	Errorf(format string, args ...any)
}

// track records r so check waits for it.
func (g *replayGuard) track(r *Replayer) {
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
	rs := append([]*Replayer(nil), g.rs...)
	g.mu.Unlock()
	for i, r := range rs {
		if !r.WaitStopped(d) {
			t.Errorf("delivery replayer left running: replayer #%d started by this test did not exit within %s after cleanup; cancel its context and wait for it in t.Cleanup (startIngestion does this)", i+1, d)
		}
	}
}

// trackReplayer guards a replayer a test builds directly (not through an
// Engine start path, so replayBuilt never sees it). Register it before the
// cleanup that cancels the replayer's context.
func trackReplayer(t *testing.T, r *Replayer) {
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
	r := NewReplayer(&fakeHookAPI{}, &fakeReplayLedger{seen: map[string]bool{}}, nil, nil, time.Now)
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
	g.track(NewReplayer(&fakeHookAPI{}, &fakeReplayLedger{seen: map[string]bool{}}, nil, nil, time.Now))
	g.check(&rec, 50*time.Millisecond)
	if len(rec.msgs) != 0 {
		t.Fatalf("unexpected failure: %q", rec.msgs)
	}
}

// WaitStopped covers all three goroutines: Run, the armed follow-up sleeper and
// the start-grace sleeper.
func TestReplayer_WaitStoppedCoversEveryGoroutine(t *testing.T) {
	r := NewReplayer(&fakeHookAPI{}, &fakeReplayLedger{seen: map[string]bool{}}, nil, nil, time.Now)
	r.wait = func(ctx context.Context, d time.Duration) bool { <-ctx.Done(); return false }
	ctx, cancel := context.WithCancel(context.Background())
	r.StartAfter(ctx, time.Hour) // Run + grace sleeper
	r.armFollowUp(ctx)           // follow-up sleeper
	if r.WaitStopped(50 * time.Millisecond) {
		t.Fatal("WaitStopped true while goroutines are live")
	}
	cancel()
	if !r.WaitStopped(replayStopWait) {
		t.Fatal("WaitStopped false after cancel")
	}
}
