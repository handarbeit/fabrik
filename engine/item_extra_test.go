package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
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

	// Set an expired CooldownAt entry (>cooldown ago) so re-eval fires
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 42, Reason: "periodic-re-eval", Until: time.Now().Add(-2 * time.Minute)})

	if !eng.itemMayNeedWork(item) {
		t.Error("stale item with expired cooldown should need work")
	}
}

// TestPollPreFilter_StaleItemWithinCooldown_Skipped verifies that a stale item
// within cooldown is not deep-fetched by the poll pre-filter.
func TestPollPreFilter_StaleItemWithinCooldown_Skipped(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 43, Status: "Research", ItemID: "PVTI_43", UpdatedAt: ts}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 43, Reason: "periodic-re-eval", Until: time.Now().Add(10 * time.Minute)})
	// item not in cycleSet (eng.mayNeedWork is empty), active CooldownAt → must be skipped
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("stale item within active cooldown should not be deep-fetched")
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

// TestItemNeedsWork_DependencyGate_PassesThrough verifies that itemNeedsWork
// now passes items with open blockers through to processItem, which calls
// checkDependencies to apply the fabrik:blocked label. The previous silent
// skip here caused items to get stuck: without fabrik:blocked, the updatedAt
// cache-bypass logic in itemMayNeedWork never re-evaluated them after
// blockers closed (since blocker closure doesn't bump the item's updatedAt).
func TestItemNeedsWork_DependencyGate_PassesThrough(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 5,
		Status: "Plan",
		BlockedBy: []gh.Dependency{
			{Number: 4, State: "OPEN", Repo: "owner/repo"},
		},
	}

	// Dep gating moved entirely to processItem via checkDependencies.
	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should pass blocked items through so processItem can apply fabrik:blocked")
	}
}

// TestItemMayNeedWork_DependencyGate_FirstStage_NotFiltered verifies that
// itemMayNeedWork does not filter first-stage items with open blockers.
// The dep gate runs in processItem (via checkDependencies), not in itemMayNeedWork.
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
// FetchItemDetails fails for an item, the failure is recorded in the store so
// the next poll retries the deep-fetch after the cooldown expires.
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
	// Seed item into cycleSet so the pre-filter admits it for deep-fetch.
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#10"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := t.Context()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// LastDeepFetchFailureAt must be recorded in the store.
	snap, _ := eng.store.Get("owner/repo", 10)
	if snap.State().LastDeepFetchFailureAt.IsZero() {
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
	// Pre-seed a failure time via the store.
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: "owner/repo", Number: 11, At: now.Add(-time.Minute)})
	// Seed item into cycleSet so the pre-filter admits it for deep-fetch.
	eng.mayNeedWorkMu.Lock()
	eng.mayNeedWork["owner/repo#11"] = true
	eng.mayNeedWorkMu.Unlock()

	ctx := t.Context()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	snapAfter, _ := eng.store.Get("owner/repo", 11)
	if !snapAfter.State().LastDeepFetchFailureAt.IsZero() {
		t.Error("deepFetchFailureTime should be cleared after a successful FetchItemDetails")
	}
}

// TestPollPreFilter_AwaitingInput_WithoutChange_Skipped verifies that an item with
// fabrik:awaiting-input is not deep-fetched when it has not changed (not in cycleSet)
// and has no expired CooldownAt. Adding a comment bumps updatedAt (fires observer),
// so the normal cycleSet mechanism catches it.
func TestPollPreFilter_AwaitingInput_WithoutChange_Skipped(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 50, Status: "Research", ItemID: "PVTI_50", UpdatedAt: ts, Labels: []string{"fabrik:awaiting-input", "fabrik:paused"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Seed the store so the pre-filter sees this as a known (previously-processed) item.
	// InvocationRecorded fires InvocationChanged (not wakeChFlags) so no observer side-effect.
	eng.store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 50, Completed: true})
	// Not in cycleSet, no bypass label (awaiting-input is NOT a bypass), no expired CooldownAt.
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("awaiting-input item without observer-triggered change should not be deep-fetched")
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
		// UpdatedAt zero — not in cycleSet, so pre-filter would skip unless failure cooldown fires
	}

	// Record a very recent failure via the store.
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: "owner/repo", Number: 51, At: time.Now()})

	if eng.itemMayNeedWork(item) {
		t.Error("item with recent deep-fetch failure should be skipped (within cooldown)")
	}

	// Simulate cooldown expiry by backdating the failure time.
	eng.store.Apply(itemstate.DeepFetchFailed{Repo: "owner/repo", Number: 51, At: time.Now().Add(-20 * time.Second)})

	if !eng.itemMayNeedWork(item) {
		t.Error("item with expired deep-fetch failure cooldown should be retried")
	}
}


// TestItemMayNeedWork_AwaitingCI_BypassesUpdatedAtCache verifies that items with
// fabrik:awaiting-ci bypass the updatedAt cache so the catch-up loop can
// re-evaluate CI status on every poll even when the issue hasn't changed.
func TestItemMayNeedWork_AwaitingCI_BypassesUpdatedAtCache(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60 // long cooldown that would normally suppress

	ts := time.Now().Add(-time.Minute)
	item := gh.ProjectItem{
		Number:    61,
		Status:    "Research",
		ItemID:    "PVTI_61",
		UpdatedAt: ts,
		Labels:    []string{"fabrik:awaiting-ci"},
	}

	// No CooldownAt entry — awaiting-ci items use per-poll bypass (not cooldown path).
	if !eng.itemMayNeedWork(item) {
		t.Error("item with fabrik:awaiting-ci should bypass the updatedAt cache and return true")
	}
}

// TestPollPreFilter_AwaitingReview_WithinCooldown_Skipped verifies that items with
// fabrik:awaiting-review use the CooldownAt["review-blocked"] path and are skipped
// when the cooldown is still active (not in cycleSet, no bypass).
//
// Real-world repro: issue #467 — fabrik filed this regression after observing an
// issue stuck for hours waiting on Copilot when Phase 1 should have re-fired.
func TestPollPreFilter_AwaitingReview_WithinCooldown_Skipped(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 67, Status: "Research", ItemID: "PVTI_67", UpdatedAt: ts, Labels: []string{"fabrik:awaiting-review"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 67, Reason: "review-blocked", Until: time.Now().Add(10 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("fabrik:awaiting-review item within cooldown should not be deep-fetched")
	}
}

// TestPollPreFilter_AwaitingReview_ExpiredCooldown_Admitted verifies that items with
// fabrik:awaiting-review and an expired CooldownAt["review-blocked"] are re-admitted.
func TestPollPreFilter_AwaitingReview_ExpiredCooldown_Admitted(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 67, Status: "Research", ItemID: "PVTI_67", UpdatedAt: ts, Labels: []string{"fabrik:awaiting-review"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 67, Reason: "review-blocked", Until: time.Now().Add(-15 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !deepFetched {
		t.Error("fabrik:awaiting-review item past cooldown should be deep-fetched")
	}
}

// TestPollPreFilter_AwaitingReview_NoCooldown_NotBypassed verifies that
// fabrik:awaiting-review is NOT in the unconditional bypass list (unlike
// fabrik:awaiting-ci and fabrik:rebase-needed). Without a CooldownAt entry and
// without being in cycleSet, the item is filtered by the pre-filter.
func TestPollPreFilter_AwaitingReview_NoCooldown_NotBypassed(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 68, Status: "Research", ItemID: "PVTI_68", UpdatedAt: ts, Labels: []string{"fabrik:awaiting-review"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60
	// Seed the store so the pre-filter sees this as a known (previously-processed) item.
	// Item is in store but has no CooldownAt → not bypassed by the active-cooldown path.
	eng.store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 68, Completed: true})
	// Not in cycleSet, no awaiting-ci/rebase-needed, no CooldownAt → should be skipped.
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("fabrik:awaiting-review without CooldownAt entry should be filtered (no per-poll bypass)")
	}
}

// TestPollPreFilter_Blocked_WithinCooldown_Skipped verifies that a fabrik:blocked
// item with an active CooldownAt is not deep-fetched. The dependency item's own
// updatedAt changes when it closes (fires observer), so no special bypass is needed.
func TestPollPreFilter_Blocked_WithinCooldown_Skipped(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 63, Status: "Research", ItemID: "PVTI_63", UpdatedAt: ts, Labels: []string{"fabrik:blocked"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 63, Reason: "dep-blocked", Until: time.Now().Add(10 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("fabrik:blocked item with active CooldownAt should not be deep-fetched")
	}
}

// TestPollPreFilter_NoSpecialLabel_WithinCooldown_Skipped verifies that without
// fabrik:awaiting-ci, a stale item within cooldown is filtered by the pre-filter.
func TestPollPreFilter_NoSpecialLabel_WithinCooldown_Skipped(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 62, Status: "Research", ItemID: "PVTI_62", UpdatedAt: ts, Labels: []string{"stage:Validate:complete"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 60
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 62, Reason: "periodic-re-eval", Until: time.Now().Add(10 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if deepFetched {
		t.Error("item without bypass labels should be filtered by active CooldownAt")
	}
}

// TestPollPreFilter_CompleteStage_ExpiredCooldown_DeepFetched verifies that an item
// with a stage:X:complete label IS deep-fetched when the cooldown has expired — the
// pre-filter admits it, and suppression happens in itemNeedsWork (not the pre-filter).
// After the deep-fetch, the deferred refresh sets a new CooldownAt so the next poll
// cycle is suppressed (see TestPoll_CruiseValidateComplete_NoRepeatDeepFetch).
func TestPollPreFilter_CompleteStage_ExpiredCooldown_DeepFetched(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 70, Status: "Research", ItemID: "PVTI_70", UpdatedAt: ts, Labels: []string{"stage:Research:complete"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 70, Reason: "periodic-re-eval", Until: time.Now().Add(-2 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !deepFetched {
		t.Error("stage-complete item with expired cooldown should be deep-fetched (suppression is in itemNeedsWork, not the pre-filter)")
	}
}

// TestPollPreFilter_IncompleteStage_ExpiredCooldown_Admitted verifies that an item
// WITHOUT a stage:X:complete label is deep-fetched when the cooldown has expired —
// the stage-complete suppression only applies to completed stages.
func TestPollPreFilter_IncompleteStage_ExpiredCooldown_Admitted(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 71, Status: "Research", ItemID: "PVTI_71", UpdatedAt: ts}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 71, Reason: "periodic-re-eval", Until: time.Now().Add(-2 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !deepFetched {
		t.Error("incomplete stage with expired cooldown should be deep-fetched")
	}
}

// TestPollPreFilter_AwaitingReview_WithCompleteLabel_ExpiredCooldown_Admitted verifies
// the critical interaction: an item with BOTH stage:X:complete AND fabrik:awaiting-review
// must still be deep-fetched when the cooldown expires, so Phase 1/Phase 2
// review-reprompt timers can fire. The stage-complete suppression explicitly exempts
// awaiting-review items for this reason.
func TestPollPreFilter_AwaitingReview_WithCompleteLabel_ExpiredCooldown_Admitted(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	var deepFetched bool
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{{Number: 69, Status: "Research", ItemID: "PVTI_69", UpdatedAt: ts, Labels: []string{"stage:Research:complete", "fabrik:awaiting-review"}}},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error { deepFetched = true; return nil },
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 69, Reason: "review-blocked", Until: time.Now().Add(-2 * time.Minute)})
	if _, err := eng.poll(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !deepFetched {
		t.Error("item with stage:X:complete AND fabrik:awaiting-review and expired cooldown must be deep-fetched (awaiting-review exempts from stage-complete suppression)")
	}
}


// ── fabrik:awaiting-ci conjunctive gate dispatch guards ──────────────────────

// TestItemNeedsWork_AwaitingCI_NoDispatch verifies that itemNeedsWork returns
// false when fabrik:awaiting-ci is present on a wait_for_ci: true stage (R3).
// The catch-up loop evaluates CI; processItem must not be called.
func TestItemNeedsWork_AwaitingCI_NoDispatch(t *testing.T) {
	tr := true
	stgs := []*stages.Stage{{Name: "Validate", Order: 3, WaitForCI: &tr}}
	eng := testEngineWithStages(&mockGitHubClient{}, stgs)
	item := gh.ProjectItem{
		Number: 64,
		Status: "Validate",
		Labels: []string{"fabrik:awaiting-ci"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork must return false when fabrik:awaiting-ci is present on a wait_for_ci stage (R3)")
	}
}

// TestItemNeedsWork_AwaitingCI_NonCIGatedStage_Dispatches verifies that
// fabrik:awaiting-ci does NOT suppress dispatch for non-wait_for_ci stages.
// A stale label from a prior CI-gated invocation must not permanently block
// a stage that was moved to a different (non-CI-gated) column.
func TestItemNeedsWork_AwaitingCI_NonCIGatedStage_Dispatches(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 67,
		Status: "Research", // no wait_for_ci
		Labels: []string{"fabrik:awaiting-ci"},
	}
	if !eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork must not suppress dispatch for fabrik:awaiting-ci on a non-CI-gated stage")
	}
}

// TestItemMayNeedWork_ClosedIssue_AwaitingCI_Passes verifies that a closed item
// with fabrik:awaiting-ci passes the closed-issue guard in itemMayNeedWork, so
// the catch-up loop can complete the CI gate after a PR merge closes the issue.
func TestItemMayNeedWork_ClosedIssue_AwaitingCI_Passes(t *testing.T) {
	tr := true
	stgs := []*stages.Stage{{Name: "Validate", Order: 3, WaitForCI: &tr}}
	eng := testEngineWithStages(&mockGitHubClient{}, stgs)
	item := gh.ProjectItem{
		Number:   65,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-ci"},
	}
	// itemMayNeedWork should NOT filter this closed item — it has fabrik:awaiting-ci
	if !eng.itemMayNeedWork(item) {
		t.Error("closed item with fabrik:awaiting-ci should pass itemMayNeedWork closed-issue guard")
	}
}

// TestItemNeedsWork_ClosedIssue_AwaitingCI_Passes verifies that a closed item
// with fabrik:awaiting-ci on a wait_for_ci stage passes the closed-issue guard
// but is still filtered by the awaiting-ci dispatch gate (no Claude invocation).
func TestItemNeedsWork_ClosedIssue_AwaitingCI_Passes(t *testing.T) {
	tr := true
	stgs := []*stages.Stage{{Name: "Validate", Order: 3, WaitForCI: &tr}}
	eng := testEngineWithStages(&mockGitHubClient{}, stgs)
	item := gh.ProjectItem{
		Number:   66,
		Status:   "Validate",
		IsClosed: true,
		Labels:   []string{"fabrik:awaiting-ci"},
	}
	// Passes the closed-issue guard (fabrik:awaiting-ci present) but is filtered
	// by the awaiting-ci dispatch gate — the catch-up loop handles the CI gate.
	if eng.itemNeedsWork(item) {
		t.Error("closed item with fabrik:awaiting-ci on wait_for_ci stage should be filtered by awaiting-ci dispatch gate")
	}
}

// TestPoll_DeferredRefresh_DoesNotRefreshIncompleteItem verifies that the deferred
// CooldownAt["periodic-re-eval"] refresh in poll() does NOT refresh LastAttemptAt
// for incomplete items. Refreshing LastAttemptAt would defeat the retry-after-cooldown
// mechanism and block retries indefinitely (#504 regression fix).
func TestPoll_DeferredRefresh_DoesNotRefreshIncompleteItem(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 80, Status: "Research", UpdatedAt: ts},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Locked by another user — prevents dispatch. No stage:Research:complete.
			item.Labels = append(item.Labels, "fabrik:locked:otheruser")
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // cooldown = 10s

	initial := time.Now().Add(-2 * time.Minute) // expired timestamp
	// Seed LastAttemptAt to verify the deferred block never refreshes it
	eng.store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: 80, StageName: "Research", At: initial})
	// Seed expired CooldownAt so itemMayNeedWork enters the re-eval path (returns true → deep-fetch)
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 80, Reason: "periodic-re-eval", Until: initial})

	ctx := t.Context()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// LastAttemptAt["Research"] must NOT have been refreshed by the deferred block.
	// The deferred block only updates CooldownAt["periodic-re-eval"], never LastAttemptAt.
	// This is the structural fix for the #504 regression.
	snap, snapErr := eng.store.Get("owner/repo", 80)
	if snapErr != nil {
		t.Fatalf("store.Get after poll: %v", snapErr)
	}
	if time.Since(snap.LastAttemptAt("Research")) < 90*time.Second {
		t.Error("LastAttemptAt[Research] was refreshed by the deferred block; retry-after-cooldown is defeated (#504 regression)")
	}
}

// TestPoll_DeferredRefresh_RefreshesTerminalItem verifies that the deferred block in
// poll() DOES refresh CooldownAt["periodic-re-eval"] for terminal items — those where
// stage:X:complete appears in the full label set from deep-fetch (even if beyond the
// 15-label shallow query window). This is the #488 belt-and-suspenders behavior: it
// caps deep-fetch frequency for terminal items so they don't trigger a perpetual loop.
func TestPoll_DeferredRefresh_RefreshesTerminalItem(t *testing.T) {
	ts := time.Now().Add(-time.Minute)
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					// No stage:Research:complete in shallow labels — simulates
					// the label being beyond the first 15 shallow positions.
					{Number: 81, Status: "Research", UpdatedAt: ts},
				},
			}, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Full label set (from deep-fetch) has the complete label.
			item.Labels = append(item.Labels, "stage:Research:complete")
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 1 // cooldown = 10s

	// Seed expired CooldownAt so the pre-filter admits the item for deep-fetch
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 81, Reason: "periodic-re-eval", Until: time.Now().Add(-2 * time.Minute)})

	ctx := t.Context()
	if _, err := eng.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// CooldownAt["periodic-re-eval"] must have been refreshed by the deferred block.
	// Terminal items cap their own deep-fetch frequency via CooldownAt (#488 behavior preserved).
	snap, snapErr := eng.store.Get("owner/repo", 81)
	if snapErr != nil {
		t.Fatalf("store.Get after poll: %v", snapErr)
	}
	if time.Until(snap.CooldownAt("periodic-re-eval")) < -5*time.Second {
		t.Error("CooldownAt[periodic-re-eval] was NOT refreshed by the deferred block; #488 behavior is broken")
	}
}
