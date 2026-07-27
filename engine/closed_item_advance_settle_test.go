package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// closedAdvanceStages returns a pipeline matching the standard shape used
// elsewhere in these tests: a Holding stage, a gate-checked Validate stage,
// and a cleanup (Done) stage, plus a couple of ordinary non-gate stages.
func closedAdvanceStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Implement", Order: 2},
		{Name: "Review", Order: 3},
		{Name: "Queued", Order: 4, HoldingStage: true},
		{Name: "Validate", Order: 5, WaitForCI: &tr},
		{Name: "Done", Order: 6, CleanupWorktree: true},
	}
}

func TestSettleClosedItemsToDone_ClosedAtEligibleStage_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Status: "Implement", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update call, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected advance to Done option, got %s", client.updateStatusCalls[0].optionID)
	}
}

func TestSettleClosedItemsToDone_ClosedAtUnconfiguredColumn_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	// "Backlog" has no matching entry in closedAdvanceStages(), so
	// stages.FindStage returns nil for it — this is the case a closed item
	// has no worktree and no stage bookkeeping to protect, so it must still
	// be advanced rather than skipped.
	item := gh.ProjectItem{Number: 9, ItemID: "PVTI_9", Repo: "owner/repo", Status: "Backlog", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected a closed item at an unconfigured column (Backlog) to be advanced, got %d status update calls", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected advance to Done option, got %s", client.updateStatusCalls[0].optionID)
	}
}

// TestSettleClosedItemsToDone_ClosedAtDeclaredUnmanagedStage_Advances covers
// the case stages.FindStage no longer returns nil for once a declarative
// `unmanaged: true` Backlog stage exists (issue #973). settleClosedItemsToDone's
// skip condition only excludes CleanupWorktree/HoldingStage/gate-checked
// stages, so an Unmanaged stage — which is none of those — must still fall
// through to advanceClosedItemToDone, exactly as it did when stage was nil.
// This guards against a future edit that adds `stage.Unmanaged` to the skip
// list by analogy with the dispatch guards elsewhere, which would silently
// strand closed Backlog items forever (no worktree, so nothing else reaps them).
func TestSettleClosedItemsToDone_ClosedAtDeclaredUnmanagedStage_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := append([]*stages.Stage{{Name: "Backlog", Order: -1, Unmanaged: true}}, closedAdvanceStages()...)
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 10, ItemID: "PVTI_10", Repo: "owner/repo", Status: "Backlog", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected a closed item at a declared unmanaged column (Backlog) to be advanced, got %d status update calls", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected advance to Done option, got %s", client.updateStatusCalls[0].optionID)
	}
}

func TestSettleClosedItemsToDone_AlreadyAtDone_Idempotent(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 2, ItemID: "PVTI_2", Repo: "owner/repo", Status: "Done", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update for an item already at Done, got %d", len(client.updateStatusCalls))
	}
}

// TestSettleClosedItemsToDone_ClosedAtHoldingStage_NoTrainWorker_Advances is
// the regression guard for issue #1072: a closed item stranded at a Holding
// stage (e.g. Queued) — however it got there — must be rescued to Done like
// any other stage, as long as no merge-train worker is actively assembling a
// batch for its repo. Before #1072 this scan blanket-skipped Holding stages,
// which is exactly why 17 items merged outside the train's own landing path
// stayed stranded in Queued forever.
func TestSettleClosedItemsToDone_ClosedAtHoldingStage_NoTrainWorker_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 3, ItemID: "PVTI_3", Repo: "owner/repo", Status: "Queued", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected a closed item at an idle Holding stage to be advanced, got %d status update calls", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected advance to Done option, got %s", client.updateStatusCalls[0].optionID)
	}
}

// TestSettleClosedItemsToDone_ClosedAtHoldingStage_TrainWorkerActive_Skipped
// guards the one real race #1072's backstop must avoid: an item can be closed
// without merging while it is still a live batch member mid-assembly or
// mid-bisection. While a merge-train worker is in flight for the item's repo,
// this scan must leave the item alone rather than yanking it out from under
// the worker.
func TestSettleClosedItemsToDone_ClosedAtHoldingStage_TrainWorkerActive_Skipped(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.mergeTrainInFlight.Store("owner/repo", &mergeTrainWorkerState{assembling: true})
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 3, ItemID: "PVTI_3", Repo: "owner/repo", Status: "Queued", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update for a closed item at a Holding stage with an active train worker, got %d", len(client.updateStatusCalls))
	}
}

func TestSettleClosedItemsToDone_ClosedAtGateCheckedStage_LeftToTerminalAdvance(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 4, ItemID: "PVTI_4", Repo: "owner/repo", Status: "Validate", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update for a closed item at a gate-checked stage (owned by runValidatePRTerminalAdvance), got %d", len(client.updateStatusCalls))
	}
}

func TestSettleClosedItemsToDone_OpenItem_Untouched(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Implement", IsClosed: false}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update for an open item, got %d", len(client.updateStatusCalls))
	}
}

func TestSettleClosedItemsToDone_IgnoresLabelState(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{
		Number: 6, ItemID: "PVTI_6", Repo: "owner/repo", Status: "Review", IsClosed: true,
		Labels: []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:blocked"},
	}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected the item to still be advanced despite in-flight labels, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestSettleClosedItemsToDone_NoStatusField_NoPanicNoCall(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	eng.statusField = nil
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 7, ItemID: "PVTI_7", Repo: "owner/repo", Status: "Implement", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update when statusField is nil, got %d", len(client.updateStatusCalls))
	}
}

func TestSettleClosedItemsToDone_NoCleanupStageConfigured_NoOp(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Implement", Order: 2},
	}
	eng := testEngineWithStages(t, client, stgs)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 8, ItemID: "PVTI_8", Repo: "owner/repo", Status: "Implement", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update when no CleanupWorktree stage is configured, got %d", len(client.updateStatusCalls))
	}
}

func TestCleanupStage_ReturnsLowestOrder(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Archived", Order: 99, CleanupWorktree: true},
		{Name: "Done", Order: 6, CleanupWorktree: true},
		{Name: "Implement", Order: 2},
	}}
	got := cleanupStage(cfg)
	if got == nil || got.Name != "Done" {
		t.Errorf("expected lowest-Order cleanup stage %q, got %+v", "Done", got)
	}
}

// TestCleanupStage_SkipsUnmanaged is the regression guard for a PR review
// finding on issue #973: a stage combining unmanaged: true with
// cleanup_worktree: true (e.g. an order: -1 Backlog stage, the lowest Order in
// the pipeline) must never be resolved as "the" cleanup stage — that would
// make settleClosedItemsToDone move every closed board item into Backlog
// instead of Done, with no self-heal since the guard at
// closed_item_advance_settle.go skips resolved CleanupWorktree stages.
func TestCleanupStage_SkipsUnmanaged(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Backlog", Order: -1, Unmanaged: true, CleanupWorktree: true},
		{Name: "Done", Order: 6, CleanupWorktree: true},
		{Name: "Implement", Order: 2},
	}}
	got := cleanupStage(cfg)
	if got == nil || got.Name != "Done" {
		t.Errorf("expected the non-unmanaged cleanup stage %q, got %+v (Backlog must be skipped despite its lower Order)", "Done", got)
	}
}

// TestCleanupStage_OnlyUnmanagedCleanupStage_ReturnsNil verifies the degrade
// path when the sole CleanupWorktree stage is also Unmanaged: cleanupStage
// returns nil (same as "no cleanup stage configured at all") rather than
// silently returning the unmanaged stage.
func TestCleanupStage_OnlyUnmanagedCleanupStage_ReturnsNil(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Backlog", Order: -1, Unmanaged: true, CleanupWorktree: true},
		{Name: "Implement", Order: 2},
	}}
	if got := cleanupStage(cfg); got != nil {
		t.Errorf("expected nil (no eligible cleanup stage), got %+v", got)
	}
}

// TestHoldingStage_SkipsUnmanaged mirrors TestCleanupStage_SkipsUnmanaged for
// holdingStage: a stage combining unmanaged: true with holding_stage: true
// must never be resolved as "the" holding stage (advanceToQueued would move
// items into a parking column instead of the real merge-train queue).
func TestHoldingStage_SkipsUnmanaged(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Backlog", Order: -1, Unmanaged: true, HoldingStage: true},
		{Name: "Queued", Order: 6, HoldingStage: true},
	}}
	got := holdingStage(cfg)
	if got == nil || got.Name != "Queued" {
		t.Errorf("expected the non-unmanaged holding stage %q, got %+v (Backlog must be skipped)", "Queued", got)
	}
}

// TestHoldingStage_OnlyUnmanagedHoldingStage_ReturnsNil verifies the degrade
// path when the sole HoldingStage stage is also Unmanaged: holdingStage
// returns nil rather than silently returning the unmanaged stage.
func TestHoldingStage_OnlyUnmanagedHoldingStage_ReturnsNil(t *testing.T) {
	cfg := Config{Stages: []*stages.Stage{
		{Name: "Backlog", Order: -1, Unmanaged: true, HoldingStage: true},
		{Name: "Implement", Order: 2},
	}}
	if got := holdingStage(cfg); got != nil {
		t.Errorf("expected nil (no eligible holding stage), got %+v", got)
	}
}
