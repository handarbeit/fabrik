package engine

import (
	"context"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// Tests for the lock-then-verify protocol in processItem.
//
// After acquiring fabrik:locked:<user>, processItem sleeps lockVerifyDelay,
// re-fetches labels, and applies lexicographic tie-breaking when competing
// locks are present. Tests zero out lockVerifyDelay to avoid slow sleeps.

func TestLockThenVerify_NoCompetingLock_Proceeds(t *testing.T) {
	skipIfNoGit(t)

	// FetchLabels returns only our own lock — no conflict.
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:locked:testuser", "stage:Research:in_progress"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)

	orig := lockVerifyDelay
	lockVerifyDelay = 0
	defer func() { lockVerifyDelay = orig }()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Title:  "Test issue",
		Status: "Research",
	}

	// processItem will proceed past lock-then-verify and may error later
	// (e.g. in Claude invocation) — we only care about the locking behaviour.
	_ = eng.processItem(context.Background(), board, item)

	// Our lock must NOT have been removed by the loser path.
	lockLabel := "fabrik:locked:testuser"
	for _, call := range client.removeLabelCalls {
		if call.labelName == lockLabel && call.issueNumber == item.Number {
			t.Errorf("lock label %q was removed — should have proceeded as winner", lockLabel)
		}
	}

	// in_progress label should have been added (confirms we got past lock-then-verify).
	var inProgressAdded bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:in_progress" && call.issueNumber == item.Number {
			inProgressAdded = true
		}
	}
	if !inProgressAdded {
		t.Error("stage:Research:in_progress was not added — did not proceed past lock-then-verify")
	}
}

func TestLockThenVerify_CompetingLockHigherUser_Proceeds(t *testing.T) {
	skipIfNoGit(t)

	// FetchLabels returns our lock plus a competing lock from "zephyr".
	// "testuser" < "zephyr" lexicographically, so we win.
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:locked:testuser", "fabrik:locked:zephyr"}, nil
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)

	orig := lockVerifyDelay
	lockVerifyDelay = 0
	defer func() { lockVerifyDelay = orig }()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 11,
		Title:  "Test issue",
		Status: "Research",
	}

	_ = eng.processItem(context.Background(), board, item)

	// Winner must NOT remove its own lock.
	lockLabel := "fabrik:locked:testuser"
	for _, call := range client.removeLabelCalls {
		if call.labelName == lockLabel && call.issueNumber == item.Number {
			t.Errorf("lock label %q was removed — should have proceeded as winner (testuser < zephyr)", lockLabel)
		}
	}

	// in_progress label should have been added.
	var inProgressAdded bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:in_progress" && call.issueNumber == item.Number {
			inProgressAdded = true
		}
	}
	if !inProgressAdded {
		t.Error("stage:Research:in_progress was not added — winner did not proceed past lock-then-verify")
	}
}

func TestLockThenVerify_CompetingLockLowerUser_YieldsAndReturnsNil(t *testing.T) {
	// FetchLabels returns our lock plus a competing lock from "aardvark".
	// "testuser" > "aardvark" lexicographically, so we lose.
	// No git required — loser path returns before any worktree operations.
	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return []string{"fabrik:locked:testuser", "fabrik:locked:aardvark"}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	orig := lockVerifyDelay
	lockVerifyDelay = 0
	defer func() { lockVerifyDelay = orig }()

	// Pre-register the worktree manager so ensureRepoReady returns nil.
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager("/tmp/fake-repo")
	eng.mu.Unlock()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 12,
		Title:  "Test issue",
		Status: "Research",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("loser path should return nil, got: %v", err)
	}

	// Our lock must have been removed (releaseLock called by loser path).
	lockLabel := "fabrik:locked:testuser"
	var lockRemoved bool
	for _, call := range client.removeLabelCalls {
		if call.labelName == lockLabel && call.issueNumber == item.Number {
			lockRemoved = true
		}
	}
	if !lockRemoved {
		t.Errorf("lock label %q was not removed — loser should release its lock", lockLabel)
	}

	// in_progress label must NOT have been added (loser exits before that).
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:in_progress" && call.issueNumber == item.Number {
			t.Error("stage:Research:in_progress was added — loser should not have proceeded past lock-then-verify")
		}
	}

	// lockedIssues map should be clean after loser path.
	// issueKey format is "owner/repo#N" (no dash before #).
	eng.mu.Lock()
	_, stillLocked := eng.lockedIssues["owner/repo#12"]
	eng.mu.Unlock()
	if stillLocked {
		t.Error("lockedIssues entry should be cleared after loser releases lock")
	}
}

// TestLockThenVerify_FetchLabelsError_Proceeds verifies that a FetchLabels
// error is treated as non-fatal: the engine logs a warning and proceeds.
func TestLockThenVerify_FetchLabelsError_Proceeds(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{
		fetchLabelsFn: func(owner, repo string, issueNumber int) ([]string, error) {
			return nil, &testError{"simulated fetch error"}
		},
	}
	claude := &mockClaudeInvoker{}
	eng := testEngineWithRepo(t, client, claude)

	orig := lockVerifyDelay
	lockVerifyDelay = 0
	defer func() { lockVerifyDelay = orig }()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 13,
		Title:  "Test issue",
		Status: "Research",
	}

	_ = eng.processItem(context.Background(), board, item)

	// On FetchLabels error the engine should NOT remove the lock and should proceed.
	lockLabel := "fabrik:locked:testuser"
	for _, call := range client.removeLabelCalls {
		if call.labelName == lockLabel && call.issueNumber == item.Number {
			t.Errorf("lock removed despite FetchLabels error — should proceed on error")
		}
	}
}

// testError is a minimal error type for mock failures.
type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
