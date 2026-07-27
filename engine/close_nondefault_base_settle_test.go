package engine

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestSettleNonDefaultBaseClose_AlreadyClosed_SkipsCloseAndClearsMarker covers the
// idempotency requirement: if the issue is already closed (e.g. GitHub's own Closes #N
// auto-close finally landed, or a human closed it manually), the settle pass must skip the
// redundant CloseIssue call and just clear the marker.
func TestSettleNonDefaultBaseClose_AlreadyClosed_SkipsCloseAndClearsMarker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 9, Repo: "owner/repo", IsClosed: true,
		Labels: []string{nonDefaultBaseAwaitingCloseLabel},
	}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for an already-closed issue, got %v", client.closeIssueCalls)
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == nonDefaultBaseAwaitingCloseLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected marker removed once the issue is confirmed closed")
	}
}

// TestSettleNonDefaultBaseClose_RetrySucceeds verifies the retry path: CloseIssue succeeds
// on this pass, the marker is cleared, and the retry counter resets.
func TestSettleNonDefaultBaseClose_RetrySucceeds(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 10, Repo: "owner/repo", IsClosed: false,
		Labels: []string{nonDefaultBaseAwaitingCloseLabel},
	}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 10 {
		t.Errorf("expected CloseIssue called once for #10, got %v", client.closeIssueCalls)
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == nonDefaultBaseAwaitingCloseLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected marker removed after a successful retry close")
	}
}

// TestSettleNonDefaultBaseClose_RetryFails leaves the marker in place and increments the
// retry counter — verified indirectly via TestRecordNonDefaultBaseCloseRetry_EscalatesAtMaxRetries.
func TestSettleNonDefaultBaseClose_RetryFails_MarkerStays(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return fmt.Errorf("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 5

	item := gh.ProjectItem{
		Number: 11, Repo: "owner/repo", IsClosed: false,
		Labels: []string{nonDefaultBaseAwaitingCloseLabel},
	}

	eng.settleNonDefaultBaseClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 1 {
		t.Errorf("expected CloseIssue attempted once, got %v", client.closeIssueCalls)
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == nonDefaultBaseAwaitingCloseLabel {
			t.Error("did not expect marker removed after a single failed retry")
		}
	}
}

// TestSettleNonDefaultBaseCloses_SkipsPausedItems mirrors the merge-train member-close
// settle scan's own paused-item guard: an operator investigating a paused item must not be
// fought by this scan.
func TestSettleNonDefaultBaseCloses_SkipsPausedItems(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 11, Repo: "owner/repo",
				Labels: []string{nonDefaultBaseAwaitingCloseLabel, "fabrik:paused"},
			},
		},
	}

	eng.settleNonDefaultBaseCloses(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for a paused item, got %v", client.closeIssueCalls)
	}
}

// TestSettleNonDefaultBaseCloses_SkipsItemsWithoutMarker verifies the scan only acts on
// items carrying the durable marker.
func TestSettleNonDefaultBaseCloses_SkipsItemsWithoutMarker(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 12, Repo: "owner/repo", Labels: []string{"stage:Validate:complete"}},
		},
	}

	eng.settleNonDefaultBaseCloses(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for an item without the marker, got %v", client.closeIssueCalls)
	}
}

// TestRecordNonDefaultBaseCloseRetry_EscalatesAtMaxRetries mirrors
// TestRecordMergeTrainMemberCloseRetry_EscalatesAtMaxRetries: repeated settle failures must
// eventually pause the issue, remove the marker, and post an explanatory comment naming the
// merged PR.
func TestRecordNonDefaultBaseCloseRetry_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return fmt.Errorf("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 2

	item := gh.ProjectItem{
		Number: 12, Repo: "owner/repo", IsClosed: false, LinkedPRNumber: 55,
		Labels: []string{nonDefaultBaseAwaitingCloseLabel},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleNonDefaultBaseClose(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == nonDefaultBaseAwaitingCloseLabel {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures")
	}
	if !markerRemoved {
		t.Errorf("expected %s to be removed on escalation", nonDefaultBaseAwaitingCloseLabel)
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an explanatory escalation comment to be posted")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 12 && strings.Contains(c.body, "#55") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected escalation comment to name the merged PR #55, got: %v", client.addCommentCalls)
	}
}

// TestRecordNonDefaultBaseCloseRetry_UnlimitedWhenMaxRetriesZero mirrors the merge-train
// member-close counter's documented behavior: MaxRetries == 0 means unlimited retries,
// never escalate.
func TestRecordNonDefaultBaseCloseRetry_UnlimitedWhenMaxRetriesZero(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return fmt.Errorf("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 0

	item := gh.ProjectItem{
		Number: 13, Repo: "owner/repo", IsClosed: false,
		Labels: []string{nonDefaultBaseAwaitingCloseLabel},
	}

	for i := 0; i < 10; i++ {
		eng.settleNonDefaultBaseClose(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("did not expect escalation (fabrik:paused) when MaxRetries == 0")
		}
	}
}
