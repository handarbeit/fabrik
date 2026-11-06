package engine

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestSettleBotServiceNotices_WatermarksQuotaNotice verifies that a bot
// quota-notice comment excluded by findNewComments gets a 🚀 reaction and a
// recorded CommentProcessed watermark from the settle scan, so it never
// re-admits on a later poll (#1083/#1088).
func TestSettleBotServiceNotices_WatermarksQuotaNotice(t *testing.T) {
	client := &mockGitHubClient{
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.stageByName("Research").FeedbackBots = []string{"gemini-code-assist"} // opted in (#2552)
	item := gh.ProjectItem{
		Number: 30,
		Status: "Research",
		Comments: []gh.Comment{
			{ID: "C_quota", DatabaseID: 601, Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{item}}

	eng.settleBotServiceNotices(board)

	if len(client.addCommentReactionCalls) != 1 {
		t.Fatalf("expected 1 AddCommentReaction call, got %d: %v", len(client.addCommentReactionCalls), client.addCommentReactionCalls)
	}
	if client.addCommentReactionCalls[0].content != "rocket" {
		t.Errorf("expected rocket reaction, got %q", client.addCommentReactionCalls[0].content)
	}
	snap, err := eng.store.Get("owner/repo", 30)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.CommentProcessed("C_quota").IsZero() {
		t.Error("expected C_quota to be recorded as CommentProcessed")
	}
}

// TestSettleBotServiceNotices_AlreadyProcessed_NoOp verifies idempotency: a
// comment already watermarked as CommentProcessed is not re-reacted.
func TestSettleBotServiceNotices_AlreadyProcessed_NoOp(t *testing.T) {
	client := &mockGitHubClient{
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.stageByName("Research").FeedbackBots = []string{"gemini-code-assist"} // opted in (#2552)
	item := gh.ProjectItem{
		Number: 31,
		Status: "Research",
		Comments: []gh.Comment{
			{ID: "C_quota", DatabaseID: 602, Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{item}}

	// First pass watermarks it.
	eng.settleBotServiceNotices(board)
	if len(client.addCommentReactionCalls) != 1 {
		t.Fatalf("expected 1 AddCommentReaction call after first pass, got %d", len(client.addCommentReactionCalls))
	}

	// Second pass over the same (unmutated) board must be a no-op.
	eng.settleBotServiceNotices(board)
	if len(client.addCommentReactionCalls) != 1 {
		t.Errorf("expected still 1 AddCommentReaction call after second pass (idempotent), got %d", len(client.addCommentReactionCalls))
	}
}

// TestSettleBotServiceNotices_NonNoticeComments_Untouched verifies that human
// comments and genuine bot review comments are never watermarked by this scan
// — only comment-processing's normal flow handles those.
func TestSettleBotServiceNotices_NonNoticeComments_Untouched(t *testing.T) {
	client := &mockGitHubClient{
		addCommentReactionFn: func(_, _ string, _ int, _ string) error {
			t.Fatal("AddCommentReaction should not be called for non-notice comments")
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 32,
		Comments: []gh.Comment{
			{ID: "C_human", DatabaseID: 603, Author: "humanuser", Body: "please continue"},
			{ID: "C_review", DatabaseID: 604, Author: "gemini-code-assist[bot]", Body: "CHANGES_REQUESTED: this loop never terminates."},
		},
	}
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{item}}

	eng.settleBotServiceNotices(board)

	snap, _ := eng.store.Get("owner/repo", 32)
	if !snap.CommentProcessed("C_human").IsZero() {
		t.Error("expected C_human to remain unprocessed")
	}
	if !snap.CommentProcessed("C_review").IsZero() {
		t.Error("expected C_review to remain unprocessed")
	}
}

// TestSettleBotServiceNotices_ReactionFailure_LeavesUnwatermarked verifies
// that a failed AddCommentReaction call leaves the comment unwatermarked (so
// it retries on the next poll) rather than panicking or recording a false
// CommentProcessed watermark.
func TestSettleBotServiceNotices_ReactionFailure_LeavesUnwatermarked(t *testing.T) {
	client := &mockGitHubClient{
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return errors.New("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 33,
		Comments: []gh.Comment{
			{ID: "C_quota", DatabaseID: 605, Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}
	board := &gh.ProjectBoard{Items: []gh.ProjectItem{item}}

	eng.settleBotServiceNotices(board)

	snap, _ := eng.store.Get("owner/repo", 33)
	if !snap.CommentProcessed("C_quota").IsZero() {
		t.Error("expected C_quota to remain unprocessed after a failed reaction call, so it retries next poll")
	}
}

// A notice from a bot that is not opted in is derived state (#2552, R5): the
// settle scan writes no reaction and no processed marker for it.
func TestSettleBotServiceNotices_UndeclaredBot_WritesNothing(t *testing.T) {
	client := &mockGitHubClient{
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 32,
		Status: "Research",
		Comments: []gh.Comment{
			{ID: "C_quota", DatabaseID: 603, Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}
	eng.settleBotServiceNotices(&gh.ProjectBoard{Items: []gh.ProjectItem{item}})

	if len(client.addCommentReactionCalls) != 0 {
		t.Fatalf("expected no reaction on an undeclared bot's comment, got %v", client.addCommentReactionCalls)
	}
	if snap, ok := eng.store.Peek("owner/repo", 32); ok && !snap.CommentProcessed("C_quota").IsZero() {
		t.Error("an undeclared bot's comment must not be recorded as processed")
	}
}
