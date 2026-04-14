package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// testEngineWithRepo creates an engine using a real git repo for worktree operations.
func testEngineWithRepo(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker) *Engine {
	t.Helper()
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)
	return NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		claude,
		wm,
	)
}

func TestProcessComments_CreatesNewStageComment(t *testing.T) {
	skipIfNoGit(t)

	var addCommentBody string
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) error {
			addCommentBody = body
			return nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Claude's response to comment", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number:   10,
		Body:     "spec",
		Comments: []gh.Comment{}, // no existing stage comment
	}
	userComments := []gh.Comment{
		{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "please research X"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// Should use AddComment (no existing stage comment to rewrite)
	if addCommentBody == "" {
		t.Fatal("expected AddComment to be called")
	}
	if len(client.updateCommentCalls) > 0 {
		t.Error("should not call UpdateComment when no existing stage comment")
	}
	// The posted comment should use the base stage name header
	if !strings.Contains(addCommentBody, "🏭 **Fabrik — stage: Research**") {
		t.Errorf("posted comment should use base stage name header, got: %q", addCommentBody[:min(100, len(addCommentBody))])
	}
}

func TestProcessComments_RewritesExistingStageComment(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "updated research output", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number: 11,
		Body:   "spec",
		Comments: []gh.Comment{
			// Existing stage comment to be rewritten
			{ID: "C_existing", DatabaseID: 200, Author: "fabrik-bot",
				Body: "🏭 **Fabrik — stage: Research**\nold research output"},
		},
	}
	userComments := []gh.Comment{
		{ID: "C_user", DatabaseID: 201, Author: "testuser", Body: "please update research"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// Should use UpdateComment (existing stage comment found)
	if len(client.updateCommentCalls) == 0 {
		t.Fatal("expected UpdateComment to be called")
	}
	call := client.updateCommentCalls[0]
	if call.commentID != 200 {
		t.Errorf("UpdateComment called with commentID=%d, want 200", call.commentID)
	}
	if !strings.Contains(call.body, "updated research output") {
		t.Errorf("updated body should contain new output, got: %q", call.body[:min(100, len(call.body))])
	}
	// Should not AddComment (since we rewrote existing)
	if len(client.addCommentCalls) > 0 {
		t.Error("should not AddComment when existing stage comment was found and rewritten")
	}
}

func TestProcessComments_PostToPR_AlwaysAddsNewComment(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "review comment output", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// PostToPR stage — should not rewrite, should always add new comment
	stage := &stages.Stage{Name: "Review", Order: 4, PostToPR: true}
	item := gh.ProjectItem{
		Number: 12,
		Body:   "spec",
		Comments: []gh.Comment{
			// Even with an existing stage comment, post_to_pr should not rewrite
			{ID: "C_existing", DatabaseID: 300, Author: "fabrik-bot",
				Body: "🏭 **Fabrik — stage: Review**\nold review output"},
		},
	}
	userComments := []gh.Comment{
		{ID: "C_user", DatabaseID: 301, Author: "testuser", Body: "please re-review"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// Should AddComment, not UpdateComment, for post_to_pr stages
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected AddComment to be called for post_to_pr stage")
	}
	if len(client.updateCommentCalls) > 0 {
		t.Error("should not UpdateComment for post_to_pr stage")
	}
}

func TestProcessComments_UpdatesIssueBodyOnMarker(t *testing.T) {
	skipIfNoGit(t)

	var updatedBody string
	client := &mockGitHubClient{
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updatedBody = body
			return nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "FABRIK_ISSUE_UPDATE_BEGIN\nnew issue body\nFABRIK_ISSUE_UPDATE_END\nstage comment content"
			return output, false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Specify", Order: 0, UpdateIssueBody: true}
	item := gh.ProjectItem{
		Number: 13,
		Body:   "old spec",
	}
	userComments := []gh.Comment{
		{ID: "C_u", DatabaseID: 400, Author: "testuser", Body: "update please"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// Issue body should be updated
	if updatedBody != "new issue body" {
		t.Errorf("updatedBody = %q, want %q", updatedBody, "new issue body")
	}

	// Stage comment should be posted with FABRIK_ISSUE_UPDATE stripped
	if len(client.addCommentCalls) > 0 {
		body := client.addCommentCalls[0].body
		if strings.Contains(body, "FABRIK_ISSUE_UPDATE") {
			t.Error("FABRIK_ISSUE_UPDATE block should be stripped from stage comment")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
