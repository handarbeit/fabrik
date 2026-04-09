// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"errors"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestRemoveEditingLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			if labelName == "fabrik:editing" {
				return errors.New("remove failed")
			}
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic when removal fails
	eng.removeEditingLabel("owner", "repo", 5)
}

func TestRemoveLockLabel_NonNotFoundError_LogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("network error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// non-ErrNotFound error should log a warning without panicking
	eng.removeLockLabel("owner", "repo", 5, "fabrik:locked:testuser")
}

func TestRemoveLockLabel_ErrNotFound_Ignored(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return gh.ErrNotFound
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// ErrNotFound should be silently ignored
	eng.removeLockLabel("owner", "repo", 5, "fabrik:locked:testuser")
}

func TestEnsureDraftPR_FindPRError_ReturnsZero(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Title: "Test"}
	prNum := eng.ensureDraftPR(item, "main")
	if prNum != 0 {
		t.Errorf("expected 0 on FindPRForIssue error, got %d", prNum)
	}
}

func TestEnsurePRLinksIssue_GetIssueBodyError_Logs(t *testing.T) {
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "", errors.New("not found")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic when GetIssueBody fails
	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 5}, 10)
}

func TestMarkPRReady_MarkReadyError_LogsWarning(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	client := &mockGitHubClient{
		markPRReadyFn: func(owner, repo string, prNumber int) error {
			return errors.New("api error")
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 7, Title: "test"}
	// Should not panic on MarkPRReady error
	eng.markPRReady(item, 55)
}

func TestAddFailedLabel_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.addFailedLabel("owner", "repo", 9, "Research")

	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 AddLabel call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != fmt.Sprintf("stage:Research:failed") {
		t.Errorf("label = %q", client.addLabelCalls[0].labelName)
	}
}

func TestAddFailedLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic
	eng.addFailedLabel("owner", "repo", 10, "Plan")
}

func TestRemoveFailedLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic
	eng.removeFailedLabel("owner", "repo", 10, "Plan")
}

func TestRemoveInProgressLabel_ErrorLogsWarning(t *testing.T) {
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return errors.New("api error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	// Should not panic
	eng.removeInProgressLabel("owner", "repo", 10, "Plan")
}
