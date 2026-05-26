package engine

import (
	"context"
	"errors"
	"fmt"
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

	eng.handleStageComplete(context.Background(), board, item, stage)

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

	eng.handleStageComplete(context.Background(), board, item, stage)

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

	eng.handleStageComplete(context.Background(), board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 advance via label, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Plan" {
		t.Errorf("advanced to wrong stage: %s", client.updateStatusCalls[0].optionID)
	}
}

func TestHandleStageComplete_YoloLabel_ValidateMergeableAdvances(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "abc123"}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	// EnablePullRequestAutoMerge (not MergePR) should have been called once.
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected 1 EnablePullRequestAutoMerge call, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if client.enablePullRequestAutoMergeCalls[0].prNumber != 99 {
		t.Errorf("EnablePullRequestAutoMerge called with prNumber %d, want 99", client.enablePullRequestAutoMergeCalls[0].prNumber)
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called in the new auto-merge path, got %d call(s)", len(client.mergePRCalls))
	}
	// Done advancement is deferred to checkAutoMergeConvergence; no immediate advance.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no immediate advance (deferred to convergence monitor), got %d status updates", len(client.updateStatusCalls))
	}
}

// TestHandleStageComplete_YoloLabel_ValidateAutoMergeNotEnabled_NoAdvance verifies
// that when EnablePullRequestAutoMerge returns ErrAutoMergeNotEnabled (the repository
// has not enabled auto-merge in its settings), handleStageComplete logs a warning,
// does NOT add stage:Validate:complete, and does NOT advance to Done — so the engine
// can retry on the next poll cycle.
func TestHandleStageComplete_YoloLabel_ValidateAutoMergeNotEnabled_NoAdvance(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "abc123"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return gh.ErrAutoMergeNotEnabled
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	// No pause, no completion label — engine retries on next poll.
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added when auto-merge is not enabled on the repo")
		}
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must NOT be added when auto-merge enablement failed")
		}
	}
	// No advance.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when auto-merge enablement failed, got %d status updates", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_YoloLabel_ValidateNoPR_AdvancesAnyway(t *testing.T) {
	// FetchLinkedPR returns nil — no PR found
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

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

	eng.handleStageComplete(context.Background(), board, item, implementStage)

	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR call for non-Validate stage, got %d", len(client.mergePRCalls))
	}
	// Should advance (label is set)
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected advance for non-Validate with yolo label, got %d", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_AutoAdvanceFalse_OverridesAdvanceButMergeStillFires(t *testing.T) {
	// auto_advance: false on Validate should suppress advancement but NOT suppress
	// auto-merge enablement. Global cfg.Yolo=true activates auto-merge; item has no
	// fabrik:yolo label so auto_advance:false is respected (item-level yolo would override it).
	f := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, HeadSHA: "abc123"}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.Yolo = true // global yolo triggers auto-merge

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"} // no fabrik:yolo label
	validateStage := &stages.Stage{Name: "Validate", AutoAdvance: &f}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	// EnablePullRequestAutoMerge should fire (global yolo active).
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge to fire even with auto_advance:false, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called in new auto-merge path, got %d", len(client.mergePRCalls))
	}
	// Advancement suppressed: auto_advance:false AND autoMergeEnabled defers Done.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when auto_advance:false, got %d", len(client.updateStatusCalls))
	}
}

func TestHandleStageComplete_MergeAPIError_LogsAndDoesNotAdvance(t *testing.T) {
	// Transient API error from both EnablePullRequestAutoMerge and the MergePR fallback:
	// log the error and do NOT advance. Under the generalised fallback, any
	// non-ErrAutoMergeNotEnabled error triggers a direct MergePR attempt. When that
	// also fails (e.g. PR not yet mergeable), the engine must not advance — it will
	// retry Validate on the next cooldown cycle.
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 88, HeadSHA: "abc123"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return errors.New("network error")
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

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	// Should NOT add fabrik:paused (not a terminal failure, just a retriable error).
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			t.Error("should not add fabrik:paused for transient auto-merge API error")
		}
	}
	// Should NOT advance — enablement failed, no completion label added, engine retries.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when auto-merge enablement fails, got %d status updates", len(client.updateStatusCalls))
	}
	// Should NOT add completion label — engine must be able to retry Validate.
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("should not add stage:Validate:complete when auto-merge enablement failed")
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

	eng.handleStageComplete(context.Background(), board, item, stage)

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
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

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

	eng.handleStageComplete(context.Background(), board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 advance: cruise overrides auto_advance:false, got %d", len(client.updateStatusCalls))
	}
}

// ── wait_for_ci: true conjunctive gate ───────────────────────────────────────

// TestHandleStageComplete_WaitForCI_AddsAwaitingCINotComplete verifies that
// when wait_for_ci: true, handleStageComplete adds fabrik:awaiting-ci and does
// NOT add stage:X:complete (R1, R2).
func TestHandleStageComplete_WaitForCI_AddsAwaitingCINotComplete(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.Yolo = true // ensure yolo doesn't bypass the new gate

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}
	validateStage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	foundAwaitingCI := false
	foundComplete := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundAwaitingCI = true
		}
		if c.labelName == "stage:Validate:complete" {
			foundComplete = true
		}
	}
	if !foundAwaitingCI {
		t.Error("expected fabrik:awaiting-ci to be added when wait_for_ci: true")
	}
	if foundComplete {
		t.Error("stage:Validate:complete must NOT be added when wait_for_ci: true (deferred to checkCIGate)")
	}
	// No stage advancement either
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when wait_for_ci: true, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleStageComplete_WaitForCI_Idempotent verifies that when
// fabrik:awaiting-ci is already present, handleStageComplete does not re-add it.
func TestHandleStageComplete_WaitForCI_Idempotent(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// Item already has the awaiting-ci label
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:awaiting-ci"}}
	validateStage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	count := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			count++
		}
	}
	if count != 0 {
		t.Errorf("fabrik:awaiting-ci should not be re-added when already present, got %d add calls", count)
	}
}

// TestHandleStageComplete_WaitForCI_DoesNotSeedAwaitingReview verifies that
// when wait_for_ci: true AND wait_for_reviews: true, only fabrik:awaiting-ci
// is added — fabrik:awaiting-review is NOT seeded here. Path 2 (checkReviewGate
// in the catch-up loop) handles the review gate after CI clears (#617, Bug A).
func TestHandleStageComplete_WaitForCI_DoesNotSeedAwaitingReview(t *testing.T) {
	tr := true
	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	validateStage := &stages.Stage{Name: "Validate", WaitForCI: &tr, WaitForReviews: &tr}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	foundCI := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundCI = true
		}
		if c.labelName == "fabrik:awaiting-review" {
			t.Errorf("handleStageComplete must not seed fabrik:awaiting-review when wait_for_ci: true (#617)")
		}
	}
	if !foundCI {
		t.Error("expected fabrik:awaiting-ci when wait_for_ci: true")
	}
}

// TestHandleStageComplete_WaitForCI_AppliesRegardlessOfYolo verifies that the
// wait_for_ci gate fires for both yolo and non-yolo items (R1 applies regardless).
func TestHandleStageComplete_WaitForCI_AppliesRegardlessOfYolo(t *testing.T) {
	tr := true
	for _, yolo := range []bool{true, false} {
		t.Run(fmt.Sprintf("yolo=%v", yolo), func(t *testing.T) {
			client := &mockGitHubClient{}
			stgs := testStagesWithValidate()
			eng := testEngineWithStages(client, stgs)
			eng.cfg.Yolo = yolo

			board := &gh.ProjectBoard{ProjectID: "PVT_1"}
			item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
			validateStage := &stages.Stage{Name: "Validate", WaitForCI: &tr}

			eng.handleStageComplete(context.Background(), board, item, validateStage)

			foundCI := false
			foundComplete := false
			for _, c := range client.addLabelCalls {
				if c.labelName == "fabrik:awaiting-ci" {
					foundCI = true
				}
				if c.labelName == "stage:Validate:complete" {
					foundComplete = true
				}
			}
			if !foundCI {
				t.Errorf("yolo=%v: expected fabrik:awaiting-ci regardless of yolo setting", yolo)
			}
			if foundComplete {
				t.Errorf("yolo=%v: stage:Validate:complete must not be added for wait_for_ci: true", yolo)
			}
		})
	}
}

// TestHandleStageComplete_BothCruiseAndYolo_CruiseWins verifies that when both
// fabrik:cruise and fabrik:yolo labels are present, cruise takes precedence at
// Validate: EnablePullRequestAutoMerge is NOT called and the item is NOT advanced
// to Done. This implements FR-003 / FR-015: cruise leaves the PR for human merge.
func TestHandleStageComplete_BothCruiseAndYolo_CruiseWins(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "abc123"}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo", "fabrik:cruise"}}
	validateStage := &stages.Stage{Name: "Validate"}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	// cruise wins: auto-merge must NOT be enabled.
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Fatalf("expected no EnablePullRequestAutoMerge when cruise wins over yolo, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called, got %d", len(client.mergePRCalls))
	}
	// No advancement — cruise stops at Validate.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no advance when cruise wins at Validate, got %d status updates", len(client.updateStatusCalls))
	}
}

// TestHandleStageComplete_ClearsAwaitingInput verifies that fabrik:awaiting-input is
// removed when a stage completes, covering the orphaned-label scenario where the user
// manually removed fabrik:paused after FABRIK_BLOCKED_ON_INPUT was emitted.
func TestHandleStageComplete_ClearsAwaitingInput(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:awaiting-input"}}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(context.Background(), board, item, stage)

	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-input" {
			found = true
			if c.issueNumber != 1 {
				t.Errorf("RemoveLabelFromIssue called with issueNumber %d, want 1", c.issueNumber)
			}
		}
	}
	if !found {
		t.Error("expected RemoveLabelFromIssue call for fabrik:awaiting-input, got none")
	}
}

// TestHandleStageComplete_NoAwaitingInput_NoSpuriousRemove verifies that when
// fabrik:awaiting-input is absent, no spurious RemoveLabelFromIssue call is made
// for that label name.
func TestHandleStageComplete_NoAwaitingInput_NoSpuriousRemove(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(context.Background(), board, item, stage)

	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-input" {
			t.Error("unexpected RemoveLabelFromIssue call for fabrik:awaiting-input when label was not present")
		}
	}
}
