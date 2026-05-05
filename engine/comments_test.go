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
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			addCommentBody = body
			return 0, nil
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
	stage := &stages.Stage{Name: "Specify", Order: 0}
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

// TestFindNewComments_SkipsRocketReactedComment verifies that findNewComments
// skips a comment that has a 🚀 reaction even if the body lacks the Fabrik header.
// This is the defense-in-depth dedup signal for engine-authored comments.
func TestFindNewComments_SkipsRocketReactedComment(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 20,
		Comments: []gh.Comment{
			{
				ID:         "C_rocketed",
				DatabaseID: 500,
				Author:     "fabrik-bot",
				Body:       "Waiting for dependencies to close: #100", // no 🏭 header
				Reactions:  []gh.ReactionGroup{{Content: "ROCKET", Count: 1}},
			},
		},
	}

	newComments := eng.findNewComments(item)
	if len(newComments) != 0 {
		t.Errorf("expected 0 new comments (should skip rocket-reacted), got %d", len(newComments))
	}
}

// TestAddComment_ReactsWithRocket verifies that processComments adds a 🚀 reaction
// to every comment it posts via AddComment, using the returned database ID.
func TestAddComment_ReactsWithRocket(t *testing.T) {
	skipIfNoGit(t)

	const postedCommentID = 99

	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return postedCommentID, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "some output from Claude", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number:   21,
		Body:     "spec",
		Comments: []gh.Comment{}, // no existing stage comment → will call AddComment
	}
	userComments := []gh.Comment{
		{ID: "C_user", DatabaseID: 600, Author: "testuser", Body: "please do research"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// AddComment should have been called and produced a reaction call
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected AddComment to be called")
	}

	// Verify AddCommentReaction was called with the returned comment ID and "rocket"
	var rocketFound bool
	for _, rc := range client.addCommentReactionCalls {
		if rc.commentDatabaseID == postedCommentID && rc.content == "rocket" {
			rocketFound = true
			break
		}
	}
	if !rocketFound {
		t.Errorf("expected AddCommentReaction(_, _, %d, %q) to be called; got calls: %+v",
			postedCommentID, "rocket", client.addCommentReactionCalls)
	}
}

// ── isReviewReinvoke ──────────────────────────────────────────────────────────

func TestIsReviewReinvoke_AllReviewThreadIDs_ReturnsTrue(t *testing.T) {
	comments := []gh.Comment{
		{ID: "C_1", ReviewThreadID: "RT_abc"},
		{ID: "C_2", ReviewThreadID: "RT_def"},
	}
	if !isReviewReinvoke(comments) {
		t.Error("expected true when all comments have ReviewThreadID")
	}
}

func TestIsReviewReinvoke_MixedComments_ReturnsFalse(t *testing.T) {
	comments := []gh.Comment{
		{ID: "C_1", ReviewThreadID: "RT_abc"},
		{ID: "C_2", ReviewThreadID: ""},
	}
	if isReviewReinvoke(comments) {
		t.Error("expected false for mixed batch (some without ReviewThreadID)")
	}
}

func TestIsReviewReinvoke_NoReviewThreadIDs_ReturnsFalse(t *testing.T) {
	comments := []gh.Comment{
		{ID: "C_1", ReviewThreadID: ""},
		{ID: "C_2", ReviewThreadID: ""},
	}
	if isReviewReinvoke(comments) {
		t.Error("expected false when no comments have ReviewThreadID")
	}
}

func TestIsReviewReinvoke_EmptySlice_ReturnsFalse(t *testing.T) {
	if isReviewReinvoke(nil) {
		t.Error("expected false for nil slice")
	}
	if isReviewReinvoke([]gh.Comment{}) {
		t.Error("expected false for empty slice")
	}
}

// ── fabrik:extend-turns in comment processing ─────────────────────────────────

// TestCommentProcessingExtendTurnsLabelAbsent verifies that when fabrik:extend-turns
// is absent, MaxTurnsOverride=0 is passed to InvokeForComments (base budget used).
func TestCommentProcessingExtendTurnsLabelAbsent(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "comment output", false, TokenUsage{TurnsUsed: 3}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", CommentMaxTurns: 5}
	item := gh.ProjectItem{
		Number: 30,
		Body:   "spec",
		// No fabrik:extend-turns label
	}
	userComments := []gh.Comment{
		{ID: "C_1", DatabaseID: 800, Author: "testuser", Body: "please research"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	calls := claude.forCommentsCalls
	if len(calls) != 1 {
		t.Fatalf("expected 1 InvokeForComments call, got %d", len(calls))
	}
	if calls[0].opts.MaxTurnsOverride != 0 {
		t.Errorf("MaxTurnsOverride = %d, want 0 (label absent → base budget)", calls[0].opts.MaxTurnsOverride)
	}
}

// TestCommentProcessingExtendTurnsLabelPresent verifies that when fabrik:extend-turns
// is present, the first InvokeForComments call uses 2× commentMaxTurns as MaxTurnsOverride.
func TestCommentProcessingExtendTurnsLabelPresent(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "comment output", false, TokenUsage{TurnsUsed: 3}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", CommentMaxTurns: 5}
	item := gh.ProjectItem{
		Number: 31,
		Body:   "spec",
		Labels: []string{"fabrik:extend-turns"},
	}
	userComments := []gh.Comment{
		{ID: "C_2", DatabaseID: 801, Author: "testuser", Body: "please research"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	calls := claude.forCommentsCalls
	if len(calls) != 1 {
		t.Fatalf("expected 1 InvokeForComments call, got %d", len(calls))
	}
	wantOverride := 2 * commentMaxTurns(stage) // 2 × 5 = 10
	if calls[0].opts.MaxTurnsOverride != wantOverride {
		t.Errorf("MaxTurnsOverride = %d, want %d (2× commentMaxTurns)", calls[0].opts.MaxTurnsOverride, wantOverride)
	}
}

// TestCommentProcessingExtendTurnsProgressDetected verifies the full 2×→3× loop:
// label present, first invocation hits limit, progress detected → second invocation at 3× total.
// Uses Validate stage: detectProgress checks comment count via FetchItemDetails mock.
func TestCommentProcessingExtendTurnsProgressDetected(t *testing.T) {
	skipIfNoGit(t)

	const commentMaxTurnsVal = 5
	budget2x := 2 * commentMaxTurnsVal // 10
	budget1x := commentMaxTurnsVal     // 5 (second slot)

	var callCount int
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, nil
		},
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			// Simulate progress: add a new comment on re-fetch.
			item.Comments = append(item.Comments, gh.Comment{Body: "new comment"})
			return nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			if callCount == 1 {
				// Hit the budget without completing.
				return "partial output", false, TokenUsage{TurnsUsed: opts.MaxTurnsOverride}, nil
			}
			return "final output", true, TokenUsage{TurnsUsed: 3}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Validate", CommentMaxTurns: commentMaxTurnsVal}
	item := gh.ProjectItem{
		Number: 32,
		Body:   "spec",
		Labels: []string{"fabrik:extend-turns"},
		// No comments initially → baseline commentCount=0; FetchItemDetails adds one.
	}
	userComments := []gh.Comment{
		{ID: "C_3", DatabaseID: 802, Author: "testuser", Body: "please validate"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	calls := claude.forCommentsCalls
	if len(calls) != 2 {
		t.Fatalf("expected 2 InvokeForComments calls (2×→3× extension), got %d", len(calls))
	}
	if calls[0].opts.MaxTurnsOverride != budget2x {
		t.Errorf("first call MaxTurnsOverride = %d, want %d (2× budget)", calls[0].opts.MaxTurnsOverride, budget2x)
	}
	if calls[1].opts.MaxTurnsOverride != budget1x {
		t.Errorf("second call MaxTurnsOverride = %d, want %d (1× extension)", calls[1].opts.MaxTurnsOverride, budget1x)
	}
}

// TestCommentProcessingExtendTurnsNoProgress verifies that when label is present, budget is
// hit, but no progress is detected, there is no re-invoke.
// Uses Research stage: detectProgress always returns false for no-signal stages.
func TestCommentProcessingExtendTurnsNoProgress(t *testing.T) {
	skipIfNoGit(t)

	const commentMaxTurnsVal = 5
	budget2x := 2 * commentMaxTurnsVal // 10

	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Hit the budget without completing; no progress signal for Research stage.
			return "partial output", false, TokenUsage{TurnsUsed: opts.MaxTurnsOverride}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", CommentMaxTurns: commentMaxTurnsVal}
	item := gh.ProjectItem{
		Number: 33,
		Body:   "spec",
		Labels: []string{"fabrik:extend-turns"},
	}
	userComments := []gh.Comment{
		{ID: "C_4", DatabaseID: 803, Author: "testuser", Body: "please research"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	calls := claude.forCommentsCalls
	if len(calls) != 1 {
		t.Fatalf("expected 1 InvokeForComments call (no re-invoke on no-progress), got %d", len(calls))
	}
	if calls[0].opts.MaxTurnsOverride != budget2x {
		t.Errorf("MaxTurnsOverride = %d, want %d (2× budget pre-granted)", calls[0].opts.MaxTurnsOverride, budget2x)
	}
}
