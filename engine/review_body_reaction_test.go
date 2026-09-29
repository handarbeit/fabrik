package engine

import (
	"context"
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func reviewBodyReactionFixture(t *testing.T, comments []gh.Comment, client *mockGitHubClient) {
	t.Helper()
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(s *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "handled", false, TokenUsage{TurnsUsed: 1}, nil
		},
	}
	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", CommentMaxTurns: 5}
	item := gh.ProjectItem{Number: 616, Body: "spec"}
	if err := eng.processComments(context.Background(), board, item, stage, comments); err != nil {
		t.Fatalf("processComments: %v", err)
	}
}

// #1953 R8: a synthetic review-body comment (no DatabaseID, a node ID) gets 👀
// when dispatched and 🚀 when processing finishes, through GraphQL
// addReaction. Without the fix both sites hit the "skipping … (no DatabaseID)"
// path and no AddReviewReaction call is ever made.
func TestReviewBodyComment_ReactedToAtDispatchAndCompletion(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	reviewBodyReactionFixture(t, []gh.Comment{
		{ID: "review-body:5356295491", Author: "handarbeit-pruefer", Body: "finding", ReactionNodeID: "PRR_kwDOabc"},
	}, client)

	calls := client.addReviewReactionCalls
	if len(calls) != 2 {
		t.Fatalf("AddReviewReaction calls = %+v, want exactly 👀 then 🚀", calls)
	}
	if calls[0] != (addReviewReactionCall{"PRR_kwDOabc", "eyes"}) {
		t.Errorf("first call = %+v, want eyes on PRR_kwDOabc", calls[0])
	}
	if calls[1] != (addReviewReactionCall{"PRR_kwDOabc", "rocket"}) {
		t.Errorf("second call = %+v, want rocket on PRR_kwDOabc", calls[1])
	}
	// The one REST 🚀 is the engine's own posted output comment (the mock's
	// AddComment returns ID 0); the review body itself must never go through
	// REST — there is no 👀 there at all.
	for _, c := range client.addCommentReactionCalls {
		if c.content == "eyes" {
			t.Errorf("review body must not use the REST reaction endpoint: %+v", c)
		}
	}
	if len(client.addPRReviewCommentReactionCalls) != 0 {
		t.Errorf("review body must not use the inline-comment endpoint: %v", client.addPRReviewCommentReactionCalls)
	}
}

// A failing addReaction is logged and non-fatal: processing still completes and
// the comment is still marked processed, exactly as for comment reactions.
func TestReviewBodyComment_ReactionFailureIsNonFatal(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{
		addReviewReactionFn: func(string, string) error { return errors.New("resource not accessible by integration") },
	}
	reviewBodyReactionFixture(t, []gh.Comment{
		{ID: "review-body:77", Author: "handarbeit-pruefer", Body: "finding", ReactionNodeID: "PRR_x"},
	}, client)
	if got := len(client.addReviewReactionCalls); got != 2 {
		t.Errorf("AddReviewReaction calls = %d, want 2 (a failure on 👀 must not stop 🚀)", got)
	}
}

// A synthetic comment with neither a DatabaseID nor a node ID (CI-fix and
// rebase synthetics) still skips reactions entirely.
func TestSyntheticComment_NoNodeID_StillSkipsReactions(t *testing.T) {
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	reviewBodyReactionFixture(t, []gh.Comment{
		{ID: "ci-fix:1", Author: "fabrik", Body: "CI failed"},
	}, client)
	if len(client.addReviewReactionCalls) != 0 {
		t.Errorf("expected no review reactions, got %v", client.addReviewReactionCalls)
	}
	for _, c := range client.addCommentReactionCalls {
		if c.content == "eyes" {
			t.Errorf("synthetic comment with no IDs must not be 👀-reacted: %+v", c)
		}
	}
}

// buildReviewBodyCommentsFromReviews must carry the review's node ID onto the
// synthetic comment — that is what makes the reaction branch reachable.
func TestBuildReviewBodyComments_CarriesReactionNodeID(t *testing.T) {
	eng := testEngineWithRepo(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 616}
	got := eng.buildReviewBodyCommentsFromReviews(item, []gh.PRReview{
		{Author: "handarbeit-pruefer", State: "COMMENTED", Body: "finding", DatabaseID: 5356295491, NodeID: "PRR_kwDOabc"},
	})
	if len(got) != 1 {
		t.Fatalf("got %d comments, want 1", len(got))
	}
	if got[0].ReactionNodeID != "PRR_kwDOabc" || got[0].DatabaseID != 0 {
		t.Errorf("comment = %+v, want ReactionNodeID PRR_kwDOabc and no DatabaseID", got[0])
	}
}
