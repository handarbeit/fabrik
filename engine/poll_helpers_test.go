package engine

import (
	"context"
	"os"
	"path/filepath"
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

	dispatched := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
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

	dispatched := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})

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

	dispatched := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
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

	dispatched := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
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

	dispatched := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
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

	dispatched := eng.dispatchCandidates(context.Background(), board, items)
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

	dispatched := eng.dispatchCandidates(context.Background(), board, []gh.ProjectItem{item})
	eng.wg.Wait()

	if dispatched != 0 {
		t.Fatalf("dispatched = %d, want 0 (item already has an in-flight worker)", dispatched)
	}
	if fetchCount != 0 {
		t.Errorf("listProcessArgvFn called %d time(s), want 0 (no candidate reached the R5 check)", fetchCount)
	}
}
