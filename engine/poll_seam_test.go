package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/pollctl"
)

// Tests for the bed-only poll hold/trigger seam (#1978, ADR-1978). The first
// group drives pollSeam directly with fake cycles; the second drives the real
// Run() loop, which is where the "every trigger path is gated" claim lives.

func seamForTest(t *testing.T) (*pollSeam, string) {
	t.Helper()
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	path := pollctl.ControlPath(t.TempDir())
	s := newPollSeam(eng, path)
	return s, path
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ranCycle(calls *int32) func() (PollBackoffResult, error) {
	return func() (PollBackoffResult, error) {
		atomic.AddInt32(calls, 1)
		return PollBackoffResult{NextInterval: time.Second, Ran: true}, nil
	}
}

func TestPollSeam_HeldDropsAutomaticPollAndReconcile(t *testing.T) {
	s, _ := seamForTest(t)
	var polls, ticks int32
	s.setHeld(true)

	if err := s.ordinary(ranCycle(&polls)); err != nil {
		t.Fatalf("ordinary while held: %v", err)
	}
	s.runUnlessHeld(func() { atomic.AddInt32(&ticks, 1) })
	if polls != 0 || ticks != 0 {
		t.Fatalf("held: polls=%d reconcile ticks=%d, want 0/0", polls, ticks)
	}

	s.setHeld(false)
	if err := s.ordinary(ranCycle(&polls)); err != nil {
		t.Fatal(err)
	}
	s.runUnlessHeld(func() { atomic.AddInt32(&ticks, 1) })
	if polls != 1 || ticks != 1 {
		t.Fatalf("released: polls=%d reconcile ticks=%d, want 1/1 (a dropped cycle is not queued)", polls, ticks)
	}
}

func TestPollSeam_TriggerRunsExactlyOneCompletePoll(t *testing.T) {
	s, path := seamForTest(t)
	var polls int32
	s.cycle = ranCycle(&polls)
	ctx := context.Background()

	req := pollctl.Request{Hold: true, HoldUntil: time.Now().Add(time.Minute).Unix(), HoldGen: 1}
	if err := pollctl.WriteRequest(path, req); err != nil {
		t.Fatal(err)
	}
	s.step(ctx)
	if a := pollctl.ReadAck(path); !a.Held || a.HoldGen != 1 {
		t.Fatalf("hold ack = %+v", a)
	}
	if polls != 0 {
		t.Fatalf("hold alone ran %d polls", polls)
	}

	req.TriggerSeq = 1
	if err := pollctl.WriteRequest(path, req); err != nil {
		t.Fatal(err)
	}
	s.step(ctx)
	s.step(ctx) // re-observing the same request must not poll again
	if polls != 1 {
		t.Fatalf("trigger ran %d polls, want exactly 1", polls)
	}
	a := pollctl.ReadAck(path)
	if a.DoneSeq != 1 || a.Outcome != pollctl.OutcomeRan || !a.Held {
		t.Fatalf("trigger ack = %+v, want done_seq=1 outcome=ran held", a)
	}

	// Automatic polls stay held after the trigger.
	if err := s.ordinary(ranCycle(&polls)); err != nil {
		t.Fatal(err)
	}
	if polls != 1 {
		t.Fatalf("automatic poll ran while held after a trigger (%d polls)", polls)
	}
}

func TestPollSeam_TriggerOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		results   []PollBackoffResult
		err       error
		wantOut   string
		wantCalls int32
	}{
		{"error", []PollBackoffResult{{}}, errors.New("boom"), pollctl.OutcomeError, 1},
		{"rest gate blocked", []PollBackoffResult{{NextInterval: time.Hour}}, nil, pollctl.OutcomeBlocked, 1},
		{"floor then ran", []PollBackoffResult{{NextInterval: time.Millisecond}, {NextInterval: time.Millisecond}, {Ran: true, NextInterval: time.Second}}, nil, pollctl.OutcomeRan, 3},
		{"floor persists", []PollBackoffResult{{NextInterval: time.Millisecond}}, nil, pollctl.OutcomeBlocked, pollSeamFloorRetries + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, path := seamForTest(t)
			var calls int32
			s.cycle = func() (PollBackoffResult, error) {
				i := int(atomic.AddInt32(&calls, 1)) - 1
				if i >= len(tc.results) {
					i = len(tc.results) - 1
				}
				return tc.results[i], tc.err
			}
			if err := pollctl.WriteRequest(path, pollctl.Request{TriggerSeq: 1}); err != nil {
				t.Fatal(err)
			}
			s.step(context.Background())
			a := pollctl.ReadAck(path)
			if a.Outcome != tc.wantOut || a.DoneSeq != 1 {
				t.Fatalf("ack = %+v, want outcome %q done_seq 1", a, tc.wantOut)
			}
			if calls != tc.wantCalls {
				t.Fatalf("cycle calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// A trigger arriving while an ordinary poll is running waits for it; the two
// never overlap.
func TestPollSeam_TriggerDuringRunningPollDoesNotOverlap(t *testing.T) {
	s, path := seamForTest(t)
	var running, maxRunning, calls int32
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	s.cycle = func() (PollBackoffResult, error) {
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		<-release
		atomic.AddInt32(&running, -1)
		return PollBackoffResult{Ran: true, NextInterval: time.Second}, nil
	}

	ordinaryDone := make(chan struct{})
	go func() {
		_ = s.ordinary(s.cycle)
		close(ordinaryDone)
	}()
	<-started

	if err := pollctl.WriteRequest(path, pollctl.Request{TriggerSeq: 1}); err != nil {
		t.Fatal(err)
	}
	stepDone := make(chan struct{})
	go func() {
		s.step(context.Background())
		close(stepDone)
	}()
	time.Sleep(100 * time.Millisecond)
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Fatalf("trigger started a poll while one was running (calls=%d)", c)
	}

	release <- struct{}{} // finish the ordinary poll
	<-ordinaryDone
	<-started // the triggered poll now runs
	release <- struct{}{}
	<-stepDone

	if m := atomic.LoadInt32(&maxRunning); m != 1 {
		t.Fatalf("max concurrent polls = %d, want 1", m)
	}
	if a := pollctl.ReadAck(path); a.DoneSeq != 1 || a.Outcome != pollctl.OutcomeRan {
		t.Fatalf("ack = %+v", a)
	}
}

// The hold ack is the harness's guarantee that the bed is quiescent: it must not
// be written until an in-flight poll has finished.
func TestPollSeam_HoldAckWaitsForInFlightPoll(t *testing.T) {
	s, path := seamForTest(t)
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = s.ordinary(func() (PollBackoffResult, error) {
			close(started)
			<-release
			return PollBackoffResult{Ran: true}, nil
		})
	}()
	<-started

	req := pollctl.Request{Hold: true, HoldUntil: time.Now().Add(time.Minute).Unix(), HoldGen: 1}
	if err := pollctl.WriteRequest(path, req); err != nil {
		t.Fatal(err)
	}
	stepDone := make(chan struct{})
	go func() {
		s.step(context.Background())
		close(stepDone)
	}()
	time.Sleep(100 * time.Millisecond)
	if a := pollctl.ReadAck(path); a.HoldGen != 0 || a.Held {
		t.Fatalf("hold acknowledged while a poll was still running: %+v", a)
	}
	close(release)
	<-stepDone
	if a := pollctl.ReadAck(path); !a.Held || a.HoldGen != 1 {
		t.Fatalf("hold ack after the poll finished = %+v", a)
	}
}

func TestPollSeam_HoldDeadlineSelfReleases(t *testing.T) {
	s, path := seamForTest(t)
	now := time.Unix(1_000_000, 0)
	s.nowFn = func() time.Time { return now }
	s.cycle = ranCycle(new(int32))

	req := pollctl.Request{Hold: true, HoldUntil: now.Add(time.Minute).Unix(), HoldGen: 1}
	if err := pollctl.WriteRequest(path, req); err != nil {
		t.Fatal(err)
	}
	s.step(context.Background())
	if !s.held.Load() {
		t.Fatal("not held after a live hold request")
	}
	now = now.Add(2 * time.Minute)
	s.step(context.Background())
	if s.held.Load() {
		t.Fatal("still held after hold_until passed")
	}
	if a := pollctl.ReadAck(path); a.Held {
		t.Fatalf("ack still reports held after self-release: %+v", a)
	}
}

func TestPollSeam_StartClearsStaleRequestAndBeginsReleased(t *testing.T) {
	s, path := seamForTest(t)
	if err := pollctl.WriteRequest(path, pollctl.Request{Hold: true, HoldGen: 9, TriggerSeq: 4}); err != nil {
		t.Fatal(err)
	}
	if err := pollctl.WriteAck(path, pollctl.Ack{Held: true, HoldGen: 9, DoneSeq: 4}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.start(ctx, ranCycle(new(int32)))
	cancel()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale request survived start (err=%v)", err)
	}
	// The stale ack is replaced by a fresh, released one — its existence is the
	// harness's "seam is live" signal.
	if _, err := os.Stat(pollctl.AckPath(path)); err != nil {
		t.Fatalf("no initial ack after start: %v", err)
	}
	if a := pollctl.ReadAck(path); a != (pollctl.Ack{}) {
		t.Fatalf("initial ack = %+v, want the zero (released) ack", a)
	}
	if s.held.Load() {
		t.Fatal("a restarted engine came up held")
	}
}

// --- Run()-level tests: the real loop, every trigger path ---

type runHarness struct {
	eng     *Engine
	fetches *int32
	wakeCh  chan struct{}
	done    chan error
	path    string
}

func startRunWithSeam(t *testing.T, seam bool) *runHarness {
	t.Helper()
	var fetches int32
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			atomic.AddInt32(&fetches, 1)
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir
	h := &runHarness{eng: eng, fetches: &fetches, path: pollctl.ControlPath(dir), wakeCh: make(chan struct{}, 4), done: make(chan error, 1)}
	if seam {
		eng.cfg.PollControlFile = h.path
	}
	eng.SetWakeCh(h.wakeCh)
	ready := make(chan struct{})
	eng.cfg.ReadyCh = ready
	go func() { h.done <- eng.Run() }()
	<-ready
	if seam {
		// The startup board check fetches before the seam starts, so a fetch count
		// is not readiness: the seam's initial ack is.
		waitUntil(t, "the seam's initial ack", 5*time.Second, func() bool {
			_, err := os.Stat(pollctl.AckPath(h.path))
			return err == nil
		})
	}
	return h
}

func (h *runHarness) stop(t *testing.T) {
	t.Helper()
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
}

func (h *runHarness) fetchCount() int32 { return atomic.LoadInt32(h.fetches) }

func (h *runHarness) request(t *testing.T, r pollctl.Request) {
	t.Helper()
	if err := pollctl.WriteRequest(h.path, r); err != nil {
		t.Fatal(err)
	}
}

func TestRun_PollSeamUnset_LoopUnchanged(t *testing.T) {
	h := startRunWithSeam(t, false)
	defer h.stop(t)

	waitUntil(t, "the startup poll", 5*time.Second, func() bool { return h.fetchCount() >= 1 })
	before := h.fetchCount()
	waitUntil(t, "a ticker poll", 5*time.Second, func() bool { return h.fetchCount() > before })

	// A wake still polls (once the floor has cleared).
	time.Sleep(600 * time.Millisecond)
	before = h.fetchCount()
	h.wakeCh <- struct{}{}
	waitUntil(t, "a wake poll", 5*time.Second, func() bool { return h.fetchCount() > before })

	for _, p := range []string{h.path, pollctl.AckPath(h.path)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("seam unset but %s exists (err=%v)", filepath.Base(p), err)
		}
	}
}

func TestRun_PollSeamHeld_NoPollAcrossTickerWakeAndTrigger(t *testing.T) {
	h := startRunWithSeam(t, true)
	defer h.stop(t)

	// The engine starts released: the startup poll runs.
	waitUntil(t, "the startup poll", 5*time.Second, func() bool { return h.fetchCount() >= 1 })

	until := time.Now().Add(time.Minute).Unix()
	req := pollctl.Request{Hold: true, HoldUntil: until, HoldGen: 1}
	h.request(t, req)
	waitUntil(t, "the hold ack", 5*time.Second, func() bool {
		a := pollctl.ReadAck(h.path)
		return a.Held && a.HoldGen == 1
	})

	baseline := h.fetchCount()
	// Wake path and ticker path (PollSeconds=1 → the ticker fires repeatedly).
	h.wakeCh <- struct{}{}
	time.Sleep(600 * time.Millisecond)
	h.wakeCh <- struct{}{}
	time.Sleep(2500 * time.Millisecond)
	if got := h.fetchCount(); got != baseline {
		t.Fatalf("held bed polled: board fetches %d → %d", baseline, got)
	}

	// One trigger → exactly one complete poll and an ack.
	req.TriggerSeq = 1
	h.request(t, req)
	waitUntil(t, "the trigger ack", 10*time.Second, func() bool { return pollctl.ReadAck(h.path).DoneSeq == 1 })
	if a := pollctl.ReadAck(h.path); a.Outcome != pollctl.OutcomeRan || !a.Held {
		t.Fatalf("trigger ack = %+v, want outcome ran and still held", a)
	}
	if got := h.fetchCount(); got != baseline+1 {
		t.Fatalf("trigger: board fetches %d → %d, want exactly one poll", baseline, got)
	}
	time.Sleep(1500 * time.Millisecond)
	if got := h.fetchCount(); got != baseline+1 {
		t.Fatalf("held bed polled after the trigger: %d fetches, want %d", got, baseline+1)
	}

	// Release → free-running again.
	req.Hold = false
	req.HoldGen = 2
	h.request(t, req)
	waitUntil(t, "the release ack", 5*time.Second, func() bool {
		a := pollctl.ReadAck(h.path)
		return !a.Held && a.HoldGen == 2
	})
	waitUntil(t, "automatic polling to resume", 10*time.Second, func() bool { return h.fetchCount() > baseline+1 })
}

// A hold must never block the drain.
func TestRun_PollSeamHeld_ShutdownDrainsPromptly(t *testing.T) {
	h := startRunWithSeam(t, true)
	waitUntil(t, "the startup poll", 5*time.Second, func() bool { return h.fetchCount() >= 1 })
	h.request(t, pollctl.Request{Hold: true, HoldUntil: time.Now().Add(time.Minute).Unix(), HoldGen: 1})
	waitUntil(t, "the hold ack", 5*time.Second, func() bool { return pollctl.ReadAck(h.path).Held })
	h.stop(t)
}

// reconcileLoop is poll-equivalent work: a held bed must not reconcile the
// cache (a state window must not move), and it must resume once released.
func TestReconcileLoop_PollSeamHeldDropsTick(t *testing.T) {
	t1 := time.Now().Truncate(time.Second)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.ReconcileInterval = 10 * time.Millisecond
	eng.pollSeam = newPollSeam(eng, pollctl.ControlPath(t.TempDir()))

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise"}, UpdatedAt: t1},
		},
	})
	eng.readClient = cache
	client.fetchProjectBoardFn = func(_, _ string, _ int, _ string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise", "fabrik:awaiting-ci"}, UpdatedAt: t1},
			},
		}, nil
	}
	hasLabel := func() bool {
		snap, err := eng.store.Get("owner/repo", 1)
		if err != nil {
			return false
		}
		for _, l := range snap.Labels() {
			if l == "fabrik:awaiting-ci" {
				return true
			}
		}
		return false
	}

	eng.pollSeam.setHeld(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.reconcileLoop(ctx, cache, nil)

	time.Sleep(300 * time.Millisecond) // ~30 ticks
	if hasLabel() {
		t.Fatal("a held bed reconciled the cache")
	}
	eng.pollSeam.setHeld(false)
	waitUntil(t, "reconcile to resume after release", 2*time.Second, hasLabel)
}
