package engine

import (
	"context"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestProcessComments_StageCompleteMarker_TriggersAdvance verifies that when
// Claude's comment processing output contains FABRIK_STAGE_COMPLETE, the engine
// calls handleStageComplete which advances the item (when yolo is active).
func TestProcessComments_StageCompleteMarker_TriggersAdvance(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Output includes the stage complete marker
			return "All looks good!\nFABRIK_STAGE_COMPLETE", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.Yolo = true // enable auto-advance
	stgs := testStagesWithValidate()
	eng.cfg.Stages = stgs
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{
		Number: 20,
		Body:   "spec",
		Status: "Research",
		ItemID: "PVTI_20",
	}
	userComments := []gh.Comment{
		{ID: "C_1", DatabaseID: 501, Author: "testuser", Body: "please finish"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// handleStageComplete should have been called, advancing the item
	if len(client.updateStatusCalls) == 0 {
		t.Error("expected UpdateProjectItemStatus to be called (stage completion via comment processing)")
	}
}

// TestProcessComments_NoStageCompleteMarker_NoAdvance verifies that without
// FABRIK_STAGE_COMPLETE in the output, no stage advancement happens.
func TestProcessComments_NoStageCompleteMarker_NoAdvance(t *testing.T) {
	skipIfNoGit(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// No FABRIK_STAGE_COMPLETE marker
			return "I've made some progress but not done yet", false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.Yolo = true

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Research", Order: 1}
	item := gh.ProjectItem{Number: 21, Body: "spec", Status: "Research", ItemID: "PVTI_21"}
	userComments := []gh.Comment{
		{ID: "C_2", DatabaseID: 502, Author: "testuser", Body: "any updates?"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	// No advancement should happen
	if len(client.updateStatusCalls) > 0 {
		t.Errorf("expected no UpdateProjectItemStatus when no FABRIK_STAGE_COMPLETE, got %d calls", len(client.updateStatusCalls))
	}
}
