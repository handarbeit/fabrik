package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// #2280: a comment-path run honours FABRIK_BLOCKED_ON_INPUT with the stage
// path's precedence (completion beats the marker; an error vetoes it).

const blockedCommentOutput = "I need a decision.\n\nFABRIK_SUMMARY_BEGIN\nWhich approach should I take?\nFABRIK_SUMMARY_END\n\nFABRIK_BLOCKED_ON_INPUT\n"

func blockedCommentEngine(t *testing.T, output string, completed bool) (*Engine, *mockGitHubClient) {
	t.Helper()
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(*stages.Stage, gh.ProjectItem, []gh.Comment, string, InvokeOptions) (string, bool, TokenUsage, error) {
			return output, completed, TokenUsage{}, nil
		},
	}
	return testEngineWithRepo(t, client, claude), client
}

func runBlockedComment(t *testing.T, eng *Engine) (didNotRunKind, error) {
	t.Helper()
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 2280, Body: "spec", Labels: []string{"stage:Review:complete"}}
	comments := []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "human", Body: "please fix"}}
	return eng.processCommentsClassified(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, comments)
}

func awaitingInputComments(c *mockGitHubClient) []string {
	var out []string
	for _, call := range c.addCommentCalls {
		if strings.Contains(call.body, "awaiting your input") {
			out = append(out, call.body)
		}
	}
	return out
}

func TestCommentPath_BlockedOnInputPausesWithQuestion(t *testing.T) {
	eng, client := blockedCommentEngine(t, blockedCommentOutput, false)
	kind, err := runBlockedComment(t, eng)
	if err != nil || kind != commentRunBlockedOnInput {
		t.Fatalf("kind=%q err=%v, want %q and nil", kind, err, commentRunBlockedOnInput)
	}
	added := labelsAdded(client)
	if !added["fabrik:paused"] || !added["fabrik:awaiting-input"] {
		t.Errorf("labels added = %v, want the paused/awaiting-input pair", added)
	}
	got := awaitingInputComments(client)
	if len(got) != 1 || !strings.Contains(got[0], "Which approach should I take?") {
		t.Errorf("awaiting-input comments = %q, want exactly one carrying the question", got)
	}
	// R2: the triggering comment is still marked processed.
	snap, _ := eng.store.Get("owner/repo", 2280)
	if snap.CommentProcessed("C_1").IsZero() {
		t.Error("the triggering comment was not marked processed")
	}
	// A question is no-progress to neither breaker.
	if n := snap.NoOpCommentCycles("Review"); n != 0 {
		t.Errorf("NoOpCommentCycles = %d, want 0 (breakers bypassed)", n)
	}
}

func TestCommentPath_CompletionBeatsBlockedMarker(t *testing.T) {
	eng, client := blockedCommentEngine(t, blockedCommentOutput+"FABRIK_STAGE_COMPLETE\n", true)
	kind, err := runBlockedComment(t, eng)
	if err != nil || kind == commentRunBlockedOnInput {
		t.Fatalf("kind=%q err=%v: completion must win over the blocked marker", kind, err)
	}
	if labelsAdded(client)["fabrik:paused"] || len(awaitingInputComments(client)) != 0 {
		t.Error("a completed run was paused")
	}
}

func TestCommentPath_NoMarkerUnchanged(t *testing.T) {
	eng, client := blockedCommentEngine(t, "Applied the change.\n", false)
	kind, err := runBlockedComment(t, eng)
	if err != nil || kind != "" {
		t.Fatalf("kind=%q err=%v, want empty and nil", kind, err)
	}
	if labelsAdded(client)["fabrik:paused"] || len(awaitingInputComments(client)) != 0 {
		t.Error("a run without the marker was paused")
	}
}

func TestCommentPath_BlockedOnInputSeamRestoresOldBehaviour(t *testing.T) {
	eng, client := blockedCommentEngine(t, blockedCommentOutput, false)
	eng.SetCommentBlockedOnInputDisabledForTest(true)
	kind, err := runBlockedComment(t, eng)
	if err != nil || kind == commentRunBlockedOnInput {
		t.Fatalf("kind=%q err=%v with the handling disabled", kind, err)
	}
	if labelsAdded(client)["fabrik:paused"] {
		t.Error("the disabled seam still paused the item")
	}
}

// dispatchReinvoke gives the blocked kind its own branch: the pre-charged cycle
// is refunded, no did-not-run state is recorded and opts.after never runs.
func TestDispatchReinvoke_BlockedOnInputRefundsAndSkipsAfter(t *testing.T) {
	eng, client := blockedCommentEngine(t, blockedCommentOutput, false)
	if _, err := eng.worktreesFor("owner/repo").EnsureWorktree(2280, "main", false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	stage := &stages.Stage{Name: "Review", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 2280, Repo: "owner/repo", Body: "spec", Labels: []string{"stage:Review:complete"}}

	refunds, afterCalls := 0, 0
	eng.dispatchReinvoke(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage, reinvokeOpts{
		tag:   "test",
		build: func(string) []gh.Comment { return []gh.Comment{{ID: "C_1", DatabaseID: 1, Author: "human", Body: "fix"}} },
		after: func(string, error) { afterCalls++ },
		cycle: cycleCharge{label: "review", refund: func(repo string, number int, stageName string) []itemstate.Mutation {
			refunds++
			return nil
		}},
	})
	eng.wg.Wait()

	if refunds != 1 {
		t.Errorf("cycle refunds = %d, want 1", refunds)
	}
	if afterCalls != 0 {
		t.Errorf("opts.after ran %d time(s) on a blocked run, want 0", afterCalls)
	}
	snap, _ := eng.store.Get("owner/repo", 2280)
	if n := snap.DidNotRunReinvokes("Review"); n != 0 {
		t.Errorf("DidNotRunReinvokes = %d, want 0 (the run executed)", n)
	}
	if !labelsAdded(client)["fabrik:paused"] {
		t.Error("the blocked reinvoke did not pause the item")
	}
}
