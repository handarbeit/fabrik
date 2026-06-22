package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// TestJanitorStaleDeadWorkerReaped (SC-5) verifies that a worktree whose
// in-store worker handle has a stale heartbeat AND a confirmed-dead PID is
// reaped by the janitor rather than skipped. This is the fix for the
// multi-hour stall described in the incident report: a Claude process that died
// without the engine's context-cancel path firing left a permanent
// WorkerEntered block that the janitor honoured forever.
func TestJanitorStaleDeadWorkerReaped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stale-worker reaping requires signal-0 support; not available on Windows")
	}
	eng, wm, _ := janitorTestSetup(t)
	const issueNumber = 14
	const repo = "testowner/testrepo"

	// Start a real subprocess and kill it so we have a confirmed-dead PID.
	cmd := exec.Command("sleep", "1000")
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start subprocess: %v", err)
	}
	deadPID := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("could not kill subprocess: %v", err)
	}
	// Wait reaps the zombie so signal-0 returns ESRCH (not ECHILD).
	_ = cmd.Wait()

	// Use a 1ms stale timeout so the 10-minute-old LastSignAt is definitely stale.
	eng.cfg.WorkerStaleTimeout = 1 * time.Millisecond

	// Issue is closed and off-board.
	seedClosed(eng, repo, issueNumber)

	// Inject a worker handle with the dead PID and a stale heartbeat.
	staleTime := time.Now().Add(-10 * time.Minute)
	eng.store.Apply(itemstate.LocalLockAcquired{
		Repo:       repo,
		Number:     issueNumber,
		User:       "testuser",
		AcquiredAt: staleTime,
		Worker: &itemstate.WorkerHandle{
			PID:        deadPID,
			StageName:  "Validate",
			StartedAt:  staleTime,
			LastSignAt: staleTime,
		},
	})

	wtDir := createCleanWorktree(t, wm, issueNumber)
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("worktree should exist before janitor: %v", err)
	}

	eng.runWorktreeJanitor(context.Background())

	// The stale+dead worker must NOT block reaping.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("SC-5: expected worktree for issue #%d (stale+dead worker) to be reaped, but it still exists", issueNumber)
	}
}

// ── Log janitor tests ────────────────────────────────────────────────────────

// seedLogFile writes a file of given size to dir/name and sets its mtime.
// Returns the full path.
func seedLogFile(t *testing.T, dir, name string, size int64, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("seedLogFile mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0644); err != nil {
		t.Fatalf("seedLogFile WriteFile %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("seedLogFile Chtimes %s: %v", path, err)
	}
	return path
}

// TestLogJanitorAgePrune verifies that files older than retentionDays are deleted
// and newer files are kept.
func TestLogJanitorAgePrune(t *testing.T) {
	logsRoot := t.TempDir()
	now := time.Now()
	old := now.Add(-15 * 24 * time.Hour)   // 15 days ago — older than 14-day default
	recent := now.Add(-1 * 24 * time.Hour) // 1 day ago — within retention window

	issueDir := filepath.Join(logsRoot, "owner-repo", "issue-1")
	oldFile := seedLogFile(t, issueDir, "old.log", 100, old)
	newFile := seedLogFile(t, issueDir, "new.log", 100, recent)

	scanned, removed, _, ageRemoved, sizeRemoved, err := pruneLogs(logsRoot, 14, 0, now)
	if err != nil {
		t.Fatalf("pruneLogs: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned=%d, want 2", scanned)
	}
	if removed != 1 || ageRemoved != 1 || sizeRemoved != 0 {
		t.Errorf("removed=%d ageRemoved=%d sizeRemoved=%d, want 1 1 0", removed, ageRemoved, sizeRemoved)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old file should be deleted, but still exists")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("new file should be kept, but got: %v", err)
	}
}

// TestLogJanitorSizeCap verifies that after age pruning, if total size exceeds
// maxBytes, the oldest files are deleted first until under the cap.
func TestLogJanitorSizeCap(t *testing.T) {
	logsRoot := t.TempDir()
	now := time.Now()
	// All files are recent (within 30-day retention), but total exceeds 300 bytes cap.
	issueDir := filepath.Join(logsRoot, "owner-repo", "issue-2")

	oldest := now.Add(-3 * time.Hour)
	middle := now.Add(-2 * time.Hour)
	newest := now.Add(-1 * time.Hour)

	oldestFile := seedLogFile(t, issueDir, "oldest.log", 150, oldest)
	middleFile := seedLogFile(t, issueDir, "middle.log", 100, middle)
	newestFile := seedLogFile(t, issueDir, "newest.log", 100, newest)

	// retentionDays=30 (no age prune triggers), maxBytes=200 (oldest must be deleted)
	_, removed, bytesRemoved, ageRemoved, sizeRemoved, err := pruneLogs(logsRoot, 30, 200, now)
	if err != nil {
		t.Fatalf("pruneLogs: %v", err)
	}
	// Total = 350 bytes; cap = 200; oldest (150) deleted → 200 == cap → stop.
	if removed != 1 || ageRemoved != 0 || sizeRemoved != 1 {
		t.Errorf("removed=%d ageRemoved=%d sizeRemoved=%d, want 1 0 1", removed, ageRemoved, sizeRemoved)
	}
	if bytesRemoved != 150 {
		t.Errorf("bytesRemoved=%d, want 150", bytesRemoved)
	}
	if _, err := os.Stat(oldestFile); !os.IsNotExist(err) {
		t.Errorf("oldest file should be deleted")
	}
	if _, err := os.Stat(middleFile); err != nil {
		t.Errorf("middle file should be kept: %v", err)
	}
	if _, err := os.Stat(newestFile); err != nil {
		t.Errorf("newest file should be kept: %v", err)
	}
}

// TestLogJanitorScopeGuard verifies that pruneLogs never touches sibling
// directories (sessions/, worktrees/, repos/, debug/) even when they contain
// old files that would otherwise qualify for deletion.
func TestLogJanitorScopeGuard(t *testing.T) {
	fabrikBase := t.TempDir()
	logsRoot := filepath.Join(fabrikBase, "logs")
	if err := os.MkdirAll(logsRoot, 0755); err != nil {
		t.Fatalf("mkdir logsRoot: %v", err)
	}

	now := time.Now()
	ancient := now.Add(-365 * 24 * time.Hour) // 1 year old — would be pruned if in logs/

	// Seed sibling directories with old files.
	siblings := []string{"sessions", "worktrees", "repos", "debug"}
	var siblingFiles []string
	for _, sib := range siblings {
		sibDir := filepath.Join(fabrikBase, sib, "issue-1")
		f := seedLogFile(t, sibDir, "old.log", 1000, ancient)
		siblingFiles = append(siblingFiles, f)
	}

	// Seed one old file inside logs/ so prune actually runs.
	logsIssueDir := filepath.Join(logsRoot, "owner-repo", "issue-1")
	logsFile := seedLogFile(t, logsIssueDir, "old.log", 100, ancient)

	_, removed, _, _, _, err := pruneLogs(logsRoot, 1, 0, now) // 1-day retention
	if err != nil {
		t.Fatalf("pruneLogs: %v", err)
	}
	// Only the file in logs/ should be removed.
	if removed != 1 {
		t.Errorf("removed=%d, want exactly 1 (only the logs/ file)", removed)
	}
	if _, err := os.Stat(logsFile); !os.IsNotExist(err) {
		t.Errorf("logs/ file should be deleted")
	}
	for _, sf := range siblingFiles {
		if _, err := os.Stat(sf); err != nil {
			t.Errorf("sibling file %s should be untouched, but got: %v", sf, err)
		}
	}
}

// TestLogJanitorEmptyDirCleanup verifies that empty issue-N/ and owner-repo/
// directories are removed after pruning, but non-empty directories are kept.
func TestLogJanitorEmptyDirCleanup(t *testing.T) {
	logsRoot := t.TempDir()
	now := time.Now()
	ancient := now.Add(-30 * 24 * time.Hour)

	// emptyIssue: all files old → dir becomes empty and should be removed.
	emptyIssueDir := filepath.Join(logsRoot, "owner-repo", "issue-1")
	seedLogFile(t, emptyIssueDir, "old.log", 10, ancient)

	// nonEmptyIssue: one old, one new → dir has remaining file and must be kept.
	nonEmptyIssueDir := filepath.Join(logsRoot, "owner-repo", "issue-2")
	seedLogFile(t, nonEmptyIssueDir, "old.log", 10, ancient)
	seedLogFile(t, nonEmptyIssueDir, "new.log", 10, now)

	_, _, _, _, _, err := pruneLogs(logsRoot, 14, 0, now)
	if err != nil {
		t.Fatalf("pruneLogs: %v", err)
	}

	// issue-1/ is now empty → should be removed.
	if _, err := os.Stat(emptyIssueDir); !os.IsNotExist(err) {
		t.Errorf("emptied issue-1/ dir should be removed, but still exists")
	}
	// issue-2/ still has new.log → must be kept.
	if _, err := os.Stat(nonEmptyIssueDir); err != nil {
		t.Errorf("non-empty issue-2/ dir should be kept: %v", err)
	}
}

// TestLogJanitorDisabledPaths verifies that retentionDays=0 skips age pruning
// and maxBytes=0 skips size-cap pruning.
func TestLogJanitorDisabledPaths(t *testing.T) {
	t.Run("retentionDays=0 skips age prune", func(t *testing.T) {
		logsRoot := t.TempDir()
		now := time.Now()
		ancient := now.Add(-365 * 24 * time.Hour)
		issueDir := filepath.Join(logsRoot, "issue-1")
		f := seedLogFile(t, issueDir, "old.log", 100, ancient)

		_, removed, _, ageRemoved, _, err := pruneLogs(logsRoot, 0, 0, now)
		if err != nil {
			t.Fatalf("pruneLogs: %v", err)
		}
		if removed != 0 || ageRemoved != 0 {
			t.Errorf("retentionDays=0: removed=%d ageRemoved=%d, want 0 0", removed, ageRemoved)
		}
		if _, err := os.Stat(f); err != nil {
			t.Errorf("file should not be deleted when retentionDays=0: %v", err)
		}
	})

	t.Run("maxBytes=0 skips size cap", func(t *testing.T) {
		logsRoot := t.TempDir()
		now := time.Now()
		issueDir := filepath.Join(logsRoot, "issue-1")
		// All files are recent so age prune won't touch them.
		f1 := seedLogFile(t, issueDir, "a.log", 500*1024*1024, now.Add(-1*time.Hour))
		f2 := seedLogFile(t, issueDir, "b.log", 500*1024*1024, now.Add(-30*time.Minute))

		_, removed, _, _, sizeRemoved, err := pruneLogs(logsRoot, 0, 0, now)
		if err != nil {
			t.Fatalf("pruneLogs: %v", err)
		}
		if removed != 0 || sizeRemoved != 0 {
			t.Errorf("maxBytes=0: removed=%d sizeRemoved=%d, want 0 0", removed, sizeRemoved)
		}
		for _, f := range []string{f1, f2} {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("file %s should not be deleted when maxBytes=0: %v", f, err)
			}
		}
	})
}

// TestLogJanitorNonExistentRoot verifies that pruneLogs returns clean zero
// counts (not an error) when the logs directory doesn't exist yet.
func TestLogJanitorNonExistentRoot(t *testing.T) {
	nonExistentRoot := filepath.Join(t.TempDir(), "no-such-dir")
	scanned, removed, bytesRemoved, ageRemoved, sizeRemoved, err := pruneLogs(nonExistentRoot, 14, 2147483648, time.Now())
	if err != nil {
		t.Errorf("expected nil error for non-existent root, got: %v", err)
	}
	if scanned != 0 || removed != 0 || bytesRemoved != 0 || ageRemoved != 0 || sizeRemoved != 0 {
		t.Errorf("expected zero counts for non-existent root, got scanned=%d removed=%d", scanned, removed)
	}
}

// TestLogJanitorPeriodicPath proves that runLogJanitor (the function called by
// the periodic goroutine on every tick) correctly prunes old files without
// requiring a process restart. This directly validates the core guarantee for
// long-running instances: disk stays bounded even when Fabrik runs for weeks.
func TestLogJanitorPeriodicPath(t *testing.T) {
	fabrikDir := t.TempDir()
	logsRoot := filepath.Join(fabrikDir, ".fabrik", "logs")

	now := time.Now()
	ancient := now.Add(-20 * 24 * time.Hour) // 20 days old — exceeds 14-day default
	recent := now.Add(-1 * time.Hour)

	issueDir := filepath.Join(logsRoot, "owner-repo", "issue-99")
	oldFile := seedLogFile(t, issueDir, "old.log", 100, ancient)
	newFile := seedLogFile(t, issueDir, "new.log", 100, recent)

	eng := NewWithDeps(Config{
		MaxConcurrent:        1,
		JanitorIntervalHours: 1,
		LogRetentionDays:     14,
		LogMaxBytes:          2147483648,
	}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	eng.fabrikDir = fabrikDir

	// Call runLogJanitor directly — this is the same function the periodic
	// goroutine calls on every tick, proving the periodic path works correctly.
	eng.runLogJanitor(context.Background())

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("periodic path: old file should be deleted on tick, but still exists")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("periodic path: recent file should be kept: %v", err)
	}
}
