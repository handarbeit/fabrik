package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// unresolvedThreadItem returns a Queued ProjectItem carrying one unresolved,
// current-head review-thread comment on its linked PR.
func unresolvedThreadItem(number int) gh.ProjectItem {
	return gh.ProjectItem{
		Number: number,
		ItemID: "PVTI_" + string(rune('0'+number)),
		Repo:   "owner/repo",
		Status: "Queued",
		LinkedPRReviewThreadComments: []gh.Comment{
			{ID: "PRRC_1", DatabaseID: 101, Author: "copilot", Body: "Please fix this.", ReviewThreadID: "RT_1"},
		},
	}
}

func TestSettleQueuedReviewFindings_DirectEject_NoWorkerActive(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{unresolvedThreadItem(1)},
	}

	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call (reroute off Queued), got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "opt-implement" {
		t.Errorf("expected reroute to Implement (stage preceding Queued), got optionID=%s", client.updateStatusCalls[0].optionID)
	}

	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 ejection comment, got %d", len(calls))
	}
	if !strings.Contains(calls[0].body, "has left the Queued column") {
		t.Errorf("expected the leaves-Queued ejection wording, got: %s", calls[0].body)
	}

	eng.mergeTrainEjectionsMu.Lock()
	count := eng.mergeTrainEjectionCounts["owner/repo#1"]
	eng.mergeTrainEjectionsMu.Unlock()
	if count != 1 {
		t.Errorf("expected MaxMergeTrainEjections counter to increment, got %d", count)
	}
}

func TestSettleQueuedReviewFindings_PendingFlag_WorkerActive(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.store.EnterRepoWorker("owner/repo")

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{unresolvedThreadItem(1)},
	}

	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update while a worker is in flight, got %d", len(client.updateStatusCalls))
	}
	client.mu.Lock()
	commentCount := len(client.addCommentCalls)
	client.mu.Unlock()
	if commentCount != 0 {
		t.Errorf("expected no ejection comment while a worker is in flight, got %d", commentCount)
	}

	count, ok := eng.takePendingReviewEject("owner/repo", 1)
	if !ok || count != 1 {
		t.Fatalf("expected a pending-eject signal with count=1, got (%d, %v)", count, ok)
	}
}

func TestSettleQueuedReviewFindings_ResolvedThreadMember_NoEject(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Queued"}, // no findings
		},
	}

	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update for a member with no findings, got %d", len(client.updateStatusCalls))
	}
}

func TestSettleQueuedReviewFindings_SkipsNativeMergeQueueMembers(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.cfg.MergeQueue = "on"

	item := unresolvedThreadItem(1)
	item.LinkedPRIsMergeQueueEnabled = true

	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected a native-merge-queue member to be skipped entirely, got %d status update(s)", len(client.updateStatusCalls))
	}
	if len(client.fetchItemDetailsCalls) != 0 {
		t.Errorf("expected a native-merge-queue member to be skipped before any deep-fetch, got %d fetch call(s)", len(client.fetchItemDetailsCalls))
	}
}

func TestSettleQueuedReviewFindings_SkipsAutoMergeEnabledMembers(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	item := unresolvedThreadItem(1)
	item.Labels = []string{"fabrik:auto-merge-enabled"}

	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{item}}
	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected a fabrik:auto-merge-enabled member to be skipped, got %d status update(s)", len(client.updateStatusCalls))
	}
}

func TestSettleQueuedReviewFindings_SkipsPausedAndClosedItems(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	paused := unresolvedThreadItem(1)
	paused.Labels = []string{"fabrik:paused"}
	closed := unresolvedThreadItem(2)
	closed.IsClosed = true

	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{paused, closed}}
	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected paused/closed members to be excluded by groupQueuedByRepo, got %d status update(s)", len(client.updateStatusCalls))
	}
}

func TestSettleQueuedReviewFindings_NoHoldingStageConfigured_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", MaxConcurrent: 1,
			Stages: []*stages.Stage{{Name: "Validate", Order: 1}},
		},
		client, claude, NewWorktreeManager(t.TempDir()),
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1", Items: []gh.ProjectItem{unresolvedThreadItem(1)}}
	eng.settleQueuedReviewFindings(board) // must not panic

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no-op with no holding stage configured, got %d status update(s)", len(client.updateStatusCalls))
	}
}
