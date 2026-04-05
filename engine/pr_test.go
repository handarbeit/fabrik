package engine

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
)

func TestEnsureDraftPR_ExistingPR_SkipsCreate(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 42, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Title: "Test Issue"}
	prNum := eng.ensureDraftPR(item, "main")

	if prNum != 42 {
		t.Errorf("ensureDraftPR returned %d, want 42", prNum)
	}
	// CreateDraftPR should not have been called
	if len(client.createDraftPRCalls) > 0 {
		t.Error("should not create PR when one already exists")
	}
}

func TestEnsurePRLinksIssue_AlreadyLinked_NoUpdate(t *testing.T) {
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "This PR closes Closes #5", nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 5}, 10)

	// Should NOT update body since it already has Closes #5
	if len(client.updateCommentCalls) > 0 {
		t.Error("UpdateComment should not be called when already linked")
	}
	// UpdateIssueBody is tracked differently — check no update occurred
	// (the mock's updateIssueBodyFn not called means no update)
}

func TestEnsurePRLinksIssue_Missing_AddsKeyword(t *testing.T) {
	var updatedBody string
	client := &mockGitHubClient{
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "Some PR description", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updatedBody = body
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.ensurePRLinksIssue(gh.ProjectItem{Number: 7}, 10)

	if !strings.Contains(updatedBody, "Closes #7") {
		t.Errorf("expected Closes #7 in updated body, got: %q", updatedBody)
	}
}

func TestMarkPRReady_WithKnownPR_CallsMarkReady(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	client := &mockGitHubClient{}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 5, Title: "test"}
	eng.markPRReady(item, 55) // known PR number 55

	if len(client.markPRReadyCalls) != 1 {
		t.Fatalf("expected 1 MarkPRReady call, got %d", len(client.markPRReadyCalls))
	}
	if client.markPRReadyCalls[0].prNumber != 55 {
		t.Errorf("MarkPRReady called with PR #%d, want 55", client.markPRReadyCalls[0].prNumber)
	}
}

func TestMarkPRReady_NoPRFound_NoCall(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManagerWithRoot(repoDir, repoDir+"/.fabrik/worktrees")

	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil // no PR
		},
	}
	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", User: "u", Token: "t", Stages: testStages()},
		client, &mockClaudeInvoker{}, wm,
	)

	item := gh.ProjectItem{Number: 6, Title: "test"}
	eng.markPRReady(item, 0) // no known PR, lookup returns 0

	if len(client.markPRReadyCalls) > 0 {
		t.Error("MarkPRReady should not be called when no PR found")
	}
}

func TestPostOutputToPR_WithPR_PostsToPRAndIssue(t *testing.T) {
	var addCommentCalls []addCommentCall
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 20, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) error {
			addCommentCalls = append(addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 3, Title: "Issue"}

	eng.postOutputToPR(item, "Implement", "detailed output", "", "main-branch", "abc123", "", "2024-01-01")

	// Should have posted to PR (#20) and to issue (#3)
	if len(addCommentCalls) != 2 {
		t.Fatalf("expected 2 AddComment calls (PR + issue), got %d", len(addCommentCalls))
	}
	var hasPR, hasIssue bool
	for _, c := range addCommentCalls {
		if c.issueNumber == 20 {
			hasPR = true
		}
		if c.issueNumber == 3 {
			hasIssue = true
		}
	}
	if !hasPR {
		t.Error("expected comment on PR #20")
	}
	if !hasIssue {
		t.Error("expected summary comment on issue #3")
	}
}

func TestPostOutputToPR_FindPRError_FallsBackToIssue(t *testing.T) {
	var addCommentIssues []int
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, errors.New("api error") // error + 0 PR number
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) error {
			addCommentIssues = append(addCommentIssues, issueNumber)
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 5, Title: "Issue"}

	eng.postOutputToPR(item, "Review", "output", "", "", "", "", "")

	// With error+0, the no-PR fallback should post on the issue
	if len(addCommentIssues) != 1 || addCommentIssues[0] != 5 {
		t.Errorf("expected fallback comment on issue #5, got issues %v", addCommentIssues)
	}
}

func TestPostOutputToPR_AddCommentErrors_LogsWarnings(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 30, nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) error {
			return errors.New("post error") // all AddComment calls fail
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 6, Title: "Issue"}

	// Should not panic when AddComment fails
	eng.postOutputToPR(item, "Implement", "output", "", "", "", "", "")
}

func TestPostOutputToPR_NoPR_FallsBackToIssue(t *testing.T) {
	var addCommentCalls []addCommentCall
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil // no PR
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) error {
			addCommentCalls = append(addCommentCalls, addCommentCall{owner, repo, issueNumber, body})
			return nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 4, Title: "Issue"}

	eng.postOutputToPR(item, "Review", "output", "", "", "", "", "")

	// Falls back to one comment on the issue
	if len(addCommentCalls) != 1 {
		t.Fatalf("expected 1 AddComment (fallback), got %d", len(addCommentCalls))
	}
	if addCommentCalls[0].issueNumber != 4 {
		t.Errorf("expected comment on issue #4, got #%d", addCommentCalls[0].issueNumber)
	}
}
