// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// TestEmitStructural_WithChannel sends a structural event and verifies it's received.
func TestEmitStructural_WithChannel(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	ch := make(chan tui.Event, 4)
	eng.events = ch

	eng.emitStructural(tui.PollStartedEvent{Owner: "owner", Repo: "repo", Project: 1})

	select {
	case ev := <-ch:
		if _, ok := ev.(tui.PollStartedEvent); !ok {
			t.Errorf("expected PollStartedEvent, got %T", ev)
		}
	default:
		t.Error("expected event in channel")
	}
}

// TestItemMayNeedWork_StaleButCooldownExpired verifies that a stale item is
// retried after the cooldown period expires.
func TestItemMayNeedWork_StaleButCooldownExpired(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // short cooldown

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    42,
		Status:    "Research",
		ItemID:    "PVTI_42",
		UpdatedAt: ts,
	}

	// Record the last-seen timestamp so the "unchanged" path triggers
	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#42"] = ts
	// Record a processedSet entry from >cooldown ago
	eng.processedSet["owner/repo#42-Research"] = time.Now().Add(-2 * time.Minute)
	eng.mu.Unlock()

	if !eng.itemMayNeedWork(item) {
		t.Error("stale item with expired cooldown should need work")
	}
}

// TestItemMayNeedWork_StaleWithinCooldown verifies that a stale item within
// cooldown is skipped.
func TestItemMayNeedWork_StaleWithinCooldown(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60 // long cooldown

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    43,
		Status:    "Research",
		ItemID:    "PVTI_43",
		UpdatedAt: ts,
	}

	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#43"] = ts
	eng.processedSet["owner/repo#43-Research"] = time.Now() // just processed
	eng.mu.Unlock()

	if eng.itemMayNeedWork(item) {
		t.Error("stale item within cooldown should not need work")
	}
}

// TestItemMayNeedWork_LockedByOtherUser verifies that itemMayNeedWork no longer
// filters locked items — that check moved to itemNeedsWork after deep fetch.
func TestItemMayNeedWork_LockedByOtherUser(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 44,
		Status: "Research",
		Labels: []string{"fabrik:locked:otheruser"},
	}
	// itemMayNeedWork no longer checks labels — locked check is in itemNeedsWork.
	if !eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should not filter locked items (lock check is in itemNeedsWork)")
	}
}

// TestItemNeedsWork_LockedByOtherUser verifies that items locked by another user
// are filtered out in itemNeedsWork (which runs after deep fetch).
func TestItemNeedsWork_LockedByOtherUser(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 44,
		Status: "Research",
		Labels: []string{"fabrik:locked:otheruser"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("item locked by other user should not need work (itemNeedsWork)")
	}
}

// TestBlockOnInput_Success covers both AddLabel calls.
func TestBlockOnInput_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 5}
	eng.blockOnInput(item, stage)

	// Both fabrik:paused and fabrik:awaiting-input should have been added
	if len(client.addLabelCalls) < 2 {
		t.Errorf("expected 2 AddLabel calls, got %d", len(client.addLabelCalls))
	}
}

// TestBlockOnInput_LabelErrors_LogsWarning covers the warning log branches.
func TestBlockOnInput_LabelErrors_LogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("label error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 6}
	// Should not panic when labels fail
	eng.blockOnInput(item, stage)
}

// TestCommitWIP_ExcludesContextFiles verifies that commitWIP does not include
// files under .fabrik-context/ in the WIP commit, even when they were previously
// committed (making them tracked by git).
func TestCommitWIP_ExcludesContextFiles(t *testing.T) {
	skipIfNoGit(t)

	// Set up a minimal git repo.
	workDir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}

	// Create a regular file and a context file — both with changes.
	regularFile := filepath.Join(workDir, "app.go")
	if err := os.WriteFile(regularFile, []byte("package main\n"), 0644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	contextDir := filepath.Join(workDir, ".fabrik-context")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		t.Fatalf("mkdir context dir: %v", err)
	}
	contextFile := filepath.Join(contextDir, "issue.md")
	if err := os.WriteFile(contextFile, []byte("# Issue\n"), 0644); err != nil {
		t.Fatalf("write context file: %v", err)
	}

	// Commit both files so the context file is tracked.
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "seed both files"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = workDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed commit %v: %s: %v", args, out, err)
		}
	}

	// Now modify both files so they appear as uncommitted changes.
	if err := os.WriteFile(regularFile, []byte("package main\n\n// changed\n"), 0644); err != nil {
		t.Fatalf("modify regular file: %v", err)
	}
	if err := os.WriteFile(contextFile, []byte("# Issue\n\nupdated context\n"), 0644); err != nil {
		t.Fatalf("modify context file: %v", err)
	}

	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.commitWIP(workDir, 42, "Research")

	// Verify the WIP commit was created.
	logCmd := exec.Command("git", "log", "--oneline", "-1")
	logCmd.Dir = workDir
	logOut, err := logCmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(logOut), "WIP") {
		t.Errorf("expected WIP commit, got: %s", string(logOut))
	}

	// Verify the context file is NOT in the WIP commit.
	showCmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	showCmd.Dir = workDir
	showOut, err := showCmd.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	filesInCommit := string(showOut)
	if strings.Contains(filesInCommit, ".fabrik-context") {
		t.Errorf(".fabrik-context files should not appear in WIP commit, got:\n%s", filesInCommit)
	}
	if !strings.Contains(filesInCommit, "app.go") {
		t.Errorf("app.go should appear in WIP commit, got:\n%s", filesInCommit)
	}

	// Verify the context file change is preserved on disk (not lost).
	data, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("read context file after commitWIP: %v", err)
	}
	if !strings.Contains(string(data), "updated context") {
		t.Errorf("context file content should be preserved on disk")
	}
}

// TestItemMayNeedWork_DependencyGate_OpenBlocker_PastFirstStage verifies that
// itemMayNeedWork no longer filters items with open blockers — that check moved
// to itemNeedsWork after deep fetch.
func TestItemMayNeedWork_DependencyGate_OpenBlocker_PastFirstStage(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// testEngine uses testStages(): Research(1), Plan(2), Implement(3)
	// "Research" is the first stage; "Plan" is past the first.
	item := gh.ProjectItem{
		Number: 5,
		Status: "Plan",
		BlockedBy: []gh.Dependency{
			{Number: 4, State: "OPEN", Repo: "owner/repo"},
		},
	}

	// itemMayNeedWork no longer checks blockedBy — dep gate is in itemNeedsWork.
	if !eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should not filter items with open blockers (dep gate is in itemNeedsWork)")
	}
}

// TestItemNeedsWork_DependencyGate_OpenBlocker_PastFirstStage verifies that
// an item past the first stage with an open blocker is filtered in itemNeedsWork.
func TestItemNeedsWork_DependencyGate_OpenBlocker_PastFirstStage(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 5,
		Status: "Plan",
		BlockedBy: []gh.Dependency{
			{Number: 4, State: "OPEN", Repo: "owner/repo"},
		},
	}

	if eng.itemNeedsWork(item) {
		t.Error("expected itemNeedsWork=false for past-first-stage item with open blocker")
	}
}

// TestItemMayNeedWork_DependencyGate_FirstStage_NotFiltered verifies that
// an item in the first stage is NOT filtered even with an open blocker.
// (Dependency gate check is in itemNeedsWork and bypasses first-stage items.)
func TestItemMayNeedWork_DependencyGate_FirstStage_NotFiltered(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// "Research" is the first stage in testStages()
	item := gh.ProjectItem{
		Number: 5,
		Status: "Research",
		BlockedBy: []gh.Dependency{
			{Number: 4, State: "OPEN", Repo: "owner/repo"},
		},
	}

	if !eng.itemMayNeedWork(item) {
		t.Error("expected itemMayNeedWork=true for first-stage item regardless of blockers")
	}
}

// TestItemMayNeedWork_DependencyGate_AllClosed_NotFiltered verifies that
// an item past the first stage with all blockers closed is not filtered.
// (Dependency gate check is in itemNeedsWork; all-closed items pass through.)
func TestItemMayNeedWork_DependencyGate_AllClosed_NotFiltered(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 5,
		Status: "Plan",
		BlockedBy: []gh.Dependency{
			{Number: 4, State: "CLOSED", Repo: "owner/repo"},
		},
	}

	if !eng.itemMayNeedWork(item) {
		t.Error("expected itemMayNeedWork=true for past-first-stage item with all blockers closed")
	}
}

// TestItemMayNeedWork_ClosedIssue verifies that a closed issue in a non-cleanup
// stage is skipped, regardless of yolo or labels.
func TestItemMayNeedWork_ClosedIssue(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number:   99,
		Status:   "Research",
		IsClosed: true,
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed issue should not need work")
	}
}

// TestItemNeedsWork_ClosedIssue verifies that itemNeedsWork returns false for
// a closed issue in a non-cleanup stage.
func TestItemNeedsWork_ClosedIssue(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number:   99,
		Status:   "Research",
		IsClosed: true,
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed issue should not need work (itemNeedsWork)")
	}
}

// TestItemMayNeedWork_ClosedIssue_CleanupStage verifies that a closed issue in
// a cleanup stage still passes itemMayNeedWork when the worktree directory exists.
func TestItemMayNeedWork_ClosedIssue_CleanupStage(t *testing.T) {
	rootDir := t.TempDir()
	wm := NewWorktreeManager(rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)
	const issueNum = 99
	if err := os.MkdirAll(wm.WorktreeDir(issueNum), 0755); err != nil {
		t.Fatal(err)
	}
	item := gh.ProjectItem{
		Number:   issueNum,
		Status:   "Done",
		IsClosed: true,
	}
	if !eng.itemMayNeedWork(item) {
		t.Error("closed issue in cleanup stage with worktree should need work")
	}
}

// TestItemMayNeedWork_ClosedIssue_CleanupStage_NoWorktree verifies that a closed
// issue in a cleanup stage is skipped when no worktree directory exists.
func TestItemMayNeedWork_ClosedIssue_CleanupStage_NoWorktree(t *testing.T) {
	rootDir := t.TempDir()
	wm := NewWorktreeManager(rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)
	item := gh.ProjectItem{
		Number:   99,
		Status:   "Done",
		IsClosed: true,
	}
	if eng.itemMayNeedWork(item) {
		t.Error("closed issue in cleanup stage without worktree should not need work")
	}
}

// TestItemNeedsWork_ClosedIssue_CleanupStage verifies that a closed issue in a
// cleanup stage passes itemNeedsWork when no complete label is set and worktree exists.
func TestItemNeedsWork_ClosedIssue_CleanupStage(t *testing.T) {
	rootDir := t.TempDir()
	wm := NewWorktreeManager(rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)
	const issueNum = 99
	if err := os.MkdirAll(wm.WorktreeDir(issueNum), 0755); err != nil {
		t.Fatal(err)
	}
	item := gh.ProjectItem{
		Number:   issueNum,
		Status:   "Done",
		IsClosed: true,
	}
	if !eng.itemNeedsWork(item) {
		t.Error("closed issue in cleanup stage without complete label should need work")
	}
}

// TestItemNeedsWork_ClosedIssue_CleanupStage_Complete verifies that a closed issue
// in a cleanup stage is skipped when the stage:Done:complete label is present.
func TestItemNeedsWork_ClosedIssue_CleanupStage_Complete(t *testing.T) {
	rootDir := t.TempDir()
	wm := NewWorktreeManager(rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)
	item := gh.ProjectItem{
		Number:   99,
		Status:   "Done",
		IsClosed: true,
		Labels:   []string{"stage:Done:complete"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("closed issue in cleanup stage with complete label should not need work")
	}
}

// TestPoll_DeepFetchFailureExcludesFromLastUpdatedAt verifies that when
// FetchItemDetails fails for an item, lastUpdatedAt is NOT updated for that
// item, so the next poll retries the deep-fetch.
func TestPoll_DeepFetchFailureExcludesFromLastUpdatedAt(t *testing.T) {
	now := time.Now()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 10, Title: "Broken", Status: "Research", UpdatedAt: now},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return errors.New("simulated rate limit error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 999 // very long cooldown so we don't accidentally bypass

	ctx := t.Context()
	if err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// lastUpdatedAt must NOT be set for item #10 — failed deep-fetch must not cache.
	eng.mu.Lock()
	_, ok := eng.lastUpdatedAt["owner/repo#10"]
	eng.mu.Unlock()
	if ok {
		t.Error("lastUpdatedAt should NOT be updated when FetchItemDetails fails")
	}

	// deepFetchFailureTime must be recorded.
	eng.mu.Lock()
	_, recorded := eng.deepFetchFailureTime["owner/repo#10"]
	eng.mu.Unlock()
	if !recorded {
		t.Error("deepFetchFailureTime should be recorded when FetchItemDetails fails")
	}
}

// TestPoll_DeepFetchSuccessClearsFailureTime verifies that a successful
// FetchItemDetails clears a previously recorded deepFetchFailureTime.
func TestPoll_DeepFetchSuccessClearsFailureTime(t *testing.T) {
	now := time.Now()
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 11, Title: "Fixed", Status: "Research", UpdatedAt: now},
				},
			}, nil
		},
		// fetchItemDetailsFn nil = success (mock returns nil by default)
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Pre-seed a failure time.
	eng.mu.Lock()
	eng.deepFetchFailureTime["owner/repo#11"] = now.Add(-time.Minute)
	eng.mu.Unlock()

	ctx := t.Context()
	if err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	eng.mu.Lock()
	_, stillRecorded := eng.deepFetchFailureTime["owner/repo#11"]
	eng.mu.Unlock()
	if stillRecorded {
		t.Error("deepFetchFailureTime should be cleared after a successful FetchItemDetails")
	}
}

// TestItemMayNeedWork_AwaitingInputBypassesUnchanged verifies that an item with
// fabrik:awaiting-input and an unchanged updatedAt still returns true from
// itemMayNeedWork (so a new comment can be detected on the next poll).
func TestItemMayNeedWork_AwaitingInputBypassesUnchanged(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    50,
		Status:    "Research",
		ItemID:    "PVTI_50",
		UpdatedAt: ts,
		Labels:    []string{"fabrik:awaiting-input", "fabrik:paused"},
	}

	// Record the last-seen timestamp so the "unchanged" path triggers.
	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#50"] = ts
	eng.mu.Unlock()

	if !eng.itemMayNeedWork(item) {
		t.Error("awaiting-input item with unchanged updatedAt should still return true from itemMayNeedWork")
	}
}

// TestItemMayNeedWork_DeepFetchFailureCooldown verifies that after a failure is
// recorded in deepFetchFailureTime, itemMayNeedWork returns false within the
// cooldown window and true after the cooldown has expired.
func TestItemMayNeedWork_DeepFetchFailureCooldown(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // 10-second cooldown

	item := gh.ProjectItem{
		Number: 51,
		Status: "Research",
		ItemID: "PVTI_51",
		// UpdatedAt zero — no lastUpdatedAt entry, so "unchanged" branch won't fire
	}

	// Record a very recent failure.
	eng.mu.Lock()
	eng.deepFetchFailureTime["owner/repo#51"] = time.Now()
	eng.mu.Unlock()

	if eng.itemMayNeedWork(item) {
		t.Error("item with recent deep-fetch failure should be skipped (within cooldown)")
	}

	// Simulate cooldown expiry by backdating the failure time.
	eng.mu.Lock()
	eng.deepFetchFailureTime["owner/repo#51"] = time.Now().Add(-20 * time.Second)
	eng.mu.Unlock()

	if !eng.itemMayNeedWork(item) {
		t.Error("item with expired deep-fetch failure cooldown should be retried")
	}
}

// TestProcessItem_EvictsLastUpdatedAtAfterStageRun verifies that processItem
// deletes lastUpdatedAt[iKey] after a stage runs (claudeRan=true). This ensures
// the next poll re-evaluates the item, catching any comments that arrived during
// the in-flight run.
func TestProcessItem_EvictsLastUpdatedAtAfterStageRun(t *testing.T) {
	skipIfNoGit(t)

	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, TokenUsage, error) {
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			PollSeconds:   60,
			Stages:        testStages(),
		},
		&mockGitHubClient{},
		claude,
		wm,
	)

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    60,
		Title:     "Eviction test",
		Status:    "Research",
		ItemID:    "PVTI_60",
		UpdatedAt: ts,
		Repo:      "owner/repo",
	}

	// Pre-populate lastUpdatedAt as if a concurrent poll cached this item.
	eng.mu.Lock()
	eng.lastUpdatedAt["owner/repo#60"] = ts
	eng.mu.Unlock()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	_ = eng.processItem(context.Background(), board, item)

	// After processItem (with claudeRan), lastUpdatedAt must be evicted.
	eng.mu.Lock()
	_, stillCached := eng.lastUpdatedAt["owner/repo#60"]
	eng.mu.Unlock()

	if stillCached {
		t.Error("lastUpdatedAt should be evicted after a stage runs so next poll re-evaluates the item")
	}
}
