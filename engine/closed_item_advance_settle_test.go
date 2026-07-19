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

func TestSettleClosedItemsToDone_ClosedAtHoldingStage_Skipped(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, closedAdvanceStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	item := gh.ProjectItem{Number: 3, ItemID: "PVTI_3", Repo: "owner/repo", Status: "Queued", IsClosed: true}
	board.Items = []gh.ProjectItem{item}

	eng.settleClosedItemsToDone(board)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update for a closed item at a Holding stage, got %d", len(client.updateStatusCalls))
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
