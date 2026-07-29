package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestWorktreelessClosedItem_AdvancesLabelsAndArchives_EndToEnd exercises the
// full chain described in #1224: a closed issue with no worktree on disk
// (1) advances to Done via settleClosedItemsToDone (§6.11, ADR-064), (2) is
// admitted by both cleanup-stage admission gates and gets fully labelled by
// handleCleanupStage as if a normal dispatch had run, (3) becomes archive-
// eligible and is archived by settleArchiveDoneItems once its grace period
// elapses (§6.12, ADR-068), and (4) settles into a low-cost terminal state —
// re-admission is refused once labelled (FR-3) — rather than resuming to be
// deep-fetched on every subsequent poll.
//
// This test does not exercise the boardcache/probe cold-start path
// (isProbeOnlyTerminal / seedTerminalFromProbeItems) — that mechanism (Path B
// in the issue's Research) is explicitly out of scope for #1224; see the Plan
// stage comment's Key Decisions.
func TestWorktreelessClosedItem_AdvancesLabelsAndArchives_EndToEnd(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	const issueNum = 42
	item := gh.ProjectItem{
		Number:   issueNum,
		ItemID:   "PVTI_42",
		Repo:     "owner/repo",
		Status:   "Implement",
		IsClosed: true,
		// No worktree ever created for this item — it never went through
		// Implement in this engine instance; e.g. it was closed by a human
		// comment/duplicate-close before any stage ran.
	}
	board.Items = []gh.ProjectItem{item}

	// Step 1 (§6.11): settleClosedItemsToDone advances the closed, worktree-less
	// item straight to Done — no completion label applied by this scan itself.
	eng.settleClosedItemsToDone(board)
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call advancing to Done, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected advance to Done option, got %s", client.updateStatusCalls[0].optionID)
	}

	// Simulate the board refresh that would follow the status move.
	item.Status = "Done"
	board.Items = []gh.ProjectItem{item}
	doneStage := cleanupStage(eng.cfg)
	if doneStage == nil {
		t.Fatal("expected a resolved cleanup stage")
	}

	// Step 2: both cleanup-stage admission gates must admit this item — the
	// defect #1224 fixes. Before the fix, worktreeExistsForItem(item) was the
	// sole criterion and both gates returned false here, forever.
	if !eng.itemMayNeedWork(item) {
		t.Fatal("itemMayNeedWork should admit a closed, non-PR, worktree-less item at the cleanup stage")
	}
	if !eng.itemNeedsWork(item) {
		t.Fatal("itemNeedsWork should admit a closed, non-PR, worktree-less item at the cleanup stage")
	}

	// Dispatch would now call handleCleanupStage directly (worktree removal is
	// a no-op for a worktree that never existed; addLabelCalls/removeLabelCalls
	// below prove the rest of the bookkeeping still runs).
	eng.handleCleanupStage(item, doneStage, "owner/repo")

	var labelled bool
	for _, call := range client.addLabelCalls {
		if call.issueNumber == issueNum && call.labelName == "stage:Done:complete" {
			labelled = true
		}
	}
	if !labelled {
		t.Fatal("handleCleanupStage should have applied stage:Done:complete for the worktree-less item")
	}
	var extendTurnsRemoved bool
	for _, call := range client.removeLabelCalls {
		if call.issueNumber == issueNum && call.labelName == "fabrik:extend-turns" {
			extendTurnsRemoved = true
		}
	}
	if !extendTurnsRemoved {
		t.Error("handleCleanupStage should have removed fabrik:extend-turns")
	}

	// Simulate the board refresh reflecting the label handleCleanupStage applied.
	item.Labels = []string{"stage:Done:complete"}
	board.Items = []gh.ProjectItem{item}

	// Step 3 (§6.12): settleArchiveDoneItems keys only on column + completion
	// label — worktree-agnostic by construction — so it must pick up this item
	// exactly as it would a worktree-having one, once past its grace period.
	eng.cfg.ArchiveAfter = 24 * time.Hour
	appliedAt := time.Now().Add(-25 * time.Hour)
	client.fetchLabelAppliedAtFn = func(owner, repo string, issueNumber int, labelName string) (time.Time, error) {
		return appliedAt, nil
	}
	eng.settleArchiveDoneItems(board)
	if len(client.archiveProjectItemCalls) != 1 {
		t.Fatalf("expected 1 archive call, got %d", len(client.archiveProjectItemCalls))
	}
	if client.archiveProjectItemCalls[0].projectID != "PVT_1" || client.archiveProjectItemCalls[0].itemID != "PVTI_42" {
		t.Errorf("unexpected archive call: %+v", client.archiveProjectItemCalls[0])
	}

	// Step 4 (FR-3): once labelled, the item must not resume being re-admitted
	// on every poll — both gates settle into the same low-cost terminal refusal
	// a worktree-having item reaches once its worktree is reaped.
	if eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should refuse a labelled, worktree-less item — it must not be re-admitted every poll (FR-3)")
	}
	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should refuse a labelled, worktree-less item — it must not be re-admitted every poll (FR-3)")
	}
}
