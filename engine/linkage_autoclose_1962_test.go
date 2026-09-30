package engine

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
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
