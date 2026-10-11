package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Focused tests for the deep-fetch pre-filter loop and dispatch loop extracted
// out of poll() (#1029). These call the helpers directly instead of driving the
// whole poll() cycle (board fetch, cache refresh, rate-limit stats, catch-up
// loop), which is exactly the isolated testability the decomposition enables.

func TestSelectDeepFetchCandidates_CleanupStageSkipsDeepFetch(t *testing.T) {
	var fetchDetailsCalled bool
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchDetailsCalled = true
			return nil
		},
	}

	// itemMayNeedWork admits a cleanup-stage item only when its worktree
	// directory exists on disk — create one so the item reaches the loop body.
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "issue-42"), 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	wm := NewWorktreeManagerWithRoot(t.TempDir(), rootDir)

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			Stages: testStagesWithCleanup()},
		client, &mockClaudeInvoker{}, wm,
	)

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 42, Title: "Done item", Status: "Done"},
		},
	}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", map[string]bool{}, map[string]bool{})

	if fetchDetailsCalled {
		t.Error("FetchItemDetails must not be called for cleanup-stage items")
	}
	if deepFetched != 0 {
		t.Errorf("deepFetched = %d, want 0 (cleanup-stage admission doesn't count as a deep-fetch)", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 42 {
		t.Fatalf("expected the cleanup-stage item to be admitted as a candidate, got %+v", candidates)
	}
}

func TestSelectDeepFetchCandidates_DeepFetchesEligibleItem(t *testing.T) {
	var fetchedNumber int
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchedNumber = item.Number
			item.Labels = []string{"stage:Research:in_progress"}
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 7, Title: "New item", Status: "Research"},
		},
	}
	// cycleSet marks the item as changed, so it passes the pre-filter regardless
	// of store/cooldown state.
	cycleSet := map[string]bool{issueKey(board.Items[0], eng.defaultRepo()): true}

	candidates, deepFetched := eng.selectDeepFetchCandidates(board, "", cycleSet, map[string]bool{})

	if fetchedNumber != 7 {
		t.Errorf("FetchItemDetails was not called for the eligible item (fetchedNumber=%d)", fetchedNumber)
	}
	if deepFetched != 1 {
		t.Errorf("deepFetched = %d, want 1", deepFetched)
	}
	if len(candidates) != 1 || candidates[0].Number != 7 {
		t.Fatalf("expected item #7 to be a candidate, got %+v", candidates)
	}
}

func TestSelectDeepFetchCandidates_RepoFilterExcludesOtherRepos(t *testing.T) {
	var fetchDetailsCalled bool
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			fetchDetailsCalled = true
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 1, Title: "Other repo item", Status: "Research", Repo: "other/repo"},
		},
	}
	cycleSet := map[string]bool{issueKey(board.Items[0], eng.defaultRepo()): true}

	candidates, _ := eng.selectDeepFetchCandidates(board, "owner/repo", cycleSet, map[string]bool{})

	if fetchDetailsCalled {
		t.Error("FetchItemDetails must not be called for items outside repoFilter")
	}
	if len(candidates) != 0 {
		t.Errorf("expected no candidates from a filtered-out repo, got %d", len(candidates))
	}
}

func TestDispatchCandidates_DispatchesEligibleItem(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 1
	eng.sem = make(chan struct{}, 1)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 3, Title: "Test", Status: "Research"}

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
	eng.wg.Wait()

	if dispatched != 1 {
		t.Errorf("dispatched = %d, want 1", dispatched)
	}
}

func TestDispatchCandidates_SkipsInFlightWorker(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 1
	eng.sem = make(chan struct{}, 1)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 9, Title: "Test", Status: "Research"}
	eng.store.Apply(itemstate.WorkerEntered{
		Repo:      itemOwnerRepoString(item, eng.defaultRepo()),
		Number:    item.Number,
		StageName: "Research",
		StartedAt: time.Now(),
	})

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})

	if dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 (item already has an in-flight worker)", dispatched)
	}
}

// TestDispatchCandidates_RefusesLiveSentinelAfterClear is the R5/Acceptance-5
// regression: reproduces the reported shape — a prior worker for this
// (issue, stage) was cleared (Worker() == nil in the store, exactly as
// worker-liveness leaves it after a timeout-based clear), but a real process
// carrying that worker's sentinel is still live. Dispatch must refuse to
// start a second worker even though the store-level in-flight guard alone
// would have let it through.
func TestDispatchCandidates_RefusesLiveSentinelAfterClear(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 1
	eng.sem = make(chan struct{}, 1)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 12, Title: "Test", Status: "Research"}
	// No Worker in the store — mirrors the state right after worker-liveness
	// clears a worker (WorkerExited already applied).

	// The fake process table carries the item's own sentinel — proving both
	// that dispatchCandidates constructs the expected sentinel value AND that
	// the batched match finds it: if the wrong sentinel were computed, this
	// fake process wouldn't match it and dispatch would proceed (dispatched
	// would be 1, failing the assertion below).
	wantSentinel := sessionNameSentinel(itemOwnerRepoString(item, eng.defaultRepo()), item.Number, "Research")
	withProcessListProbe(t, true, []procArgvEntry{{PID: 55555, Argv: []string{"--name", wantSentinel}}}, nil)

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
	eng.wg.Wait()

	if dispatched != 0 {
		t.Errorf("dispatched = %d, want 0 (a live sentinel must refuse dispatch even with Worker() == nil)", dispatched)
	}
	if snap, err := eng.store.Get(itemOwnerRepoString(item, eng.defaultRepo()), item.Number); err == nil && snap.Worker() != nil {
		t.Error("expected no WorkerEntered mutation to have been applied for a refused dispatch")
	}
}

// TestDispatchCandidates_DispatchesWhenSentinelNotFound verifies the R5
// guard's complement: when the probe affirmatively finds no live sentinel,
// dispatch proceeds normally.
func TestDispatchCandidates_DispatchesWhenSentinelNotFound(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 1
	eng.sem = make(chan struct{}, 1)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 13, Title: "Test", Status: "Research"}

	withProcessListProbe(t, true, nil, nil)

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
	eng.wg.Wait()

	if dispatched != 1 {
		t.Errorf("dispatched = %d, want 1 (no live sentinel found — dispatch must proceed)", dispatched)
	}
}

// TestDispatchCandidates_DispatchesOnProbeError verifies R5 fails OPEN on a
// probe error: a broken `ps` must not wedge dispatch for every item on every
// poll, which would be a strictly worse failure mode than the rare
// duplicate-writer this guard exists to prevent.
func TestDispatchCandidates_DispatchesOnProbeError(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 1
	eng.sem = make(chan struct{}, 1)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 14, Title: "Test", Status: "Research"}

	withProcessListProbe(t, true, nil, errSentinelProbeUnsupported)

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
	eng.wg.Wait()

	if dispatched != 1 {
		t.Errorf("dispatched = %d, want 1 (a probe error must fail open, not block dispatch)", dispatched)
	}
}

// TestDispatchCandidates_BatchesProcessListFetchAcrossCandidates is the
// review-finding regression for #1779: the R5 sentinel check must fetch the
// live process table at most once per dispatchCandidates call, reusing it
// for every dispatch-eligible item, not once per item — the original
// implementation spawned one `ps` subprocess per candidate, serially, before
// any of them could be dispatched. Neutralizing the batching (reverting to a
// per-item fetch) makes fetchCount == 3 instead of 1, failing this test.
func TestDispatchCandidates_BatchesProcessListFetchAcrossCandidates(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 5
	eng.sem = make(chan struct{}, 5)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	items := []gh.ProjectItem{
		{Number: 20, Title: "A", Status: "Research"},
		{Number: 21, Title: "B", Status: "Research"},
		{Number: 22, Title: "C", Status: "Research"},
	}

	var fetchCount int
	origSupported := claudeNameFlagSupported
	origFn := listProcessArgvFn
	claudeNameFlagSupported = true
	listProcessArgvFn = func() ([]procArgvEntry, error) {
		fetchCount++
		return nil, nil
	}
	t.Cleanup(func() {
		claudeNameFlagSupported = origSupported
		listProcessArgvFn = origFn
	})

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, items)
	eng.wg.Wait()

	if dispatched != 3 {
		t.Fatalf("dispatched = %d, want 3", dispatched)
	}
	if fetchCount != 1 {
		t.Errorf("listProcessArgvFn called %d time(s) for 3 dispatch-eligible items, want 1 (process table must be fetched once per dispatchCandidates call, not once per item)", fetchCount)
	}
}

// TestDispatchCandidates_SkipsProcessListFetchWhenNoCandidatesNeedIt verifies
// the fetch stays lazy: when every candidate already has an in-flight worker
// (never reaches the R5 check), listProcessArgvFn must not be invoked at all.
func TestDispatchCandidates_SkipsProcessListFetchWhenNoCandidatesNeedIt(t *testing.T) {
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, &mockGitHubClient{}, claude)
	eng.cfg.MaxConcurrent = 5
	eng.sem = make(chan struct{}, 5)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 23, Title: "Test", Status: "Research"}
	itemRepo := itemOwnerRepoString(item, eng.defaultRepo())
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       itemRepo,
		Number:     item.Number,
		User:       eng.cfg.User,
		AcquiredAt: time.Now(),
		Worker:     &itemstate.WorkerHandle{StageName: "Research", StartedAt: time.Now(), LastSignAt: time.Now()},
	})

	var fetchCount int
	origSupported := claudeNameFlagSupported
	origFn := listProcessArgvFn
	claudeNameFlagSupported = true
	listProcessArgvFn = func() ([]procArgvEntry, error) {
		fetchCount++
		return nil, nil
	}
	t.Cleanup(func() {
		claudeNameFlagSupported = origSupported
		listProcessArgvFn = origFn
	})

	dispatched, _ := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
	eng.wg.Wait()

	if dispatched != 0 {
		t.Fatalf("dispatched = %d, want 0 (item already has an in-flight worker)", dispatched)
	}
	if fetchCount != 0 {
		t.Errorf("listProcessArgvFn called %d time(s), want 0 (no candidate reached the R5 check)", fetchCount)
	}
}

// fullSemEngine returns a test engine whose worker semaphore has every slot held.
func fullSemEngine(t *testing.T, client *mockGitHubClient, slots int) *Engine {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxConcurrent = slots
	eng.sem = make(chan struct{}, slots)
	for i := 0; i < slots; i++ {
		eng.sem <- struct{}{}
	}
	return eng
}

// TestDispatchCandidates_FullSemaphore_DefersWithoutBlocking is the #2114 R1
// regression: with every slot held, dispatchCandidates must return promptly
// (the old blocking acquire never returned), dispatch nothing, and report the
// deferred keys. It fails (times out) against the blocking acquire.
func TestDispatchCandidates_FullSemaphore_DefersWithoutBlocking(t *testing.T) {
	withProcessListProbe(t, false, nil, nil)
	client := &mockGitHubClient{}
	eng := fullSemEngine(t, client, 2)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	items := []gh.ProjectItem{
		{Number: 31, Title: "A", Status: "Research"},
		{Number: 32, Title: "B", Status: "Research"},
	}

	type result struct {
		dispatched int
		deferred   []string
	}
	var res result
	var timedOut bool
	out := captureStdout(func() {
		done := make(chan result, 1)
		go func() {
			d, def := eng.dispatchCandidates(context.Background(), board, items)
			done <- result{d, def}
		}()
		select {
		case res = <-done:
		case <-time.After(5 * time.Second):
			timedOut = true
		}
	})
	if timedOut {
		t.Fatal("dispatchCandidates blocked on a full worker semaphore")
	}

	if res.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0", res.dispatched)
	}
	want := []string{issueKey(items[0], eng.defaultRepo()), issueKey(items[1], eng.defaultRepo())}
	if len(res.deferred) != 2 || res.deferred[0] != want[0] || res.deferred[1] != want[1] {
		t.Errorf("deferred = %v, want %v", res.deferred, want)
	}
	if n := strings.Count(out, "deferred dispatch"); n != 1 {
		t.Errorf("deferral log line appeared %d times, want exactly 1; output:\n%s", n, out)
	}
	if !strings.Contains(out, "#31, #32") || !strings.Contains(out, "(2/2 in use)") {
		t.Errorf("deferral log line should name the issues and in_use/max; got:\n%s", out)
	}

	// No side effects against the deferred items.
	for _, it := range items {
		snap, err := eng.store.Get(itemOwnerRepoString(it, eng.defaultRepo()), it.Number)
		if err != nil {
			continue
		}
		if snap.Worker() != nil {
			t.Errorf("#%d: deferred item must not have a worker entry", it.Number)
		}
		if !snap.LastAttemptAt("Research").IsZero() {
			t.Errorf("#%d: deferred item must have no LastAttemptAt", it.Number)
		}
		if snap.HasActiveCooldown(eng.now()) {
			t.Errorf("#%d: deferred item must have no cooldown", it.Number)
		}
	}
	client.mu.Lock()
	labelCalls := len(client.addLabelCalls)
	client.mu.Unlock()
	if labelCalls != 0 {
		t.Errorf("AddLabelToIssue called %d times for deferred items, want 0", labelCalls)
	}
	if len(eng.sem) != 2 {
		t.Errorf("sem occupancy = %d, want 2 (deferral must not take a slot)", len(eng.sem))
	}
}

// TestDispatchCandidates_PartialSemaphore_DefersOverflow: with one free slot the
// first candidate dispatches and the rest are deferred; guard-skipped candidates
// in between cost nothing extra.
func TestDispatchCandidates_PartialSemaphore_DefersOverflow(t *testing.T) {
	withProcessListProbe(t, false, nil, nil)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.MaxConcurrent = 2
	eng.sem = make(chan struct{}, 2)
	eng.sem <- struct{}{} // one slot busy

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	items := []gh.ProjectItem{
		{Number: 41, Title: "A", Status: "Research"},
		{Number: 42, Title: "B", Status: "Research"},
		{Number: 43, Title: "C", Status: "Research"},
	}
	dispatched, deferred := eng.dispatchCandidates(context.Background(), board, items)
	eng.wg.Wait()

	if dispatched != 1 {
		t.Errorf("dispatched = %d, want 1", dispatched)
	}
	if len(deferred) != 2 {
		t.Errorf("deferred = %v, want 2 keys", deferred)
	}
}

// TestDispatchCandidates_CancelledContextReturnsEarly: with the blocking select
// gone, ctx cancellation must still abort the loop.
func TestDispatchCandidates_CancelledContextReturnsEarly(t *testing.T) {
	withProcessListProbe(t, false, nil, nil)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.MaxConcurrent = 2
	eng.sem = make(chan struct{}, 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	dispatched, deferred := eng.dispatchCandidates(ctx, board, []gh.ProjectItem{{Number: 51, Title: "A", Status: "Research"}})
	eng.wg.Wait()

	if dispatched != 0 || len(deferred) != 0 {
		t.Errorf("cancelled ctx: dispatched=%d deferred=%v, want 0 and none", dispatched, deferred)
	}
}

func TestRequeueDeferredDispatch_AddsKeysWithoutWake(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.requeueDeferredDispatch(nil)
	eng.requeueDeferredDispatch([]string{"o/r#7", "o/r#8"})

	eng.mayNeedWorkMu.Lock()
	defer eng.mayNeedWorkMu.Unlock()
	if !eng.mayNeedWork["o/r#7"] || !eng.mayNeedWork["o/r#8"] {
		t.Errorf("mayNeedWork = %v, want both keys", eng.mayNeedWork)
	}
}
