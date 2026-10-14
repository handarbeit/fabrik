//go:build !windows

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// The Run()-level tests below deliver a real SIGHUP/SIGINT to the test process,
// so none of them is t.Parallel(), and each awaits ReadyCh first (#2259, ADR 2259).

type drainHarness struct {
	eng     *Engine
	done    chan error
	execs   atomic.Int32
	paused  atomic.Bool
	runCtx  context.Context
	ctxOnce sync.Once
	logMu   sync.Mutex
	logs    []string
}

func (h *drainHarness) logText() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return strings.Join(h.logs, "")
}

// startDrainRun starts Run() on an empty board with the exec stubbed out and
// returns once the signal handlers are registered.
func startDrainRun(t *testing.T) *drainHarness {
	t.Helper()
	h := &drainHarness{done: make(chan error, 1)}
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:paused" {
				h.paused.Store(true)
			}
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300
	eng.cfg.DrainDeadline = 300 * time.Millisecond
	eng.SetDrainPollIntervalForTest(10 * time.Millisecond)
	h.eng = eng

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng.fabrikDir = dir
	ready := make(chan struct{})
	eng.cfg.ReadyCh = ready
	eng.sighupExecFn = func(string, []string, []string) error {
		h.execs.Add(1)
		return nil
	}
	ctxCh := make(chan context.Context, 1)
	eng.SetRunCtxHookForTest(func(ctx context.Context) { ctxCh <- ctx })

	events := make(chan tui.Event, 256)
	eng.SetEvents(events)
	go func() {
		for ev := range events {
			if le, ok := ev.(tui.LogEvent); ok {
				h.logMu.Lock()
				h.logs = append(h.logs, le.Message)
				h.logMu.Unlock()
			}
		}
	}()

	go func() { h.done <- eng.Run() }()
	awaitRunReady(t, ready, h.done)
	h.runCtx = <-ctxCh
	return h
}

func signalSelf(t *testing.T, sig syscall.Signal) {
	t.Helper()
	p, _ := os.FindProcess(os.Getpid())
	if err := p.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// seedWorker mimics a dispatched worker: a store record, an e.wg slot and a
// context derived from Run()'s root context, exactly as dispatchCandidates
// derives issueCtx. It returns the worker's context, a release func, and a
// channel yielding whether the context was cancelled when the worker finished.
func (h *drainHarness) seedWorker(number int) (wctx context.Context, release func(), cancelledAtEnd <-chan bool) {
	wctx, wcancel := context.WithCancel(h.runCtx)
	rel := make(chan struct{})
	res := make(chan bool, 1)
	h.eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: number, StageName: "Implement", StartedAt: time.Now(), Kind: itemstate.WorkerKindStageRun})
	h.eng.wg.Add(1)
	go func() {
		defer h.eng.wg.Done()
		defer wcancel()
		select {
		case <-rel:
		case <-wctx.Done():
		}
		res <- wctx.Err() != nil
		h.eng.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: number})
	}()
	var once sync.Once
	return wctx, func() { once.Do(func() { close(rel) }) }, res
}

func (h *drainHarness) awaitRunReturn(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return in time")
	}
}

// steady asserts cond stays false for a short window.
func steady(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("unexpected: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Acceptance (a): the first SIGHUP leaves a mid-run worker's context uncancelled,
// dispatches nothing new, and the re-exec runs only after the worker returns.
func TestSighupDrain_FirstSighupLetsWorkerFinishThenRestarts(t *testing.T) {
	h := startDrainRun(t)
	eng := h.eng
	wctx, release, cancelledAtEnd := h.seedWorker(7)
	defer release()

	signalSelf(t, syscall.SIGHUP)
	waitUntil(t, "the drain to report one worker left", 5*time.Second, func() bool {
		s := eng.health.snapshot()
		return s.Draining && s.DrainLeft == 1
	})

	// The worker keeps running: neither its context nor the root's is cancelled,
	// nothing is exec'd and Run has not returned.
	steady(t, "the worker/root context cancelled, exec run, or Run returned during the drain", func() bool {
		return wctx.Err() != nil || h.runCtx.Err() != nil || h.execs.Load() != 0 || len(h.done) != 0
	})

	// Nothing new starts while draining.
	var probed atomic.Bool
	eng.dispatchReinvoke(h.runCtx, nil, gh.ProjectItem{Number: 99, Repo: "owner/repo"}, &stages.Stage{Name: "Implement"},
		reinvokeOpts{tag: "t", precheck: func() bool { probed.Store(true); return false }})
	if probed.Load() {
		t.Fatal("a reinvoke was dispatched during the drain")
	}

	// The status surfaces say so.
	res, err := eng.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil || res.Draining == nil || res.Draining.WorkersRemaining != 1 {
		t.Fatalf("fabrik_health must report draining with 1 worker left: %+v, %v", res, err)
	}

	release()
	waitUntil(t, "the re-exec after the worker returned", 5*time.Second, func() bool { return h.execs.Load() == 1 })
	h.awaitRunReturn(t)
	if cancelled := <-cancelledAtEnd; cancelled {
		t.Fatal("the worker's context was cancelled by the first SIGHUP")
	}
	if h.paused.Load() {
		t.Fatal("a drained restart applied fabrik:paused")
	}
	if !strings.Contains(h.logText(), "send SIGHUP again to restart now") {
		t.Fatalf("second-SIGHUP hint not logged:\n%s", h.logText())
	}
}

// Acceptance (b): a second SIGHUP cancels the worker and re-execs, without exit 1;
// a third is a no-op.
func TestSighupDrain_SecondSighupCancelsAndRestarts(t *testing.T) {
	h := startDrainRun(t)
	eng := h.eng
	wctx, release, cancelledAtEnd := h.seedWorker(8)
	defer release()

	signalSelf(t, syscall.SIGHUP)
	waitUntil(t, "the drain to begin", 5*time.Second, func() bool { return eng.health.snapshot().Draining })
	steady(t, "the worker cancelled by the first SIGHUP", func() bool { return wctx.Err() != nil })

	signalSelf(t, syscall.SIGHUP)
	signalSelf(t, syscall.SIGHUP) // third: no additional effect
	waitUntil(t, "the worker context to be cancelled", 5*time.Second, func() bool { return wctx.Err() != nil })
	waitUntil(t, "the re-exec", 5*time.Second, func() bool { return h.execs.Load() >= 1 })
	h.awaitRunReturn(t)
	if !<-cancelledAtEnd {
		t.Fatal("the second SIGHUP did not cancel the worker")
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.execs.Load(); n != 1 {
		t.Fatalf("exec ran %d times, want exactly 1", n)
	}
	if h.paused.Load() {
		t.Fatal("a SIGHUP restart applied fabrik:paused")
	}
}

// FR-008: a SIGINT during a SIGHUP drain stops; it must not become a restart.
func TestSighupDrain_SigintDuringDrainStopsWithoutExec(t *testing.T) {
	h := startDrainRun(t)
	eng := h.eng
	_, release, _ := h.seedWorker(9)
	defer release()

	signalSelf(t, syscall.SIGHUP)
	waitUntil(t, "the drain to begin", 5*time.Second, func() bool { return eng.health.snapshot().Draining })
	signalSelf(t, syscall.SIGINT)
	waitUntil(t, "the run context to be cancelled by the SIGINT", 5*time.Second, func() bool { return h.runCtx.Err() != nil })
	release()
	h.awaitRunReturn(t)
	if n := h.execs.Load(); n != 0 {
		t.Fatalf("SIGINT during a SIGHUP drain re-exec'd (%d)", n)
	}
}

// No workers: the drain ends at once.
func TestSighupDrain_IdleRestartsAtOnce(t *testing.T) {
	h := startDrainRun(t)
	signalSelf(t, syscall.SIGHUP)
	h.awaitRunReturn(t)
	if n := h.execs.Load(); n != 1 {
		t.Fatalf("exec ran %d times, want 1", n)
	}
}

// The in-flight set is logged on the first SIGHUP and repeated until empty; the
// hint is logged once.
func TestSighupDrain_LogsInFlightSetAndRepeats(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	var mu sync.Mutex
	var msgs []string
	events := make(chan tui.Event, 256)
	eng.SetEvents(events)
	go func() {
		for ev := range events {
			if le, ok := ev.(tui.LogEvent); ok {
				mu.Lock()
				msgs = append(msgs, le.Message)
				mu.Unlock()
			}
		}
	}()
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, m := range msgs {
			if strings.Contains(m, sub) {
				n++
			}
		}
		return n
	}

	eng.SetDrainLogIntervalForTest(20 * time.Millisecond)
	eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 5, StageName: "Implement", StartedAt: time.Now().Add(-90 * time.Second), Kind: itemstate.WorkerKindCIFixReinvoke})
	stop := make(chan struct{})
	defer close(stop)
	eng.onFirstSighup(stop)
	eng.onFirstSighup(stop) // a repeat must not log the hint again

	waitUntil(t, "the in-flight line to repeat", 3*time.Second, func() bool { return count("1 worker(s) in flight") >= 3 })
	if got := count("send SIGHUP again to restart now"); got != 1 {
		t.Fatalf("hint logged %d times, want 1", got)
	}
	mu.Lock()
	line := ""
	for _, m := range msgs {
		if strings.Contains(m, "worker(s) in flight") {
			line = m
			break
		}
	}
	mu.Unlock()
	for _, want := range []string{"#5", "Implement", itemstate.WorkerKindCIFixReinvoke, "1m"} {
		if !strings.Contains(line, want) {
			t.Fatalf("in-flight line %q lacks %q", line, want)
		}
	}

	eng.store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 5})
	time.Sleep(100 * time.Millisecond)
	n := count("worker(s) in flight")
	time.Sleep(150 * time.Millisecond)
	if after := count("worker(s) in flight"); after != n {
		t.Fatalf("the repeat did not stop once the set was empty (%d -> %d)", n, after)
	}
}

// Merge-train accounting: a run waiting on CI is not a worker; a step goroutine
// (repo worker) or a conflict resolution is.
func TestSighupDrain_WhatCountsAsInFlight(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if n := len(eng.drainWorkers()); n != 0 {
		t.Fatalf("idle engine reports %d workers", n)
	}
	eng.store.EnterRepoWorker("owner/repo")
	if n := len(eng.drainWorkers()); n != 1 {
		t.Fatalf("a train step goroutine must count, got %d", n)
	}
	eng.store.ExitRepoWorker("owner/repo")
	eng.store.Apply(itemstate.ConflictResolutionSet{Repo: "owner/repo", Number: 3, Active: true, Since: time.Now()})
	ws := eng.drainWorkers()
	if len(ws) != 1 || ws[0].Kind != itemstate.WorkerKindTrainConflict {
		t.Fatalf("a conflict resolution must count, got %+v", ws)
	}
}

// Every dispatch site refuses while draining; each assertion is non-vacuous
// because SetDrainDispatchGateDisabledForTest(true) makes it fail.
func TestSighupDrain_DispatchGates(t *testing.T) {
	for _, gateOff := range []bool{false, true} {
		name := "gate-on"
		if gateOff {
			name = "gate-off"
		}
		t.Run(name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
					return &gh.ProjectBoard{}, nil
				},
			}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			eng.SetDrainDispatchGateDisabledForTest(gateOff)
			eng.beginDrain()

			// Reinvokes (review / CI-fix / rebase share the scaffold).
			var probed bool
			eng.dispatchReinvoke(context.Background(), nil, gh.ProjectItem{Number: 1, Repo: "owner/repo"}, &stages.Stage{Name: "Implement"},
				reinvokeOpts{tag: "t", precheck: func() bool { probed = true; return false }})
			if probed == !gateOff {
				t.Errorf("reinvoke gate: precheck ran = %v with gate off = %v", probed, gateOff)
			}

			// Merge-train conflict resolution: an uncharged "not attempted".
			_, _, err := eng.resolveConflictWithClaude(context.Background(), "owner/repo", gh.ProjectItem{Number: 2, Repo: "owner/repo"}, t.TempDir(), &stages.Stage{Name: "Queued"}, "sha", nil, "", nil, InvokeOptions{})
			refused := errors.Is(err, errDrainingNotAttempted)
			if refused == gateOff {
				t.Errorf("conflict-resolution gate: refused = %v with gate off = %v (err %v)", refused, gateOff, err)
			}
			if refused && !trainCancelled(context.Background(), err) {
				t.Error("a drain-refused conflict resolution must read as 'not attempted' (no ejection, no charge)")
			}

			// Polling.
			var fetches atomic.Int32
			client.fetchProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
				fetches.Add(1)
				return &gh.ProjectBoard{}, nil
			}
			_, _ = eng.PollWithBackoff(context.Background(), time.Second)
			if (fetches.Load() == 0) == gateOff {
				t.Errorf("poll gate: fetches = %d with gate off = %v", fetches.Load(), gateOff)
			}
		})
	}
}

// A reinvoke queued behind a full slot pool has started no run: the drain
// abandons it at once and the new process re-dispatches it.
func TestSighupDrain_QueuedReinvokeIsAbandoned(t *testing.T) {
	for _, gateOff := range []bool{false, true} {
		eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
		eng.SetDrainDispatchGateDisabledForTest(gateOff)
		for i := 0; i < cap(eng.sem); i++ {
			eng.sem <- struct{}{}
		}
		eng.dispatchReinvoke(context.Background(), nil, gh.ProjectItem{Number: 4, Repo: "owner/repo"}, &stages.Stage{Name: "Implement"}, reinvokeOpts{tag: "t"})
		if snap, err := eng.store.Get("owner/repo", 4); err != nil || snap.Worker() == nil || !snap.Worker().Queued {
			t.Fatalf("setup: expected a queued worker, got %v %v", snap, err)
		}
		eng.beginDrain()
		gone := make(chan struct{})
		go func() { eng.wg.Wait(); close(gone) }()
		select {
		case <-gone:
			if gateOff {
				t.Fatal("gate off: the queued reinvoke was abandoned anyway")
			}
		case <-time.After(300 * time.Millisecond):
			if !gateOff {
				t.Fatal("the queued reinvoke was not abandoned by the drain")
			}
			// Gate off: the worker is still blocked on a slot, as before the change
			// (left blocked; the test process exits).
		}
	}
}

// A drain is not a poll stall.
func TestSighupDrain_PollVerdictUnknownWhileDraining(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1
	now := time.Now()
	h := healthSnapshot{StartedAt: now.Add(-time.Hour), NextPollDueAt: now.Add(-time.Hour)}
	if v := eng.pollVerdictFor(h, now); !v.Overdue {
		t.Fatalf("setup: expected an overdue verdict, got %+v", v)
	}
	h.Draining = true
	if v := eng.pollVerdictFor(h, now); v.Known || v.Overdue {
		t.Fatalf("a drain must read as unknown, got %+v", v)
	}
}

// The slot publisher carries the drain into the TUI title event.
func TestSighupDrain_SlotsEventCarriesDrain(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	events := make(chan tui.Event, 16)
	eng.SetEvents(events)
	eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Implement", StartedAt: time.Now()})
	eng.beginDrain()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.runWorkerSlotsPublisher(ctx, time.Hour)
	select {
	case ev := <-events:
		se, ok := ev.(tui.WorkerSlotsEvent)
		if !ok || !se.Draining || se.DrainLeft != 1 {
			t.Fatalf("event = %#v, want Draining with 1 left", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no WorkerSlotsEvent")
	}
}

// A drain starts no stage run or comment pass: dispatchCandidates (the original
// $5.76 loss path) enters no worker while draining, and does with the gate off.
func TestSighupDrain_DispatchCandidatesStartsNothing(t *testing.T) {
	for _, gateOff := range []bool{false, true} {
		eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
		eng.cfg.MaxConcurrent = 1
		eng.sem = make(chan struct{}, 1)
		eng.SetDrainDispatchGateDisabledForTest(gateOff)
		eng.beginDrain()

		board := &gh.ProjectBoard{ProjectID: "PVT_1"}
		item := gh.ProjectItem{Number: 3, Title: "Test", Status: "Research"}
		dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
		eng.wg.Wait()

		if started := dispatched == 1; started != gateOff {
			t.Errorf("gate off = %v: dispatched = %d", gateOff, dispatched)
		}
		if !gateOff && len(eng.drainWorkers()) != 0 {
			t.Errorf("a worker entered the store during a drain: %+v", eng.drainWorkers())
		}
	}
}

// A drain starts no merge-train worker.
func TestSighupDrain_DispatchMergeTrainWorkerStartsNothing(t *testing.T) {
	for _, gateOff := range []bool{false, true} {
		eng, _, v, _ := infraSeamEngine(t, TrainCIInfra)
		eng.SetDrainDispatchGateDisabledForTest(gateOff)
		eng.beginDrain()
		eng.dispatchMergeTrainWorker(context.Background(), makeSeamBatch(2), "PVT_test", "main")
		eng.wg.Wait()
		if ran := v.count() > 0; ran != gateOff {
			t.Errorf("gate off = %v: validations = %d", gateOff, v.count())
		}
		if !gateOff {
			if _, ok := eng.mergeTrainInFlight.Load(mergeTrainKey("owner/repo", "main")); ok {
				t.Error("a train worker claim was registered during a drain")
			}
		}
	}
}

// A drain starts no train step goroutine: the claim on the run is released and
// no repo worker is entered, so the persisted run resumes after the exec.
func TestSighupDrain_LaunchStepStartsNothing(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.beginDrain()
	// Detached, parked on an open trial: a step that did start would return at once
	// and (detached) leave its repo-worker marker behind, which is what the test sees.
	r := &trainRun{e: eng, trainKey: "owner/repo:main", detached: true, rec: trainRunRecord{Step: stepMain}}
	r.stepping.Store(true)
	eng.launchStep(context.Background(), r, nil)
	eng.wg.Wait()
	if r.stepping.Load() {
		t.Error("launchStep kept the run claimed during a drain")
	}
	if keys := eng.store.RepoWorkerKeys(); len(keys) != 0 {
		t.Errorf("a repo worker was entered during a drain: %v", keys)
	}
}

// Health reports the live in-flight count, not the last value the drain published.
func TestSighupDrain_HealthCountsLive(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.beginDrain()
	eng.publishDrainStatus() // mirror says 0 left
	eng.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 6, StageName: "Implement", StartedAt: time.Now(), Kind: itemstate.WorkerKindStageRun})
	res, err := eng.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil || res.Draining == nil || res.Draining.WorkersRemaining != 1 {
		t.Fatalf("want 1 worker remaining from the live Store, got %+v, %v", res.Draining, err)
	}
}
