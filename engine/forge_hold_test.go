package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/warnings"
)

type forgeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *forgeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *forgeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var errTimeout = fmt.Errorf("executing request: %w", &url.Error{Op: "Post", URL: "u", Err: context.DeadlineExceeded})

func newForgeTestEngine(t *testing.T) (*Engine, *forgeClock) {
	t.Helper()
	dir := t.TempDir()
	prev := warnings.WarningsPathOverride
	warnings.WarningsPathOverride = dir + "/warnings.json"
	t.Cleanup(func() { warnings.WarningsPathOverride = prev })
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	clk := &forgeClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	e.SetClock(clk)
	return e, clk
}

func TestForgeHold_EngagesOnSustainedTransportFailures(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	// A burst of many failures inside the window does not engage.
	for i := 0; i < 20; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	if e.forgeHeld() {
		t.Fatal("burst shorter than the window engaged the hold")
	}
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
	if !e.forgeHeld() {
		t.Fatal("sustained failures did not engage the hold")
	}
	snap := e.health.snapshot()
	if !snap.ForgeHeld || snap.ForgeFailures < forgeHoldMinFailures || snap.ForgeLastError == "" {
		t.Errorf("health mirror not published: %+v", snap)
	}
	entries, _ := warnings.Load()
	found := false
	for _, en := range entries {
		if en.Key == "forge_unreachable:github" {
			found = true
		}
	}
	if !found {
		t.Error("operator warning was not recorded")
	}

	e.ObserveForgeResult(nil)
	if e.forgeHeld() {
		t.Fatal("a success did not lift the hold")
	}
	if e.health.snapshot().ForgeHeld {
		t.Error("health mirror still shows the hold")
	}
	entries, _ = warnings.Load()
	for _, en := range entries {
		if en.Key == "forge_unreachable:github" {
			t.Error("warning not resolved on lift")
		}
	}
}

func TestForgeHold_SingleErrorAndFewFailuresNeverEngage(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	e.ObserveForgeResult(errTimeout)
	clk.Advance(time.Hour) // span satisfied, count is not
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("two failures engaged the hold")
	}
}

func TestForgeHold_ClientErrorLiftsAndResetsRun(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	for i := 0; i < 4; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	e.ObserveForgeResult(errors.New("GitHub API returned 404: nope")) // answered: resets the run
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("run was not reset by a 4xx")
	}
	for i := 0; i < 5; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(forgeHoldWindow)
	e.ObserveForgeResult(errTimeout)
	if !e.forgeHeld() {
		t.Fatal("hold should engage")
	}
	e.ObserveForgeResult(errors.New("GitHub API returned 403: forbidden"))
	if e.forgeHeld() {
		t.Fatal("a 4xx did not lift the hold")
	}
}

func TestForgeHold_StaleRunLapses(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	for i := 0; i < 4; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(forgeHoldRunLapse + time.Minute)
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("old failures completed by one new one engaged the hold")
	}
}

func TestForgeHold_DisabledAndCanceled(t *testing.T) {
	e, clk := newForgeTestEngine(t)
	e.SetForgeHoldDisabledForTest(true)
	for i := 0; i < 10; i++ {
		e.ObserveForgeResult(errTimeout)
	}
	clk.Advance(time.Hour)
	e.ObserveForgeResult(errTimeout)
	if e.forgeHeld() {
		t.Fatal("disabled seam still engaged")
	}
	e.SetForgeHoldDisabledForTest(false)
	for i := 0; i < 10; i++ {
		e.ObserveForgeResult(context.Canceled)
	}
	clk.Advance(time.Hour)
	e.ObserveForgeResult(context.Canceled)
	if e.forgeHeld() {
		t.Fatal("context.Canceled counted as transport")
	}
}

func TestForgeHold_ConcurrentObserve(t *testing.T) {
	e, _ := newForgeTestEngine(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if j%7 == 0 && i%2 == 0 {
					e.ObserveForgeResult(nil)
				} else {
					e.ObserveForgeResult(errTimeout)
				}
				_ = e.forgeHeld()
			}
		}(i)
	}
	wg.Wait()
}
