package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// pausedRecoveryStages returns a pipeline with both a wait_for_reviews stage
// (Review) and a wait_for_ci stage (Validate), plus a cleanup terminal (Done).
func pausedRecoveryStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1},
		{Name: "Review", Order: 2, WaitForReviews: &tr},
		{Name: "Validate", Order: 3, WaitForCI: &tr},
		{Name: "Done", Order: 4, CleanupWorktree: true},
	}
}

// pausedRecoveryCIOnlyStages returns a pipeline with only a wait_for_ci Validate
// stage (no wait_for_reviews stage), for regression testing of SC-2.
func pausedRecoveryCIOnlyStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1},
		{Name: "Validate", Order: 2, WaitForCI: &tr},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// addedLabelNames returns all label names passed to AddLabelToIssue across all calls.
func addedLabelNames(calls []addLabelCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.labelName)
	}
	return out
}

// removedLabelNames returns all label names passed to RemoveLabelFromIssue across all calls.
func removedLabelNames(calls []removeLabelCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.labelName)
	}
	return out
}

// TestPausedRecovery_WaitForReviews_AddsCompleteLabel verifies SC-1: a paused
// item with fabrik:awaiting-review and a missing stage:Review:complete gets the
// completion label added when the linked PR is found to be merged.
func TestPausedRecovery_WaitForReviews_AddsCompleteLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 7, Merged: true}, nil
		},
	}
	stgs := pausedRecoveryStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 42,
		ItemID: "PVTI_42",
		Status: "Review",
		Labels: []string{
			"fabrik:paused",
			"fabrik:awaiting-review",
			"stage:Implement:complete",
		},
	}

	advancedItems := make(map[string]bool)
	eng.runPausedItemMergedPRRecovery(board, []gh.ProjectItem{item}, advancedItems)

	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, "stage:Review:complete") {
		t.Errorf("expected stage:Review:complete to be added, got %v", added)
	}

	// The item should be marked as advanced so the cooldown defer skips it.
	if !advancedItems["owner/repo#42"] {
		t.Error("expected item to be marked as advanced")
	}

	removed := removedLabelNames(client.removeLabelCalls)
	if !containsLabel(removed, "fabrik:awaiting-review") {
		t.Errorf("expected fabrik:awaiting-review to be removed, got %v", removed)
	}
	if !containsLabel(removed, "fabrik:paused") {
		t.Errorf("expected fabrik:paused to be removed, got %v", removed)
	}

	// Board column should have advanced.
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected advanceToNextStage to call UpdateProjectItemStatus")
	}
}

// TestPausedRecovery_WaitForCIOnly_ExistingBehaviorPreserved verifies SC-2:
// a pipeline with only a wait_for_ci Validate stage still gets
// stage:Validate:complete added, preserving the original CI-only behavior.
func TestPausedRecovery_WaitForCIOnly_ExistingBehaviorPreserved(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 5, Merged: true}, nil
		},
	}
	stgs := pausedRecoveryCIOnlyStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		ItemID: "PVTI_10",
		Status: "Validate",
		Labels: []string{
			"fabrik:paused",
			"fabrik:awaiting-ci",
			"stage:Implement:complete",
		},
	}

	advancedItems := make(map[string]bool)
	eng.runPausedItemMergedPRRecovery(board, []gh.ProjectItem{item}, advancedItems)

	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("expected stage:Validate:complete to be added, got %v", added)
	}
	if !advancedItems["owner/repo#10"] {
		t.Error("expected item to be marked as advanced")
	}
}

// TestPausedRecovery_BothGates_BothCompleteLabelsAdded verifies SC-3: a paused
// item that straddles both a wait_for_reviews Review stage and a wait_for_ci
// Validate stage gets both completion labels added when the PR is merged.
func TestPausedRecovery_BothGates_BothCompleteLabelsAdded(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 9, Merged: true}, nil
		},
	}
	stgs := pausedRecoveryStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 99,
		ItemID: "PVTI_99",
		Status: "Review",
		Labels: []string{
			"fabrik:paused",
			"fabrik:awaiting-review",
			"stage:Implement:complete",
		},
	}

	advancedItems := make(map[string]bool)
	eng.runPausedItemMergedPRRecovery(board, []gh.ProjectItem{item}, advancedItems)

	added := addedLabelNames(client.addLabelCalls)
	if !containsLabel(added, "stage:Review:complete") {
		t.Errorf("expected stage:Review:complete to be added, got %v", added)
	}
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("expected stage:Validate:complete to be added, got %v", added)
	}
	if !advancedItems["owner/repo#99"] {
		t.Error("expected item to be marked as advanced")
	}
}

// TestPausedRecovery_ReviewCompleteAlreadyPresent_NoDuplicateAdd verifies SC-4:
// when stage:Review:complete is already present on the item, the recovery loop
// does not add it a second time (EC-2 idempotency). The next gate-checked stage
// (Validate) is still added when it is missing.
func TestPausedRecovery_ReviewCompleteAlreadyPresent_NoDuplicateAdd(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 3, Merged: true}, nil
		},
	}
	stgs := pausedRecoveryStages()
	eng := testEngineWithStages(t, client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 55,
		ItemID: "PVTI_55",
		Status: "Review",
		Labels: []string{
			"fabrik:paused",
			"fabrik:awaiting-review",
			"stage:Implement:complete",
			"stage:Review:complete", // already present — must not be added again
		},
	}

	advancedItems := make(map[string]bool)
	eng.runPausedItemMergedPRRecovery(board, []gh.ProjectItem{item}, advancedItems)

	added := addedLabelNames(client.addLabelCalls)
	for _, lbl := range added {
		if lbl == "stage:Review:complete" {
			t.Errorf("stage:Review:complete was added again when it was already present; all adds: %v", added)
		}
	}
	// Validate:complete should still be added (it is missing even though Review is present).
	if !containsLabel(added, "stage:Validate:complete") {
		t.Errorf("expected stage:Validate:complete to be added, got %v", added)
	}
}
