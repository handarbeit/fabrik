package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestProcessComments_HonorsCompletionOnNonZeroExit verifies that when a comment
// invocation emits FABRIK_STAGE_COMPLETE but the Claude process exits non-zero (e.g.
// a timeout kill after the stage finished), comment processing still completes the
// stage — consistent with stage runs — and records the error separately rather than
// vetoing completion. Regression guard for the comment/stage asymmetry (#890 Req 2).
func TestProcessComments_HonorsCompletionOnNonZeroExit(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Marker emitted (completed=true) AND a non-zero process exit (err != nil).
			return "done.\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, errors.New("claude exited with status 1")
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1, Completion: stages.CompletionCriteria{Type: "claude"}}
	item := gh.ProjectItem{Number: 10, Body: "spec"}
	userComments := []gh.Comment{
		{ID: "C_1", DatabaseID: 101, Author: "testuser", Body: "finish it"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments returned error despite completion: %v", err)
	}

	// The stage must have completed (handleStageComplete ran → stage:Research:complete added).
	var sawComplete bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Research:complete" {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Errorf("expected stage:Research:complete to be added (completion honored on non-zero exit); addLabelCalls=%v", client.addLabelCalls)
	}

	// History must record BOTH facts: Completed (marker) AND Errored (non-zero exit).
	snap, err := eng.store.Get("owner/repo", 10)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	st := snap.State()
	if !st.LastInvocationCompleted {
		t.Error("LastInvocationCompleted = false, want true (marker honored despite error)")
	}
	if !st.LastInvocationErrored {
		t.Error("LastInvocationErrored = false, want true (non-zero exit recorded)")
	}
}

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

// TestProcessComments_NoWorkNeeded_SuppressesStageComment verifies that a
// comment-processing invocation whose output contains FABRIK_NO_WORK_NEEDED
// posts no reply comment (neither a new comment nor a rewrite of the
// existing stage comment) — silence, not a "not actionable" message,
// prevents re-triggering a subscribed bot into a runaway loop (#1083/#1088).
func TestProcessComments_NoWorkNeeded_SuppressesStageComment(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "not actionable.\nFABRIK_NO_WORK_NEEDED\n", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number:   14,
		Body:     "spec",
		Comments: []gh.Comment{},
	}
	userComments := []gh.Comment{
		{ID: "C_quota", DatabaseID: 401, Author: "gemini-code-assist[bot]", Body: "unrelated bot chatter"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if len(client.addCommentCalls) > 0 {
		t.Errorf("expected no AddComment call on FABRIK_NO_WORK_NEEDED verdict, got: %v", client.addCommentCalls)
	}
	if len(client.updateCommentCalls) > 0 {
		t.Errorf("expected no UpdateComment call on FABRIK_NO_WORK_NEEDED verdict, got: %v", client.updateCommentCalls)
	}
}

// TestProcessComments_NoWorkNeeded_SuppressesPostToPRComment covers the
// post_to_pr reply path — also gated on FABRIK_NO_WORK_NEEDED.
func TestProcessComments_NoWorkNeeded_SuppressesPostToPRComment(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "not actionable.\nFABRIK_NO_WORK_NEEDED\n", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 4, PostToPR: true}
	item := gh.ProjectItem{
		Number: 15,
		Body:   "spec",
	}
	userComments := []gh.Comment{
		{ID: "C_quota", DatabaseID: 402, Author: "gemini-code-assist[bot]", Body: "unrelated bot chatter"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if len(client.addCommentCalls) > 0 {
		t.Errorf("expected no AddComment call on FABRIK_NO_WORK_NEEDED verdict for post_to_pr stage, got: %v", client.addCommentCalls)
	}
}

// TestProcessComments_NoWorkNeeded_SuppressesReviewFeedbackSummary covers the
// review-reinvoke PR summary path — also gated on FABRIK_NO_WORK_NEEDED. All
// comments carry a ReviewThreadID so isReviewReinvoke is true; a linked PR
// exists via FindPRForIssue, so absent the fix this would post a summary.
func TestProcessComments_NoWorkNeeded_SuppressesReviewFeedbackSummary(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) { return 900, nil },
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "addressed, but nothing to do.\nFABRIK_NO_WORK_NEEDED\n", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Review", Order: 4}
	item := gh.ProjectItem{
		Number: 16,
		Body:   "spec",
	}
	reviewComments := []gh.Comment{
		{ID: "C_thread", DatabaseID: 403, Author: "copilot-bot", Body: "consider handling nil here", ReviewThreadID: "RT_1"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, reviewComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if len(client.addCommentCalls) > 0 {
		t.Errorf("expected no PR review-feedback summary on FABRIK_NO_WORK_NEEDED verdict, got: %v", client.addCommentCalls)
	}
}

// TestProcessComments_NoWorkNeeded_IssueBodyAndReactionsStillApply verifies
// that FABRIK_NO_WORK_NEEDED suppresses only the reply comment — the issue
// body update (FABRIK_ISSUE_UPDATE), 🚀 reactions marking input comments
// processed, and the editing-label removal must all still occur.
func TestProcessComments_NoWorkNeeded_IssueBodyAndReactionsStillApply(t *testing.T) {
	skipIfNoGit(t)

	var updatedBody string
	client := &mockGitHubClient{
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			updatedBody = body
			return nil
		},
		addCommentReactionFn: func(_, _ string, _ int, _ string) error { return nil },
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			output := "FABRIK_ISSUE_UPDATE_BEGIN\nno actual change needed\nFABRIK_ISSUE_UPDATE_END\nnothing to do here.\nFABRIK_NO_WORK_NEEDED\n"
			return output, false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number: 17,
		Body:   "old spec",
	}
	userComments := []gh.Comment{
		{ID: "C_u", DatabaseID: 404, Author: "testuser", Body: "is there anything left to do?"},
	}

	if err := eng.processComments(context.Background(), board, item, stage, userComments); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if updatedBody != "no actual change needed" {
		t.Errorf("updatedBody = %q, want issue body update to still apply", updatedBody)
	}
	sawRocket := false
	for _, rc := range client.addCommentReactionCalls {
		if rc.content == "rocket" {
			sawRocket = true
		}
	}
	if !sawRocket {
		t.Errorf("expected the input comment to still get a 🚀 reaction, got: %v", client.addCommentReactionCalls)
	}
	if len(client.addCommentCalls) > 0 {
		t.Errorf("expected no reply comment despite issue-body update, got: %v", client.addCommentCalls)
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
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
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

// TestIsBotServiceNotice covers the classifier's decision boundary: bot login
// AND a known quota/rate-limit pattern must both hold. Regression guard for
// #1083/#1088 (gemini-code-assist quota-exhaustion runaway loop).
func TestIsBotServiceNotice(t *testing.T) {
	tests := []struct {
		name string
		c    gh.Comment
		want bool
	}{
		{
			name: "gemini quota banner",
			c:    gh.Comment{Author: "gemini-code-assist[bot]", Body: "[!WARNING]\nYou have reached your daily quota limit."},
			want: true,
		},
		{
			name: "rate limit banner",
			c:    gh.Comment{Author: "some-bot[bot]", Body: "You have reached your rate limit. Please try again later."},
			want: true,
		},
		{
			name: "genuine bot review body",
			c:    gh.Comment{Author: "gemini-code-assist[bot]", Body: "CHANGES_REQUESTED\n\nline 42: this loop never terminates when the slice is empty."},
			want: false,
		},
		{
			name: "human authored quota-sounding text",
			c:    gh.Comment{Author: "humanuser", Body: "You have reached your daily quota limit for this API — worth noting in the docs."},
			want: false,
		},
		{
			name: "existing quota notice fixture body must not collide",
			c:    gh.Comment{Author: "gemini-code-assist", Body: "quota notice"},
			want: false,
		},
		{
			name: "bot with unrelated body",
			c:    gh.Comment{Author: "dependabot[bot]", Body: "Bumps foo from 1.0.0 to 1.0.1."},
			want: false,
		},
		{
			name: "gemini sunset notice",
			c: gh.Comment{Author: "gemini-code-assist[bot]", Body: "The consumer version of Gemini Code Assist on GitHub has been sunset. " +
				"All code review activity has officially ceased."},
			want: true,
		},
		{
			name: "gemini unsupported file type notice",
			c: gh.Comment{Author: "gemini-code-assist[bot]", Body: "Gemini is unable to generate a review for this pull request due to the " +
				"file types involved not being currently supported."},
			want: true,
		},
		{
			name: "coderabbit rate-limit notice (prose + structural marker)",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: coderabbitRateLimitFixture},
			want: true,
		},
		{
			name: "coderabbit rate-limit notice (contraction prose only, no marker)",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: "`@verveguy`, you've reached your PR review limit, so we couldn't start this review."},
			want: true,
		},
		{
			name: "coderabbit genuine walkthrough is not a notice",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: coderabbitWalkthroughFixture},
			want: false,
		},
		{
			// #1141 / #933: CodeRabbit's structurally-marked auto-generated
			// acknowledgement reply — content-free, and the trigger for the
			// #933 mention-reply loop.
			name: "coderabbit auto-generated reply: no action taken",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: coderabbitAutoReplyFixture("`@arbeithand`, acknowledged. No action taken.")},
			want: true,
		},
		{
			name: "coderabbit auto-generated reply: acknowledged, no action required",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: coderabbitAutoReplyFixture("`@arbeithand`, acknowledged—no action required.")},
			want: true,
		},
		{
			name: "coderabbit auto-generated reply: acknowledged. No action taken.",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: coderabbitAutoReplyFixture("`@arbeithand`, acknowledged. No action taken.")},
			want: true,
		},
		{
			name: "coderabbit auto-generated reply marker alone (no acknowledgement prose)",
			c:    gh.Comment{Author: "coderabbitai[bot]", Body: "<!-- This is an auto-generated reply by CodeRabbit -->"},
			want: true,
		},
		{
			name: "human comment discussing rate limiting is not a notice",
			c: gh.Comment{Author: "humanuser", Body: "We should double check the review limit reached handling in the client — " +
				"looks like it doesn't back off correctly when we've reached our rate limit."},
			want: false,
		},
		{
			// Regression guard: a genuine bot review that quotes the CodeRabbit
			// prose patterns verbatim (e.g. critiquing this very pattern list in
			// a diff review) must not itself be classified as a notice.
			name: "bot review quoting the pattern literals is not a notice",
			c: gh.Comment{Author: "coderabbitai[bot]", Body: "the two bare-phrase fallbacks, `\"review limit reached\"` and " +
				"`\"you've reached your pr review limit\"`, now live as literal substrings inside this exact file."},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBotServiceNotice(tt.c); got != tt.want {
				t.Errorf("isBotServiceNotice(%+v) = %v, want %v", tt.c, got, tt.want)
			}
		})
	}
}

// coderabbitRateLimitFixture reproduces the real observed shape of
// CodeRabbit's rate-limit notice: a change-stack banner, promo imagery, and
// collapsible help sections, wrapping the prose warning and the HTML comment
// marker CodeRabbit emits for these notices. At ~3.9k characters, this is a
// dedicated guard against any length-based classification shortcut — real
// CodeRabbit rate-limit comments run 3,000-4,400 characters, in contrast to
// Gemini's notices (<=133 characters above).
var coderabbitRateLimitFixture = `<!-- This is an auto-generated comment: rate limited by coderabbit.ai -->

> [!WARNING]
> ## Review limit reached
>
> ` + "`@verveguy`" + `, you've reached your PR review limit, so we couldn't start this review.
> Please add the review as a comment to trigger it manually, or contact us if you'd like to increase your limit.
>
> <details>
> <summary>🚦 How to resolve this issue?</summary>
>
> After doing any of the following options, you can immediately re-trigger the review by mentioning ` + "`@coderabbitai`" + ` in a new comment:
>
> - **Enable the ` + "`Reviews > Auto Review`" + ` setting**: Automatic reviews are being blocked by the settings in your ` + "`.coderabbit.yaml`" + ` file. Adjust these settings to re-enable automatic reviews.
> - **Add the review manually**: Trigger a new review by commenting ` + "`@coderabbitai review`" + ` on this PR.
> - **Increase your review limit**: Upgrade to a higher tier plan for more reviews per hour, or contact us to discuss your specific needs.
>
> </details>
>
> <details>
> <summary>📥 Commits</summary>
>
> Reviewing files that changed from the base of the PR and between 3fe88ec284b4efa5007315834493336143ecd6d5 and 6189433d8a2c1e4f9b0a5d3e7c8f1a2b3c4d5e6f.
>
> </details>

<!-- walkthrough_start -->

## Walkthrough

This pull request stack contains 4 pull requests that are queued for review. CodeRabbit will automatically pick up the next review once your current usage resets.

| Pull Request | Status |
| --- | --- |
| #1118 | Queued |
| #1119 | Queued |
| #1120 | Queued |
| #1122 | Queued |

<!-- walkthrough_end -->

<!-- estimated_effort_start -->

**Estimated code review effort**: N/A (review not started due to rate limiting)

<!-- estimated_effort_end -->

<details>
<summary>✨ Finishing touches</summary>

- [ ] 📝 Generate docstrings
- [ ] 🧪 Generate unit tests

</details>

<details>
<summary>🪧 Tips</summary>

### Chat

There are 3 ways to chat with [CodeRabbit](https://coderabbit.ai):

- Review comments: Directly reply to a review comment made by CodeRabbit. Example:
  - I pushed a fix in commit <commit_id>, please review it.
  - Explain this complex logic.
  - Open a follow-up GitHub issue for this discussion.
- Files and specific lines of code (under the "Files changed" tab): Tag ` + "`@coderabbitai`" + ` in a new review comment at the desired location with your query.
- PR comments: Tag ` + "`@coderabbitai`" + ` in a new PR comment to ask questions about the PR branch.

### Support

Need help? Create a ticket on our [support page](https://www.coderabbit.ai/contact-us/support) for assistance with any issues or questions.

Note: Free users have a limited number of reviews available for private repos, and comments/issue reviews are also limited.

### CodeRabbit Commands (Invoked using PR comments)

- ` + "`@coderabbitai pause`" + ` to pause the reviews on a PR.
- ` + "`@coderabbitai resume`" + ` to resume the reviews on a PR.
- ` + "`@coderabbitai review`" + ` to trigger an incremental review of this PR.
- ` + "`@coderabbitai full review`" + ` to do a full review from scratch and review all the files again.
- ` + "`@coderabbitai summary`" + ` to regenerate the summary of the PR.
- ` + "`@coderabbitai generate docstrings`" + ` to generate docstrings for this PR.
- ` + "`@coderabbitai plan`" + ` to trigger planning for file edits.
- ` + "`@coderabbitai resolve`" + ` to resolve all the CodeRabbit review comments.
- ` + "`@coderabbitai configuration`" + ` to show the current CodeRabbit configuration for the repository.
- ` + "`@coderabbitai help`" + ` to get help.

### Other keywords and placeholders

- Add ` + "`@coderabbitai ignore`" + ` anywhere in the PR description to prevent this PR from being reviewed.
- Add ` + "`@coderabbitai summary`" + ` to generate the high-level summary at a specific location in the PR description.
- Add ` + "`@coderabbitai`" + ` anywhere in the PR title to generate the title automatically.

### CodeRabbit Configuration File

You can programmatically configure CodeRabbit by adding a ` + "`.coderabbit.yaml`" + ` file to the root of your repository.

</details>

<!-- This is an auto-generated comment: rate limited by coderabbit.ai -->
`

// coderabbitWalkthroughFixture reproduces the real structure of a genuine
// CodeRabbit review comment (summary + per-file walkthrough table), used to
// verify the CodeRabbit patterns don't false-positive on substantive review
// content in the same shape as the notice above.
var coderabbitWalkthroughFixture = `<!-- walkthrough_start -->

## Walkthrough

This change extends ` + "`isBotServiceNotice`" + ` to recognize additional bot vendor notices and adds table-driven test coverage for the new patterns.

## Changes

| Cohort / File(s) | Summary |
| --- | --- |
| **Bot notice patterns**<br>` + "`engine/comments.go`" + ` | Adds CodeRabbit and additional Gemini patterns to ` + "`botServiceNoticePatterns`" + `. |
| **Tests**<br>` + "`engine/comments_test.go`" + ` | Adds table cases covering the new patterns and a negative case for genuine review content. |

## Sequence Diagram(s)

` + "```" + `mermaid
sequenceDiagram
    participant Engine
    participant Classifier as isBotServiceNotice
    Engine->>Classifier: comment
    Classifier-->>Engine: notice / not a notice
` + "```" + `

<!-- walkthrough_end -->

<details>
<summary>✨ Finishing touches</summary>

- [ ] 📝 Generate docstrings

</details>
`

// coderabbitAutoReplyFixture reproduces the real structure of a CodeRabbit
// auto-generated acknowledgement reply: the HTML comment marker, a "For best
// results" tip, and the bot's acknowledgement prose (which varies by
// phrasing — that's why the marker, not the prose, is the pattern matched).
func coderabbitAutoReplyFixture(acknowledgement string) string {
	return "<!-- This is an auto-generated reply by CodeRabbit -->\n" +
		"> [!TIP]\n" +
		"> For best results, initiate chat on the files or code changes.\n\n" +
		acknowledgement
}

// TestFindNewComments_SkipsBotServiceNotice verifies the pre-admission filter
// excludes a quota/rate-limit bot notice while still admitting a genuine bot
// review comment and a human comment in the same batch.
func TestFindNewComments_SkipsBotServiceNotice(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 21,
		Comments: []gh.Comment{
			{ID: "C_quota", DatabaseID: 501, Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
			{ID: "C_review", DatabaseID: 502, Author: "gemini-code-assist[bot]", Body: "CHANGES_REQUESTED: this loop never terminates."},
			{ID: "C_human", DatabaseID: 503, Author: "humanuser", Body: "please address the review feedback"},
		},
	}

	newComments := eng.findNewComments(item)
	var gotIDs []string
	for _, c := range newComments {
		gotIDs = append(gotIDs, c.ID)
	}
	if len(newComments) != 2 {
		t.Fatalf("expected 2 new comments (quota notice excluded), got %d: %v", len(newComments), gotIDs)
	}
	for _, want := range []string{"C_review", "C_human"} {
		found := false
		for _, id := range gotIDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q to be admitted, got %v", want, gotIDs)
		}
	}
}

// TestFindNewComments_SkipsCodeRabbitRateLimitButAdmitsWalkthrough verifies
// that a rate-limited CodeRabbit notice and a genuine CodeRabbit walkthrough
// on the same PR are classified independently by content, not by author —
// the notice is excluded while the walkthrough is admitted (observed on
// #1103 and #1116).
func TestFindNewComments_SkipsCodeRabbitRateLimitButAdmitsWalkthrough(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 22,
		Comments: []gh.Comment{
			{ID: "C_ratelimit", DatabaseID: 601, Author: "coderabbitai[bot]", Body: coderabbitRateLimitFixture},
			{ID: "C_walkthrough", DatabaseID: 602, Author: "coderabbitai[bot]", Body: coderabbitWalkthroughFixture},
		},
	}

	newComments := eng.findNewComments(item)
	var gotIDs []string
	for _, c := range newComments {
		gotIDs = append(gotIDs, c.ID)
	}
	if len(newComments) != 1 || gotIDs[0] != "C_walkthrough" {
		t.Fatalf("expected only C_walkthrough admitted (rate-limit notice excluded), got %v", gotIDs)
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

// Finding 4 (#1375): a batch of only synthetic review-body comments (no
// inline thread comments at all — the exact shape a CHANGES_REQUESTED review
// with no inline comments produces) must still classify as a review reinvoke,
// so publishCommentOutput posts the PR feedback summary.
func TestIsReviewReinvoke_AllReviewBodyIDs_ReturnsTrue(t *testing.T) {
	comments := []gh.Comment{
		{ID: "review-body:555", ReviewThreadID: ""},
	}
	if !isReviewReinvoke(comments) {
		t.Error("expected true when all comments carry the review-body: ID prefix")
	}
}

// A mixed batch (some real thread comments plus a review-body comment) must
// also classify as a review reinvoke — the ID-prefix discriminator handles
// the case ReviewThreadID alone cannot (a body-derived comment has no thread).
func TestIsReviewReinvoke_MixedThreadAndBody_ReturnsTrue(t *testing.T) {
	comments := []gh.Comment{
		{ID: "C_1", ReviewThreadID: "RT_abc"},
		{ID: "review-body:555", ReviewThreadID: ""},
	}
	if !isReviewReinvoke(comments) {
		t.Error("expected true for a mixed thread-comment + review-body batch")
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

// TestProcessComments_AllNoticeReviewThreadComments_NoOp verifies the #1221
// chokepoint: when the caller supplies no comments and every unresolved
// item.LinkedPRReviewThreadComments entry is a bot service notice, the merge
// pulls the notice into the working slice but filterBotServiceNotices then
// empties it, so processComments returns before any side effect — no Claude
// invocation, no editing label, no reaction.
func TestProcessComments_AllNoticeReviewThreadComments_NoOp(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number: 40,
		Body:   "spec",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "RT_notice", DatabaseID: 901, ReviewThreadID: "thread1", Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
		},
	}

	if err := eng.processComments(context.Background(), board, item, stage, nil); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if len(claude.forCommentsCalls) != 0 {
		t.Errorf("expected 0 InvokeForComments calls, got %d", len(claude.forCommentsCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:editing" {
			t.Errorf("expected no fabrik:editing label added, got %v", client.addLabelCalls)
		}
	}
	if len(client.addCommentReactionCalls) != 0 {
		t.Errorf("expected 0 AddCommentReaction calls, got %v", client.addCommentReactionCalls)
	}
	if len(client.addPRReviewCommentReactionCalls) != 0 {
		t.Errorf("expected 0 AddPRReviewCommentReaction calls, got %v", client.addPRReviewCommentReactionCalls)
	}
}

// TestProcessComments_MixedNoticeAndLegitReviewThreadComments_FiltersOnlyNotice
// verifies the #1221 chokepoint doesn't over-filter: a bot service notice
// mixed with a genuine review comment in item.LinkedPRReviewThreadComments
// results in exactly one InvokeForComments call, and only the legitimate
// comment reaches it.
func TestProcessComments_MixedNoticeAndLegitReviewThreadComments_FiltersOnlyNotice(t *testing.T) {
	skipIfNoGit(t)

	var seenComments []gh.Comment
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			seenComments = comments
			return "addressed feedback", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number: 41,
		Body:   "spec",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "RT_notice", DatabaseID: 902, ReviewThreadID: "thread1", Author: "gemini-code-assist[bot]", Body: "You have reached your daily quota limit."},
			{ID: "RT_legit", DatabaseID: 903, ReviewThreadID: "thread2", Author: "coderabbitai[bot]", Body: "CHANGES_REQUESTED: this loop never terminates."},
		},
	}

	if err := eng.processComments(context.Background(), board, item, stage, nil); err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if len(claude.forCommentsCalls) != 1 {
		t.Fatalf("expected exactly 1 InvokeForComments call, got %d", len(claude.forCommentsCalls))
	}
	if len(seenComments) != 1 {
		t.Fatalf("expected exactly 1 comment passed to InvokeForComments, got %d: %v", len(seenComments), seenComments)
	}
	if seenComments[0].ID != "RT_legit" {
		t.Errorf("expected the legitimate comment to survive, got %q", seenComments[0].ID)
	}
}
