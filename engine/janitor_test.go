package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// janitorTestSetup creates a minimal environment for janitor tests.
// Returns the engine (with fabrikDir set), a WorktreeManager backed by a real
// git repo, and the worktrees root directory.
func janitorTestSetup(t *testing.T) (eng *Engine, wm *WorktreeManager, worktreesRoot string) {
	t.Helper()
	skipIfNoGit(t)

	fabrikDir := t.TempDir()
	repoDir := initBareRepo(t) // real git repo; used as wm.baseDir

	worktreesRoot = filepath.Join(fabrikDir, ".fabrik", "worktrees")
	if err := os.MkdirAll(worktreesRoot, 0755); err != nil {
		t.Fatalf("mkdir worktreesRoot: %v", err)
	}

	client := &mockGitHubClient{}
	stagesWithDone := []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
		{Name: "Implement", Order: 3},
		{Name: "Done", Order: 99, CleanupWorktree: true},
	}

	eng = NewWithDeps(Config{
		Owner:                "owner",
		Repo:                 "repo",
		ProjectNum:           1,
		User:                 "testuser",
		Token:                "token",
		MaxConcurrent:        1,
		Stages:               stagesWithDone,
		JanitorIntervalHours: 1,
	}, client, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = fabrikDir

	const dirName = "testowner-testrepo"
	wm = NewWorktreeManagerForRepo(repoDir, worktreesRoot, dirName)
	wm.logfFn = eng.logf
	eng.mu.Lock()
	eng.worktreeManagers["testowner/testrepo"] = wm
	eng.mu.Unlock()

	return eng, wm, worktreesRoot
}

// seedClosed puts a closed issue into the store.
func seedClosed(eng *Engine, repo string, number int) {
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo: repo, Number: number, Title: "test",
	}})
	eng.store.Apply(itemstate.IssueClosed{Repo: repo, Number: number})
}

// seedClosedOnStage puts a closed issue on a board stage.
func seedClosedOnStage(eng *Engine, repo string, number int, stageName string) {
	seedClosed(eng, repo, number)
	eng.store.Apply(itemstate.LocalStatusUpdated{Repo: repo, Number: number, NewStatus: stageName})
}

// seedOpenOnStage puts an open issue on a board stage.
func seedOpenOnStage(eng *Engine, repo string, number int, stageName string) {
	eng.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo: repo, Number: number, Title: "test",
	}})
	eng.store.Apply(itemstate.LocalStatusUpdated{Repo: repo, Number: number, NewStatus: stageName})
}

// createCleanWorktree creates a real git worktree for issueNumber and returns its path.
func createCleanWorktree(t *testing.T, wm *WorktreeManager, issueNumber int) string {
	t.Helper()
	wtDir, err := wm.EnsureWorktree(issueNumber, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree(%d): %v", issueNumber, err)
	}
	return wtDir
}

// TestJanitorSC1_ClosedOffBoard_Reaped verifies that a closed, off-board issue
// with a clean worktree is reaped on a janitor cycle.
func TestJanitorSC1_ClosedOffBoard_Reaped(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	const issueNumber = 1

	// Issue is not in the store (off-board); mock FetchIssue to return "closed".
	eng.client.(*mockGitHubClient).fetchIssueFn = func(owner, repo string, num int) (*gh.IssueData, error) {
		return &gh.IssueData{Number: num, State: "closed"}, nil
	}

	wtDir := createCleanWorktree(t, wm, issueNumber)
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("worktree should exist before janitor: %v", err)
	}

	eng.runWorktreeJanitor(context.Background())

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("SC-1: expected worktree for issue #%d to be reaped, but it still exists (err=%v)", issueNumber, err)
	}
}

// TestJanitorSC2_ClosedOnNonCleanupStage_Skipped verifies that a closed issue
// still on the board at a non-cleanup stage is NOT reaped.
func TestJanitorSC2_ClosedOnNonCleanupStage_Skipped(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	const issueNumber = 2

	seedClosedOnStage(eng, "testowner/testrepo", issueNumber, "Implement")

	wtDir := createCleanWorktree(t, wm, issueNumber)

	eng.runWorktreeJanitor(context.Background())

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("SC-2: worktree for #%d should NOT be reaped (closed but on non-cleanup stage Implement)", issueNumber)
	}
}

// TestJanitorSC3_OpenIssue_Skipped verifies that an open issue's worktree is
// never reaped, regardless of board state.
func TestJanitorSC3_OpenIssue_Skipped(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	const issueNumber = 3

	seedOpenOnStage(eng, "testowner/testrepo", issueNumber, "Research")

	wtDir := createCleanWorktree(t, wm, issueNumber)

	eng.runWorktreeJanitor(context.Background())

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("SC-3: worktree for open issue #%d should NOT be reaped", issueNumber)
	}
}

// TestJanitorSC4_DirtyWorktree_Skipped verifies that a dirty worktree is never
// reaped, even when all other reap conditions hold.
func TestJanitorSC4_DirtyWorktree_Skipped(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	const issueNumber = 4

	// Issue is off-board and closed via REST.
	eng.client.(*mockGitHubClient).fetchIssueFn = func(owner, repo string, num int) (*gh.IssueData, error) {
		return &gh.IssueData{Number: num, State: "closed"}, nil
	}

	wtDir := createCleanWorktree(t, wm, issueNumber)

	// Add an uncommitted file to make the worktree dirty.
	dirtyFile := filepath.Join(wtDir, "dirty.txt")
	if err := os.WriteFile(dirtyFile, []byte("dirty"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	eng.runWorktreeJanitor(context.Background())

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("SC-4: dirty worktree for #%d should NOT be reaped", issueNumber)
	}
}

// TestJanitorSC5_InFlightWorker_Skipped verifies that a worktree with an
// in-flight worker is never reaped.
func TestJanitorSC5_InFlightWorker_Skipped(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	const issueNumber = 5
	const repo = "testowner/testrepo"

	// Issue is closed and off-board in terms of status, but has an in-flight worker.
	seedClosed(eng, repo, issueNumber)
	eng.store.Apply(itemstate.WorkerEntered{
		Repo:      repo,
		Number:    issueNumber,
		StageName: "Implement",
		StartedAt: time.Now(),
	})

	wtDir := createCleanWorktree(t, wm, issueNumber)

	eng.runWorktreeJanitor(context.Background())

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Errorf("SC-5: worktree for issue #%d with in-flight worker should NOT be reaped", issueNumber)
	}
}

// TestJanitorSC6_Integration verifies that three stranded worktrees are reaped
// while one with an in-flight worker is left alone.
func TestJanitorSC6_Integration(t *testing.T) {
	eng, wm, _ := janitorTestSetup(t)
	const repo = "testowner/testrepo"

	// Issues 10, 11, 12 are stranded: closed, off-board, clean.
	// Issue 13 has an in-flight worker and should be skipped.
	eng.client.(*mockGitHubClient).fetchIssueFn = func(owner, r string, num int) (*gh.IssueData, error) {
		return &gh.IssueData{Number: num, State: "closed"}, nil
	}

	strandedIssues := []int{10, 11, 12}
	for _, n := range strandedIssues {
		createCleanWorktree(t, wm, n)
	}

	// Issue 13: closed in store, but has an in-flight worker.
	seedClosed(eng, repo, 13)
	eng.store.Apply(itemstate.WorkerEntered{
		Repo:      repo,
		Number:    13,
		StageName: "Done",
		StartedAt: time.Now(),
	})
	wtDir13 := createCleanWorktree(t, wm, 13)

	eng.runWorktreeJanitor(context.Background())

	// Three stranded worktrees should be gone.
	for _, n := range strandedIssues {
		wtDir := wm.WorktreeDir(n)
		if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
			t.Errorf("SC-6: expected worktree for issue #%d to be reaped, but it still exists", n)
		}
	}

	// In-flight worker's worktree should remain.
	if _, err := os.Stat(wtDir13); os.IsNotExist(err) {
		t.Errorf("SC-6: worktree for issue #13 (in-flight worker) should NOT be reaped")
	}
}
