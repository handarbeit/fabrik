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
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", defaultPartitionBase))
	// #1 must be part of the live worker's dispatched batch — mergeTrainBatchMembers
	// is what settleQueuedReviewFindings now consults to route to the pending-signal
	// path, not repo-level activity alone (see the batch-cap overflow test below).
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", defaultPartitionBase), &mergeTrainWorkerState{
		projectID:    "PVT_1",
		batchNumbers: map[int]bool{1: true},
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{unresolvedThreadItem(1)},
	}

	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update while #1 is owned by the live batch, got %d", len(client.updateStatusCalls))
	}
	client.mu.Lock()
	commentCount := len(client.addCommentCalls)
	client.mu.Unlock()
	if commentCount != 0 {
		t.Errorf("expected no ejection comment while #1 is owned by the live batch, got %d", commentCount)
	}

	count, ok := eng.takePendingReviewEject("owner/repo", 1)
	if !ok || count != 1 {
		t.Fatalf("expected a pending-eject signal with count=1, got (%d, %v)", count, ok)
	}
}

// TestSettleQueuedReviewFindings_DirectEject_WorkerActiveButMemberBeyondBatchCap
// closes the gap Pruefer flagged in review (#1208): a worker is dispatched with its
// batch already truncated to effectiveMaxBatchSize (capBatch, FR-4), so a Queued
// member beyond that cap is never part of any worker's in-memory batch. Gating
// direct-eject vs. pending-signal on repo-level worker activity alone would leave
// such a member's pending-eject signal permanently unconsumed for as long as the
// worker stays in flight — reproducing the very blackout this settle scan exists to
// close, just for members outside the front of the queue. #2 here is Queued, has an
// unresolved finding, and a worker IS active for the repo, but #2 is not in that
// worker's dispatched batch (only #1 is) — it must be ejected directly, immediately.
func TestSettleQueuedReviewFindings_DirectEject_WorkerActiveButMemberBeyondBatchCap(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", defaultPartitionBase))
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", defaultPartitionBase), &mergeTrainWorkerState{
		projectID:    "PVT_1",
		batchNumbers: map[int]bool{1: true}, // #2 is excluded — beyond the batch cap
	})

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{unresolvedThreadItem(2)},
	}

	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected #2 (beyond the batch cap) to be rerouted immediately despite a worker being active for the repo, got %d status update(s)", len(client.updateStatusCalls))
	}
	client.mu.Lock()
	calls := client.addCommentCalls
	client.mu.Unlock()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "has left the Queued column") {
		t.Fatalf("expected #2 to be directly ejected with the leaves-Queued wording, got: %+v", calls)
	}

	// No pending signal should have been recorded for #2 — it was ejected directly.
	if _, ok := eng.takePendingReviewEject("owner/repo", 2); ok {
		t.Error("expected no pending-eject signal for #2 — it was ejected directly, not flagged")
	}
}

// TestSettleQueuedReviewFindings_DirectEject_NoWorkerRegisteredForRepo confirms the
// pre-existing "no worker at all" direct-eject path still works now that the
// worker-active check has been replaced by a batch-membership lookup: a repo with no
// entry in mergeTrainInFlight (RepoWorkerActive also false) must still eject
// directly, not treat the absent membership map as "excluded from a live batch that
// doesn't actually exist" in some way that changes behavior.
func TestSettleQueuedReviewFindings_DirectEject_NoWorkerRegisteredForRepo(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := trainTestEngine(t, client, claude, NewWorktreeManager(t.TempDir()))

	board := &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items:     []gh.ProjectItem{unresolvedThreadItem(1)},
	}

	eng.settleQueuedReviewFindings(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected direct eject with no worker registered for the repo, got %d status update(s)", len(client.updateStatusCalls))
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
