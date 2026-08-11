package engine

import (
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestSettleRunawayGuardAlert_RetrySucceeds verifies the retry path: the alert comment
// succeeds on this pass, the marker is cleared, and the member is recorded as alerted so a
// subsequent fireRunawayGuard call for the same member (still within the same episode) skips
// it rather than double-posting.
func TestSettleRunawayGuardAlert_RetrySucceeds(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 10, Repo: "owner/repo",
		Labels: []string{runawayAlertMarkerLabel, "fabrik:paused", "fabrik:awaiting-input"},
	}

	eng.settleRunawayGuardAlert(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	hasAlert := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 10 && strings.Contains(c.body, "runaway guard") {
			hasAlert = true
		}
	}
	if !hasAlert {
		t.Error("expected the retry to post the runaway guard alert comment")
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == runawayAlertMarkerLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected the awaiting-runaway-alert marker removed after a successful retry")
	}

	eng.mergeTrainRunawayMu.Lock()
	alerted := eng.mergeTrainRunawayAlerted["owner/repo#10"]
	eng.mergeTrainRunawayMu.Unlock()
	if !alerted {
		t.Error("expected the member recorded as alerted after a successful retry")
	}
}

// TestSettleRunawayGuardAlert_RetryFails_MarkerStays leaves the marker in place and does not
// record the member as alerted — verified indirectly via
// TestEscalateRunawayAlertFailure_PostsFallbackCommentAtMaxRetries.
func TestSettleRunawayGuardAlert_RetryFails_MarkerStays(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, fmt.Errorf("rate limited")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 5

	item := gh.ProjectItem{
		Number: 11, Repo: "owner/repo",
		Labels: []string{runawayAlertMarkerLabel, "fabrik:paused"},
	}

	eng.settleRunawayGuardAlert(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.removeLabelCalls {
		if c.labelName == runawayAlertMarkerLabel {
			t.Error("did not expect the marker removed after a single failed retry")
		}
	}

	eng.mergeTrainRunawayMu.Lock()
	alerted := eng.mergeTrainRunawayAlerted["owner/repo#11"]
	eng.mergeTrainRunawayMu.Unlock()
	if alerted {
		t.Error("did not expect the member recorded as alerted after a failed retry")
	}
}

// TestSettleRunawayGuardAlertScan_SkipsItemsWithoutMarker verifies the scan only acts on
// items carrying the durable marker.
func TestSettleRunawayGuardAlertScan_SkipsItemsWithoutMarker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 12, Repo: "owner/repo", Labels: []string{"fabrik:paused"}},
		},
	}

	eng.settleRunawayGuardAlertScan(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment for an item without the marker, got %v", client.addCommentCalls)
	}
}

// TestSettleRunawayGuardAlertScan_DoesNotSkipPausedItems is a deliberate divergence from this
// repo's other settle scans (settleMergeTrainMemberCloses, settleNonDefaultBaseCloses): those
// skip fabrik:paused items because a paused item there means "already escalated, stop
// retrying." Here fabrik:paused is applied unconditionally by fireRunawayGuard from the very
// first application of the marker — the pause always co-occurs with the marker — so gating on
// fabrik:paused would make this scan a permanent no-op. The marker's own presence/absence is
// the only correct retry-eligibility signal.
func TestSettleRunawayGuardAlertScan_DoesNotSkipPausedItems(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 13, Repo: "owner/repo",
				Labels: []string{runawayAlertMarkerLabel, "fabrik:paused", "fabrik:awaiting-input"},
			},
		},
	}

	eng.settleRunawayGuardAlertScan(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	hasAlert := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 13 && strings.Contains(c.body, "runaway guard") {
			hasAlert = true
		}
	}
	if !hasAlert {
		t.Error("expected the scan to retry the alert for a paused item carrying the marker")
	}
}

// TestEscalateRunawayAlertFailure_PostsFallbackCommentAtMaxRetries verifies R1's terminal
// fallback: after MaxRetries failed settle passes, a fallback comment carrying the original
// alert's explanation is posted so the member is never left paused with zero delivered
// explanation, and the marker is removed so the scan stops retrying.
func TestEscalateRunawayAlertFailure_PostsFallbackCommentAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, fmt.Errorf("rate limited")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 2

	item := gh.ProjectItem{
		Number: 14, Repo: "owner/repo",
		Labels: []string{runawayAlertMarkerLabel, "fabrik:paused"},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleRunawayGuardAlert(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == runawayAlertMarkerLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Errorf("expected %s removed on escalation", runawayAlertMarkerLabel)
	}

	fallbackFound := false
	for _, c := range client.addCommentCalls {
		// addCommentFn above always errors — the fallback path goes through
		// postItemComment, which also hits the same injected error, so assert on the
		// attempt rather than a successfully-recorded post. The fallback body is built
		// and passed to postItemComment regardless of whether the underlying AddComment
		// call itself succeeds this time.
		if c.issueNumber == 14 && strings.Contains(c.body, "runaway guard alert delivery failed") {
			fallbackFound = true
		}
	}
	if !fallbackFound {
		t.Error("expected a fallback comment attempt naming the delivery failure")
	}
}
