package engine

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// setupCloseTestRepo builds a real bare-clone repo (default branch "main", via
// setupTrainRepo) with a "develop" branch present on origin, registers it on a
// fresh test Engine at "owner/repo", and returns the engine and client.
func setupCloseTestRepo(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	skipIfNoGit(t)
	_, _, worktreeRoot, wm := setupTrainRepo(t)

	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = wm.baseDir
	shaOut, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(string(shaOut))

	mustGitDir(t, wm.baseDir, "update-ref", "refs/remotes/origin/develop", sha)

	// Built directly via NewWithDeps with worktrees=nil rather than testEngine:
	// testEngine pre-registers a non-git placeholder WorktreeManager at
	// "owner/repo", and registerWorktrees is idempotent (a no-op when the key
	// is already present), so it would never install the real git-backed wm
	// built above.
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		&mockClaudeInvoker{},
		nil,
	)
	eng.registerWorktrees("owner/repo", wm.baseDir, worktreeRoot)
	return eng
}

func TestCloseIssueIfNonDefaultBase_BaseNotDefault_Closes(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error { return nil }}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}}
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected exactly one CloseIssue call, got %v", client.closeIssueCalls)
	}
	c := client.closeIssueCalls[0]
	if c.owner != "owner" || c.repo != "repo" || c.issueNumber != 42 {
		t.Errorf("unexpected CloseIssue args: %+v", c)
	}
}

func TestCloseIssueIfNonDefaultBase_NoBaseLabel_DoesNotClose(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error { return nil }}
	eng := setupCloseTestRepo(t, client)

	// No base: label — baseBranchForItem's contract guarantees this always
	// resolves to exactly the default, so the fast path must skip entirely
	// without even touching the WorktreeManager.
	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"stage:Validate:complete"}}
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for an item with no base: label, got %v", client.closeIssueCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_BaseLabelMatchesDefault_DoesNotClose(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error { return nil }}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:main"}}
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call when base: label matches the repo default (no double-close), got %v", client.closeIssueCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_AlreadyClosed_DoesNotCallClose(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error {
		t.Fatalf("CloseIssue should not be called for an already-closed item")
		return nil
	}}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}, IsClosed: true}
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for an already-closed item, got %v", client.closeIssueCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_ErrNotFound_TreatedAsSuccess(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error {
		return gh.ErrNotFound
	}}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}}

	// Must not panic and must return normally — ErrNotFound is a success case.
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected CloseIssue to have been attempted once, got %v", client.closeIssueCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_CloseFails_MarksOutstanding(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error {
		return fmt.Errorf("rate limited")
	}}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}}

	// Must not panic; the error must be logged, not silently discarded, and must
	// never block the caller (there is nothing further to assert here beyond
	// "returns normally" since this function has no error return).
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected CloseIssue to have been attempted once, got %v", client.closeIssueCalls)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	markerAdded := false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == 42 && c.labelName == nonDefaultBaseAwaitingCloseLabel {
			markerAdded = true
		}
	}
	if !markerAdded {
		t.Errorf("expected %s added on explicit close failure, got labels: %v", nonDefaultBaseAwaitingCloseLabel, client.addLabelCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_DefaultBaseBranchFails_SkipsClose(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error {
		t.Fatalf("CloseIssue should not be called when DefaultBaseBranch fails")
		return nil
	}}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	// A WorktreeManager whose baseDir is not a git repo at all — every step of
	// DefaultBaseBranch's fallback ladder fails deterministically.
	eng.registerWorktrees("owner/repo", t.TempDir(), t.TempDir())

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}}

	// Must not panic and must not block on the DefaultBaseBranch error.
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call when DefaultBaseBranch fails, got %v", client.closeIssueCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_IsPR_DoesNotClose(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error {
		t.Fatalf("CloseIssue should not be called for a PR board item")
		return nil
	}}
	eng := setupCloseTestRepo(t, client)

	item := gh.ProjectItem{Number: 42, Repo: "owner/repo", Labels: []string{"base:develop"}, IsPR: true}
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for a PR item regardless of base/default, got %v", client.closeIssueCalls)
	}
}

func TestCloseIssueIfNonDefaultBase_UnregisteredRepo_NoPanic(t *testing.T) {
	client := &mockGitHubClient{closeIssueFn: func(owner, repo string, n int) error {
		t.Fatalf("CloseIssue should not be called when no WorktreeManager is registered")
		return nil
	}}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// item.Repo has no registered WorktreeManager at all (distinct from
	// "owner/repo", the only key testEngine registers).
	item := gh.ProjectItem{Number: 42, Repo: "fail-test/unregistered", Labels: []string{"base:develop"}}

	// Must not panic — this mirrors the first-poll-after-restart scenario where
	// runValidatePRTerminalAdvance can reach a base:-labeled item before
	// ensureRepoReady has ever run for that repo this process lifetime.
	eng.closeIssueIfNonDefaultBase(item, 10)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call when no WorktreeManager is registered, got %v", client.closeIssueCalls)
	}
}
