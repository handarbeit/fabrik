package engine

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// testStagesWithValidate returns stages including Validate, for auto-merge tests.
func testStagesWithValidate() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research"},
		{Name: "Plan", Order: 2, Prompt: "plan"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "Done", Order: 5, Prompt: "done"},
	}
}

// testEngineWithStages creates an engine with the given stages and a status field
// configured for all of them.
func testEngineWithStages(client *mockGitHubClient, stgs []*stages.Stage) *Engine {
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        stgs,
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager("/tmp/test-repo"),
	)
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}
	return eng
}

func TestHandleStageComplete_NoYolo_NoAdvance(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())
	// Yolo is false (default), no label

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(board, item, stage)

	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance without yolo, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_CfgYolo_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.Yolo = true

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 advance, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Plan" {
		t.Errorf("advanced to wrong stage option: %s", client.updateStatusCalls[0].optionID)
	}
}

func TestHandleStageComplete_YoloLabel_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	// cfg.Yolo stays false; label provides yolo

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 advance via label, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Plan" {
		t.Errorf("advanced to wrong stage: %s", client.updateStatusCalls[0].optionID)
	}
}

func TestHandleStageComplete_YoloLabel_ValidateMergeableAdvances(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 99, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(board, item, validateStage)

	// MergePR should have been called once
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected 1 MergePR call, got %d", len(client.mergePRCalls))
	}
	if client.mergePRCalls[0].prNumber != 99 {
		t.Errorf("MergePR called with prNumber %d, want 99", client.mergePRCalls[0].prNumber)
	}
	// Should advance to Done
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected advance after merge, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_YoloLabel_ValidateUnmergeable_CommentPauseNoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 99, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return gh.ErrNotMergeable
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(board, item, validateStage)

	// Should post a comment explaining why merge didn't happen
	if len(client.addCommentCalls) == 0 {
		t.Error("expected a comment explaining unmergeable PR")
	}
	// Should add fabrik:paused label
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:paused label to be added")
	}
	// Should NOT advance
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance on unmergeable PR, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_YoloLabel_ValidateNoPR_AdvancesAnyway(t *testing.T) {
	// FindPRForIssue returns 0 — no PR found
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(board, item, validateStage)

	// No MergePR call (no PR to merge)
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR call when no PR, got %d", len(client.mergePRCalls))
	}
	// Should still advance
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected advance when no PR, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_YoloLabel_NonValidate_NoMergeAttempt(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	// Implement is NOT Validate — no merge should be attempted
	implementStage := &stages.Stage{Name: "Implement"}

	eng.handleStageComplete(board, item, implementStage)

	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR call for non-Validate stage, got %d", len(client.mergePRCalls))
	}
	// Should advance (label is set)
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected advance for non-Validate with yolo label, got %d", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_AutoAdvanceFalse_OverridesAdvanceButMergeStillFires(t *testing.T) {
	// auto_advance: false on Validate should suppress advancement but NOT suppress merge.
	// Global cfg.Yolo=true activates merge; item has no fabrik:yolo label so
	// auto_advance:false is respected (item-level yolo would override it).
	f := false
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 77, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.Yolo = true // global yolo triggers merge

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"} // no fabrik:yolo label
	validateStage := &stages.Stage{Name: "Validate", AutoAdvance: &f}

	eng.handleStageComplete(board, item, validateStage)

	// Merge should still fire (global yolo active)
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR to fire even with auto_advance:false, got %d", len(client.mergePRCalls))
	}
	// But advancement should be suppressed (auto_advance:false, no item-level yolo to override)
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when auto_advance:false, got %d", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_MergeAPIError_LogsAndDoesNotAdvance(t *testing.T) {
	// Non-ErrNotMergeable error from MergePR: log the error and do NOT advance.
	// This prevents silently moving to Done when the PR merge failed (e.g. transient
	// 5xx, permissions). The engine will retry Validate on the next cooldown cycle.
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 88, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return errors.New("network error")
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(board, item, validateStage)

	// Should NOT post unmergeable comment or fabrik:paused (not ErrNotMergeable)
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("should not add fabrik:paused for non-ErrNotMergeable error")
		}
	}
	// Should NOT advance — merge failed, no completion label was added, engine retries
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when merge API error, got %d status updates", len(client.updateStatusCalls))
	}
	// Should NOT add completion label — engine must be able to retry Validate
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("should not add stage:Validate:complete when merge failed")
		}
	}
}

// TestHandleStageComplete_CruiseLabel_NonValidate_Advances verifies that
// fabrik:cruise causes the engine to advance a non-Validate stage.
func TestHandleStageComplete_CruiseLabel_NonValidate_Advances(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 advance via cruise label, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Plan" {
		t.Errorf("advanced to wrong stage: %s", client.updateStatusCalls[0].optionID)
	}
}

// TestHandleStageComplete_CruiseLabel_Validate_NoMergeNoAdvance verifies that
// fabrik:cruise does NOT merge the PR and does NOT advance at Validate completion.
func TestHandleStageComplete_CruiseLabel_Validate_NoMergeNoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 99, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(board, item, validateStage)

	// No merge attempted
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR call for cruise+Validate, got %d", len(client.mergePRCalls))
	}
	// No advance
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance for cruise+Validate, got %d status updates", len(client.updateStatusCalls))
	}
}

// TestHandleStageComplete_CruiseLabel_OverridesAutoAdvanceFalse verifies that
// fabrik:cruise overrides auto_advance:false, causing the engine to advance.
func TestHandleStageComplete_CruiseLabel_OverridesAutoAdvanceFalse(t *testing.T) {
	f := false
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}
	stage := &stages.Stage{Name: "Research", AutoAdvance: &f}

	eng.handleStageComplete(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 advance: cruise overrides auto_advance:false, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleStageComplete_BothCruiseAndYolo_YoloWins verifies that when both
// fabrik:cruise and fabrik:yolo labels are present, yolo takes precedence:
// the PR is merged and the stage advances past Validate.
func TestHandleStageComplete_BothCruiseAndYolo_YoloWins(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 99, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo", "fabrik:cruise"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(board, item, validateStage)

	// yolo wins: merge should fire
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR to fire when both yolo and cruise present, got %d", len(client.mergePRCalls))
	}
	// yolo wins: advance to Done should happen
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected advance when yolo wins over cruise, got %d status updates", len(client.updateStatusCalls))
	}
}
