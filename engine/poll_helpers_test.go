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
