package engine

import (
	"errors"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestSettlePRReady_NoLinkedPR_ClearsMarker covers the case where the marker is
// stale (no PR found at all — e.g. the PR was deleted, or the label survived a
// scenario that no longer applies): the settle pass must skip MarkPRReady and
// just clear the marker.
func TestSettlePRReady_NoLinkedPR_ClearsMarker(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 20, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no MarkPRReady call when no PR is found, got %v", client.markPRReadyCalls)
	}
	if !markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("expected marker removed when no PR is found")
	}
}

// TestSettlePRReady_PRClosed_ClearsMarker covers a PR closed without merging
// (e.g. the issue was abandoned) — nothing left to mark ready.
func TestSettlePRReady_PRClosed_ClearsMarker(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 30, State: "closed", Draft: true}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 21, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no MarkPRReady call for a closed PR, got %v", client.markPRReadyCalls)
	}
	if !markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("expected marker removed for a closed PR")
	}
}

// TestSettlePRReady_PRMerged_ClearsMarker covers a PR that merged despite
// still carrying the marker (e.g. the merge-train landed it via a path that
// never called markPRReady).
func TestSettlePRReady_PRMerged_ClearsMarker(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 31, State: "closed", Merged: true}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 22, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no MarkPRReady call for a merged PR, got %v", client.markPRReadyCalls)
	}
	if !markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("expected marker removed for a merged PR")
	}
}

// TestSettlePRReady_AlreadyNotDraft_ClearsMarkerWithoutCall covers self-healing:
// a later stage's own markPRReady call already succeeded, or a human clicked
// "Ready for review" manually. The settle scan must not call MarkPRReady again.
func TestSettlePRReady_AlreadyNotDraft_ClearsMarkerWithoutCall(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 32, State: "open", Draft: false}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 23, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no redundant MarkPRReady call for an already-ready PR, got %v", client.markPRReadyCalls)
	}
	if !markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("expected marker removed for an already-ready PR")
	}
}

// TestSettlePRReady_RetrySucceeds verifies the core recovery path: the PR is
// still draft and open, MarkPRReady succeeds on this pass, and the marker is
// cleared.
func TestSettlePRReady_RetrySucceeds(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 33, State: "open", Draft: true}, nil
		},
		markPRReadyFn: func(owner, repo string, prNumber int) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	item := gh.ProjectItem{Number: 24, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 1 || client.markPRReadyCalls[0].prNumber != 33 {
		t.Errorf("expected MarkPRReady called once for PR #33, got %v", client.markPRReadyCalls)
	}
	if !markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("expected marker removed after a successful retry")
	}
}

// TestSettlePRReady_RetryFails_MarkerStays leaves the marker in place and
// increments the retry counter — verified indirectly via
// TestRecordPRReadyRetry_EscalatesAtMaxRetries.
func TestSettlePRReady_RetryFails_MarkerStays(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 34, State: "open", Draft: true}, nil
		},
		markPRReadyFn: func(owner, repo string, prNumber int) error { return errors.New("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 5

	item := gh.ProjectItem{Number: 25, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 1 {
		t.Errorf("expected MarkPRReady attempted once, got %v", client.markPRReadyCalls)
	}
	if markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("did not expect marker removed after a single failed retry")
	}
}

// TestSettlePRReady_FetchLinkedPRError_RetriesWithoutClearing covers an API
// error resolving the PR: the settle pass must not clear the marker (it can't
// tell whether the PR is still draft) and must retry on a later poll.
func TestSettlePRReady_FetchLinkedPRError_RetriesWithoutClearing(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errors.New("api error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 5

	item := gh.ProjectItem{Number: 26, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	eng.settlePRReady(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no MarkPRReady call when the PR lookup itself errors, got %v", client.markPRReadyCalls)
	}
	if markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Error("did not expect marker removed when the PR lookup errors")
	}
}

// TestSettlePRReadyScan_SkipsPausedItems mirrors settleNonDefaultBaseCloses's own
// paused-item guard: an operator investigating a paused item must not be fought
// by this scan.
func TestSettlePRReadyScan_SkipsPausedItems(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 40, State: "open", Draft: true}, nil
		},
		markPRReadyFn: func(owner, repo string, prNumber int) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 27, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel, "fabrik:paused"}},
		},
	}

	eng.settlePRReadyScan(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no MarkPRReady call for a paused item, got %v", client.markPRReadyCalls)
	}
}

// TestSettlePRReadyScan_SkipsItemsWithoutMarker verifies the scan only acts on
// items carrying the durable marker.
func TestSettlePRReadyScan_SkipsItemsWithoutMarker(t *testing.T) {
	client := &mockGitHubClient{
		markPRReadyFn: func(owner, repo string, prNumber int) error { return nil },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{Number: 28, Repo: "owner/repo", Labels: []string{"stage:Implement:complete"}},
		},
	}

	eng.settlePRReadyScan(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.markPRReadyCalls) != 0 {
		t.Errorf("expected no MarkPRReady call for an item without the marker, got %v", client.markPRReadyCalls)
	}
}

// TestRecordPRReadyRetry_EscalatesAtMaxRetries mirrors
// TestRecordNonDefaultBaseCloseRetry_EscalatesAtMaxRetries: repeated settle
// failures must eventually pause the issue, remove the marker, and post an
// explanatory comment naming the draft PR.
func TestRecordPRReadyRetry_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 55, State: "open", Draft: true}, nil
		},
		markPRReadyFn: func(owner, repo string, prNumber int) error { return errors.New("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 2

	item := gh.ProjectItem{
		Number: 29, Repo: "owner/repo", LinkedPRNumber: 55,
		Labels: []string{prReadyAwaitingLabel},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settlePRReady(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	pausedAdded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures")
	}
	if !markerRemovedIn(client.removeLabelCalls, prReadyAwaitingLabel) {
		t.Errorf("expected %s to be removed on escalation", prReadyAwaitingLabel)
	}
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected an explanatory escalation comment to be posted")
	}
	found := false
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 29 && strings.Contains(c.body, "#55") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected escalation comment to name PR #55, got: %v", client.addCommentCalls)
	}
}

// TestRecordPRReadyRetry_UnlimitedWhenMaxRetriesZero mirrors
// TestRecordNonDefaultBaseCloseRetry_UnlimitedWhenMaxRetriesZero: MaxRetries == 0
// means unlimited retries, never escalate.
func TestRecordPRReadyRetry_UnlimitedWhenMaxRetriesZero(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 56, State: "open", Draft: true}, nil
		},
		markPRReadyFn: func(owner, repo string, prNumber int) error { return errors.New("rate limited") },
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.MaxRetries = 0

	item := gh.ProjectItem{Number: 30, Repo: "owner/repo", Labels: []string{prReadyAwaitingLabel}}

	for i := 0; i < 10; i++ {
		eng.settlePRReady(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("did not expect escalation (fabrik:paused) when MaxRetries == 0")
		}
	}
}

// markerRemovedIn reports whether label appears among the removeLabelCalls.
func markerRemovedIn(calls []removeLabelCall, label string) bool {
	for _, c := range calls {
		if c.labelName == label {
			return true
		}
	}
	return false
}
