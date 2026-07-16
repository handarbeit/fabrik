package engine

import (
	"context"
	"fmt"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// TestLandSingleton_MemberIssueCloseFailure_MarksOutstanding drives the exact scenario
// this issue exists to fix: the member-issue CloseIssue call in landSingleton fails, and
// the failure must be durably recorded (fabrik:awaiting-member-close) so a settle scan can
// retry it later — mirroring ADR-060's marker-before-nothing-else pattern, but scoped to
// this single call.
func TestLandSingleton_MemberIssueCloseFailure_MarksOutstanding(t *testing.T) {
	m := makeQueuedMember(7, 70, "Issue Seven")
	client := &mockGitHubClient{
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 900, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error {
			if n == m.item.Number {
				return fmt.Errorf("rate limited")
			}
			return nil // member PR (#70) closes fine
		},
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm}

	eng.landSingleton(context.Background(), state, p, m, "merge-train-singleton-1")

	client.mu.Lock()
	defer client.mu.Unlock()
	markerAdded := false
	for _, c := range client.addLabelCalls {
		if c.issueNumber == m.item.Number && c.labelName == mergeTrainAwaitingMemberCloseLabel {
			markerAdded = true
		}
	}
	if !markerAdded {
		t.Errorf("expected %s added on member-issue close failure, got labels: %v", mergeTrainAwaitingMemberCloseLabel, client.addLabelCalls)
	}
	// The member PR (#70) close must still have been attempted — this issue's fix must
	// not disturb the sibling member-PR close three lines above it.
	prClosed := false
	for _, c := range client.closeIssueCalls {
		if c.issueNumber == m.prNum {
			prClosed = true
		}
	}
	if !prClosed {
		t.Errorf("expected member PR #%d closed regardless of member-issue close outcome", m.prNum)
	}
}

// TestLandSingleton_MemberIssueCloseSuccess_NoMarker verifies the common (success) path is
// untouched: no marker is written when CloseIssue succeeds.
func TestLandSingleton_MemberIssueCloseSuccess_NoMarker(t *testing.T) {
	m := makeQueuedMember(8, 80, "Issue Eight")
	client := &mockGitHubClient{
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 900, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{projectID: "PVT_test"}
	p := trialParams{owner: "owner", repo: "repo", baseBranch: "main", wm: wm}

	eng.landSingleton(context.Background(), state, p, m, "merge-train-singleton-1")

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == mergeTrainAwaitingMemberCloseLabel {
			t.Errorf("did not expect %s to be added when CloseIssue succeeds", mergeTrainAwaitingMemberCloseLabel)
		}
	}
}

// TestSettleMergeTrainMemberClose_AlreadyClosed_SkipsCloseAndClearsMarker covers the
// idempotency requirement: if the issue is already closed (e.g. GitHub's own Closes #N
// auto-close finally landed), the settle pass must skip the redundant CloseIssue call and
// just clear the marker.
func TestSettleMergeTrainMemberClose_AlreadyClosed_SkipsCloseAndClearsMarker(t *testing.T) {
	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	item := gh.ProjectItem{
		Number: 9, Repo: "owner/repo", IsClosed: true,
		Labels: []string{mergeTrainAwaitingMemberCloseLabel},
	}

	eng.settleMergeTrainMemberClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for an already-closed issue, got %v", client.closeIssueCalls)
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == mergeTrainAwaitingMemberCloseLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected marker removed once the issue is confirmed closed")
	}
}

// TestSettleMergeTrainMemberClose_RetrySucceeds verifies the retry path: CloseIssue
// succeeds on this pass, the marker is cleared, and the retry counter resets.
func TestSettleMergeTrainMemberClose_RetrySucceeds(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	item := gh.ProjectItem{
		Number: 10, Repo: "owner/repo", IsClosed: false,
		Labels: []string{mergeTrainAwaitingMemberCloseLabel},
	}

	eng.settleMergeTrainMemberClose(item)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 1 || client.closeIssueCalls[0].issueNumber != 10 {
		t.Errorf("expected CloseIssue called once for #10, got %v", client.closeIssueCalls)
	}
	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == mergeTrainAwaitingMemberCloseLabel {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected marker removed after a successful retry close")
	}
}

// TestSettleMergeTrainMemberCloses_SkipsPausedItems mirrors the no-work-needed settle
// scan's own paused-item guard: an operator investigating a paused item must not be
// fought by this scan.
func TestSettleMergeTrainMemberCloses_SkipsPausedItems(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return nil },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))

	board := &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				Number: 11, Repo: "owner/repo",
				Labels: []string{mergeTrainAwaitingMemberCloseLabel, "fabrik:paused"},
			},
		},
	}

	eng.settleMergeTrainMemberCloses(board)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue call for a paused item, got %v", client.closeIssueCalls)
	}
}

// TestRecordMergeTrainMemberCloseRetry_EscalatesAtMaxRetries mirrors
// TestRecordNoWorkNeededRetry_EscalatesAtMaxRetries: repeated settle failures must
// eventually pause the issue, remove the marker, and post an explanatory comment.
func TestRecordMergeTrainMemberCloseRetry_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return fmt.Errorf("rate limited") },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 2

	item := gh.ProjectItem{
		Number: 12, Repo: "owner/repo", IsClosed: false,
		Labels: []string{mergeTrainAwaitingMemberCloseLabel},
	}

	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleMergeTrainMemberClose(item)
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
		if c.labelName == mergeTrainAwaitingMemberCloseLabel {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures")
	}
	if !markerRemoved {
		t.Errorf("expected %s to be removed on escalation", mergeTrainAwaitingMemberCloseLabel)
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected an explanatory escalation comment to be posted")
	}
}

// TestRecordMergeTrainMemberCloseRetry_UnlimitedWhenMaxRetriesZero mirrors the
// no-work-needed counter's documented behavior: MaxRetries == 0 means unlimited retries,
// never escalate.
func TestRecordMergeTrainMemberCloseRetry_UnlimitedWhenMaxRetriesZero(t *testing.T) {
	client := &mockGitHubClient{
		closeIssueFn: func(owner, repo string, n int) error { return fmt.Errorf("rate limited") },
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.cfg.MaxRetries = 0

	item := gh.ProjectItem{
		Number: 13, Repo: "owner/repo", IsClosed: false,
		Labels: []string{mergeTrainAwaitingMemberCloseLabel},
	}

	for i := 0; i < 10; i++ {
		eng.settleMergeTrainMemberClose(item)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("did not expect escalation (fabrik:paused) when MaxRetries == 0")
		}
	}
}

// TestLandMergeTrainBatch_MemberIssueCloseFailure_DoesNotMarkOutstanding is the explicit
// scope guard from the issue: landMergeTrainBatch has the identical unretried
// member-issue CloseIssue call, but it is out of scope for this fix and must not gain the
// new marker as an accidental side effect of a shared-helper refactor.
func TestLandMergeTrainBatch_MemberIssueCloseFailure_DoesNotMarkOutstanding(t *testing.T) {
	survivors := []trainMember{makeQueuedMember(20, 200, "Issue Twenty")}
	client := &mockGitHubClient{
		listPRsFn:  func(owner, repo string) ([]gh.PRDetails, error) { return nil, nil },
		createPRFn: func(owner, repo, title, head, base, body string) (int, error) { return 300, nil },
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			tr := true
			return &tr, "clean", nil
		},
		mergePRFn:    func(owner, repo string, prNumber int) error { return nil },
		addCommentFn: func(owner, repo string, n int, body string) (int, error) { return 1, nil },
		closeIssueFn: func(owner, repo string, n int) error {
			if n == 20 {
				return fmt.Errorf("rate limited")
			}
			return nil
		},
	}
	wm := NewWorktreeManager(t.TempDir())
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	state := &mergeTrainWorkerState{trialName: "merge-train-main-1", projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store("owner/repo", state)

	eng.landMergeTrainBatch(context.Background(), state, "owner", "repo", "main", survivors, wm)

	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == mergeTrainAwaitingMemberCloseLabel {
			t.Error("landMergeTrainBatch's member-issue close is out of scope for this fix and must not gain the new marker")
		}
	}
}
