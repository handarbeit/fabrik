package engine

import (
	"context"
	"errors"
	"os"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Focused tests for the helpers extracted out of processItem (#1029). These call
// the helpers directly rather than driving the whole processItem pipeline
// (worktree setup, dependency checks, Claude invocation), which is exactly the
// isolated testability the decomposition is meant to enable.

func TestAcquireLockAndVerify_TieBreakLoss(t *testing.T) {
	// Competing lock from "aardvark" — lexicographically before "testuser" — wins
	// the tie-break, so this instance must yield and release what it acquired.
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:locked:testuser", "fabrik:locked:aardvark"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, client, claude)

	stage := &stages.Stage{Name: "Research"}
	item := gh.ProjectItem{Number: 10, Title: "Test issue"}

	release, _, workerDone, ok, _ := eng.acquireLockAndVerify(context.Background(), item, stage, "owner", "repo", "owner/repo", "fabrik:locked:testuser")
	if ok {
		t.Fatal("expected ok=false on lost tie-break")
	}
	if release == nil {
		t.Fatal("expected a non-nil release closure")
	}
	if workerDone == nil {
		t.Error("workerDone should be the heartbeat channel even on a lost tie-break (caller still owns closing it)")
	} else {
		close(workerDone)
	}

	var lockRemoved, inProgressAdded bool
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:locked:testuser" {
			lockRemoved = true
		}
	}
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:in_progress" {
			inProgressAdded = true
		}
	}
	if !lockRemoved {
		t.Error("lock label should have been removed by the lost tie-break")
	}
	if inProgressAdded {
		t.Error("in_progress label should NOT be added when the tie-break is lost")
	}

	// release must be idempotent-safe to call again without panicking (processItem
	// never calls it twice on this path, but the closure itself must tolerate it).
	release()
}

func TestAcquireLockAndVerify_NoConflict_AddsInProgress(t *testing.T) {
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:locked:testuser"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, client, claude)

	stage := &stages.Stage{Name: "Research"}
	item := gh.ProjectItem{Number: 11, Title: "Test issue"}

	release, workerStartedAt, workerDone, ok, _ := eng.acquireLockAndVerify(context.Background(), item, stage, "owner", "repo", "owner/repo", "fabrik:locked:testuser")
	if workerDone != nil {
		defer close(workerDone)
	}
	if !ok {
		t.Fatal("expected ok=true when there is no competing lock")
	}
	if workerStartedAt.IsZero() {
		t.Error("expected a non-zero workerStartedAt")
	}

	var inProgressAdded bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:in_progress" {
			inProgressAdded = true
		}
	}
	if !inProgressAdded {
		t.Error("expected stage:Research:in_progress to be added")
	}

	release()
	var inProgressRemoved bool
	for _, call := range client.removeLabelCalls {
		if call.labelName == "stage:Research:in_progress" {
			inProgressRemoved = true
		}
	}
	if !inProgressRemoved {
		t.Error("release() should remove the in_progress label that was acquired")
	}
}

func TestHandleCleanupStage_Direct(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	if _, err := wm.EnsureWorktree(42, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var addedLabel string
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			addedLabel = labelName
			return nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token"},
		client, claude, wm,
	)

	stage := &stages.Stage{Name: "Done", CleanupWorktree: true}
	item := gh.ProjectItem{Number: 42, Title: "Test", ItemID: "PVTI_42"}

	eng.handleCleanupStage(item, stage, "owner/repo")

	if _, err := os.Stat(wm.WorktreeDir(42)); !os.IsNotExist(err) {
		t.Error("worktree directory should have been removed")
	}
	if addedLabel != "stage:Done:complete" {
		t.Errorf("completion label = %q, want stage:Done:complete", addedLabel)
	}
	snap, _ := eng.store.Get("owner/repo", 42)
	if snap.CooldownAt("periodic-re-eval").IsZero() {
		t.Error("CooldownAt[periodic-re-eval] should be set after cleanup stage")
	}
}

func TestHandleCleanupStage_AlreadyComplete_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, client, claude)

	stage := &stages.Stage{Name: "Done", CleanupWorktree: true}
	item := gh.ProjectItem{Number: 5, Title: "Test", Labels: []string{"stage:Done:complete"}}

	eng.handleCleanupStage(item, stage, "owner/repo")

	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label calls when already complete, got %d", len(client.addLabelCalls))
	}
}

// A lock write that fails refuses the dispatch (#2393): no worker handle, no
// in_progress write, and the error is handed back to the caller.
func TestAcquireLockAndVerify_LockWriteFailureRefuses(t *testing.T) {
	boom := errors.New("context deadline exceeded")
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, label string) error { return boom },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	stage := &stages.Stage{Name: "Research"}
	item := gh.ProjectItem{Number: 12, Title: "Test issue"}
	release, _, workerDone, ok, lockErr := eng.acquireLockAndVerify(context.Background(), item, stage, "owner", "repo", "owner/repo", "fabrik:locked:testuser")
	if ok || !errors.Is(lockErr, boom) {
		t.Fatalf("ok=%v lockErr=%v, want refusal with the write error", ok, lockErr)
	}
	if workerDone != nil {
		t.Error("no heartbeat channel expected on a refused lock")
	}
	if release == nil {
		t.Fatal("release must be non-nil")
	}
	release() // no-op, must not remove anything or panic
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:in_progress" {
			t.Error("in_progress must not be attempted after a failed lock write")
		}
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("unexpected label removals: %v", client.removeLabelCalls)
	}
}

// Neutralised (the seam), the pre-#2393 behaviour returns: the worker proceeds unlocked.
func TestAcquireLockAndVerify_LockWriteFailureProceedsWhenRefusalDisabled(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, label string) error {
			if label == "fabrik:locked:testuser" {
				return errors.New("boom")
			}
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.SetLockWriteRefusalDisabledForTest(true)
	_, _, _, ok, lockErr := eng.acquireLockAndVerify(context.Background(), gh.ProjectItem{Number: 13}, &stages.Stage{Name: "Research"}, "owner", "repo", "owner/repo", "fabrik:locked:testuser")
	if !ok || lockErr != nil {
		t.Fatalf("ok=%v lockErr=%v, want the legacy proceed-unlocked path", ok, lockErr)
	}
}
