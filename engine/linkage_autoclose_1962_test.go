package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// #1962 R1 — the broken-linkage pause confirms against the PR body on every
// path. On 2026-09-30 GitHub stopped populating closingIssuesReferences for new
// PRs, so a default-base item whose PR body says "Closes #N" arrived with
// LinkedPRNumber == 0 and was paused as "broken linkage".

func brokenLinkageClient(closing func(owner, repo string, pr int) ([]int, error)) *mockGitHubClient {
	return &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: closing,
		addCommentFn:           func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
}

func pausedLabelAdded(c *mockGitHubClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.addLabelCalls {
		if l.labelName == "fabrik:paused" {
			return true
		}
	}
	return false
}

func TestHandleBrokenReviewLinkage_DefaultBaseBodyConfirms_NoPause(t *testing.T) {
	client := brokenLinkageClient(func(_, _ string, _ int) ([]int, error) { return []int{10}, nil })
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 0}

	paused, pr := eng.handleBrokenReviewLinkage("owner", "repo", item)
	if paused || pr != 77 {
		t.Errorf("PR body says Closes #10: want (paused=false, pr=77), got (%v, %d)", paused, pr)
	}
	if pausedLabelAdded(client) {
		t.Error("fabrik:paused must not be applied when the PR body confirms the linkage")
	}
}

func TestHandleBrokenReviewLinkage_DefaultBaseBodyMissingKeyword_Pauses(t *testing.T) {
	client := brokenLinkageClient(func(_, _ string, _ int) ([]int, error) { return nil, nil })
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 0}

	paused, _ := eng.handleBrokenReviewLinkage("owner", "repo", item)
	if !paused || !pausedLabelAdded(client) {
		t.Errorf("a PR body with no closing keyword is genuinely broken: want a pause, got paused=%v", paused)
	}
}

func TestHandleBrokenReviewLinkage_DefaultBaseBodyReadError_NoPause(t *testing.T) {
	client := brokenLinkageClient(func(_, _ string, _ int) ([]int, error) { return nil, errors.New("boom") })
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", LinkedPRNumber: 0}

	paused, pr := eng.handleBrokenReviewLinkage("owner", "repo", item)
	if paused || pr != 77 || pausedLabelAdded(client) {
		t.Errorf("an unreadable PR body must not pause (fail open, retry next poll): got paused=%v pr=%d", paused, pr)
	}
}

// #1962 R2 — backstop GitHub's Closes #N auto-close on the default base.

func awaitingCloseAdded(c *mockGitHubClient) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.addLabelCalls {
		if l.labelName == nonDefaultBaseAwaitingCloseLabel {
			return true
		}
	}
	return false
}

func TestGuardDefaultBaseAutoClose_MarksOnlyAnOpenIssue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		issue    *gh.IssueData
		err      error
		wantMark bool
	}{
		{"auto-close fired", &gh.IssueData{State: "closed"}, nil, false},
		{"auto-close did not fire", &gh.IssueData{State: "open"}, nil, true},
		{"state unreadable", nil, errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchIssueFn: func(_, _ string, _ int) (*gh.IssueData, error) { return tc.issue, tc.err },
			}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			item := gh.ProjectItem{Number: 20, Repo: "owner/repo"} // no base: label → default base

			eng.closeIssueIfNonDefaultBase(item, 55)

			if got := awaitingCloseAdded(client); got != tc.wantMark {
				t.Errorf("fabrik:awaiting-close added = %v, want %v", got, tc.wantMark)
			}
			client.mu.Lock()
			closes := len(client.closeIssueCalls)
			client.mu.Unlock()
			if closes != 0 {
				t.Errorf("the landing-time guard must not close directly (it would race GitHub's own auto-close); got %d CloseIssue call(s)", closes)
			}
		})
	}
}

func TestSettleDefaultBaseClose_ClosesAndSaysAutoCloseMissed(t *testing.T) {
	client := &mockGitHubClient{
		fetchIssueFn: func(_, _ string, n int) (*gh.IssueData, error) { return &gh.IssueData{Number: n, State: "open"}, nil },
		fetchLinkedPRFn: func(_, _ string, _ int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 55, State: "closed", Merged: true}, nil
		},
		addCommentFn: func(_, _ string, _ int, _ string) (int, error) { return 1, nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", Labels: []string{nonDefaultBaseAwaitingCloseLabel}}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("want 1 CloseIssue call, got %d", len(client.closeIssueCalls))
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("want 1 comment, got %d", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	for _, want := range []string{"PR #55", "auto-close did not fire", "Closes #20"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q: %s", want, body)
		}
	}
	removed := false
	for _, l := range client.removeLabelCalls {
		if l.labelName == nonDefaultBaseAwaitingCloseLabel {
			removed = true
		}
	}
	if !removed {
		t.Error("the awaiting-close marker must be cleared once the issue is closed")
	}
}

func TestSettleDefaultBaseClose_AlreadyClosed_ClearsSilently(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", IsClosed: true, Labels: []string{nonDefaultBaseAwaitingCloseLabel}}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 || len(client.addCommentCalls) != 0 {
		t.Errorf("GitHub's auto-close landed late: want no close and no comment, got %d close(s), %d comment(s)",
			len(client.closeIssueCalls), len(client.addCommentCalls))
	}
}

// Pruefer finding on #1965: the board snapshot can lag a late GitHub auto-close
// by a poll. The settle path must decide on the live state, never re-close an
// issue GitHub already closed, and never post a false "did not fire" comment.
func TestSettleDefaultBaseClose_StaleSnapshotLiveClosed_ClearsSilently(t *testing.T) {
	client := &mockGitHubClient{
		fetchIssueFn: func(_, _ string, n int) (*gh.IssueData, error) { return &gh.IssueData{Number: n, State: "closed"}, nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", IsClosed: false, Labels: []string{nonDefaultBaseAwaitingCloseLabel}}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 || len(client.addCommentCalls) != 0 {
		t.Errorf("live state is closed: want no close and no comment, got %d close(s), %d comment(s)",
			len(client.closeIssueCalls), len(client.addCommentCalls))
	}
	removed := false
	for _, l := range client.removeLabelCalls {
		if l.labelName == nonDefaultBaseAwaitingCloseLabel {
			removed = true
		}
	}
	if !removed {
		t.Error("the marker must be cleared once the live read shows the issue closed")
	}
}

func TestSettleDefaultBaseClose_LiveReadError_Defers(t *testing.T) {
	client := &mockGitHubClient{} // default FetchIssue returns an error
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", Labels: []string{nonDefaultBaseAwaitingCloseLabel}}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 || len(client.addCommentCalls) != 0 || len(client.removeLabelCalls) != 0 {
		t.Errorf("unreadable live state must defer (no close, no comment, marker kept): got %d close(s), %d comment(s), %d removal(s)",
			len(client.closeIssueCalls), len(client.addCommentCalls), len(client.removeLabelCalls))
	}
}

// Pruefer finding on #1965 (round 2): the live read applies on every base — a
// base:-labelled item (including one naming the default branch) whose issue a
// stale snapshot still shows open but which is live-closed must not be
// re-closed (CloseIssue would overwrite its state_reason).
func TestSettleClose_BaseLabelled_StaleSnapshotLiveClosed_NoReclose(t *testing.T) {
	client := &mockGitHubClient{
		fetchIssueFn: func(_, _ string, n int) (*gh.IssueData, error) { return &gh.IssueData{Number: n, State: "closed"}, nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 21, Repo: "owner/repo", IsClosed: false,
		Labels: []string{nonDefaultBaseAwaitingCloseLabel, "base:main"}}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 || len(client.addCommentCalls) != 0 {
		t.Errorf("live-closed issue must not be re-closed or commented on: got %d close(s), %d comment(s)",
			len(client.closeIssueCalls), len(client.addCommentCalls))
	}
}

// Pruefer round 4 on #1965: once R1 stops pausing a default-base item whose PR
// body confirms the linkage, the gate must evaluate the PR's REAL review state.
// The GraphQL review data (item.LinkedPRReviews/ReviewRequests) rides the same
// closing-keyword link and is empty when the link is missing, so it must come
// over REST — as it already does on a base:<branch> item.
func TestCheckReviewGate_DefaultBaseLinkMissing_UsesRESTReviews(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(_, _ string, _ int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, State: "open"}, nil
		},
		fetchPRClosingIssuesFn: func(_, _ string, _ int) ([]int, error) { return []int{10}, nil },
		fetchPRReviewsFn: func(_, _ string, _ int) ([]gh.PRReview, error) {
			return []gh.PRReview{{Author: "reviewer", State: "APPROVED", DatabaseID: 1}}, nil
		},
		fetchPRReviewRequestsFn: func(_, _ string, _ int) ([]gh.ReviewRequest, error) { return nil, nil },
	}
	stage := &stages.Stage{Name: "Validate", Order: 5, Prompt: "validate", WaitForReviews: boolPtr(true)}
	eng := testEngineWithStages(t, client, []*stages.Stage{stage})
	// Unlabelled (default base), link missing, GraphQL review data empty.
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo", Status: "Validate", LinkedPRNumber: 0,
		Labels: []string{"stage:Validate:complete"}}

	blocked, timedOut, terminated, reviews := eng.checkReviewGate(&gh.ProjectBoard{ProjectID: "PVT_1"}, item, stage)

	if terminated {
		t.Fatal("PR body confirms the linkage: the gate must not pause as broken linkage")
	}
	if blocked || timedOut {
		t.Errorf("an APPROVED review exists (over REST) and nothing is outstanding: want the gate clear, got blocked=%v timedOut=%v", blocked, timedOut)
	}
	if len(reviews) != 1 || reviews[0].Author != "reviewer" {
		t.Errorf("gate must evaluate the REST-sourced reviews, got %+v", reviews)
	}
}

// TestAttemptMergeOnValidate_LinkMissing_FeedbackReadErrorHolds pins the
// landing-decision side of round 4: a default-base item whose closing-keyword
// link is missing has its reviews resolved over REST by the feedback gate, and
// a failed PR resolution there holds the landing (deferred, no auto-merge)
// rather than trusting the empty GraphQL review data.
func TestAttemptMergeOnValidate_LinkMissing_FeedbackReadErrorHolds(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(_, _ string, _ int) (*gh.PRDetails, error) {
			return nil, errors.New("network error")
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, deferred, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil || enabled || !deferred {
		t.Fatalf("want held (deferred, no error, not enabled); got enabled=%v deferred=%v err=%v", enabled, deferred, err)
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("auto-merge must not be enabled while feedback state is unknown, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
}

// Pruefer round 5 on #1965: a live read that keeps failing (a persistent
// permission or 404 problem) must count toward the ADR-1097 retry budget and
// escalate, not leave a Done item with an open issue stalled behind a log line.
func TestSettleClose_PersistentLiveReadError_Escalates(t *testing.T) {
	client := &mockGitHubClient{} // default FetchIssue returns an error
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 2
	item := gh.ProjectItem{Number: 22, Repo: "owner/repo", Labels: []string{nonDefaultBaseAwaitingCloseLabel}}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleNonDefaultBaseClose(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("an unreadable issue must never be closed blind, got %d close(s)", len(client.closeIssueCalls))
	}
	paused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			paused = true
		}
	}
	if !paused {
		t.Error("expected fabrik:paused after MaxRetries unreadable settle passes")
	}
}
