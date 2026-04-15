package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
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

// TestProcessComments_SummaryBeforeStrip verifies that the Verification section of the
// PR body is updated when comment processing completes a stage that includes a summary.
// This is the critical "summary-before-strip" timing test: the summary must be captured
// from the raw output before FABRIK_SUMMARY_BEGIN/END are stripped in-place.
func TestProcessComments_SummaryBeforeStrip_VerificationUpdated(t *testing.T) {
	skipIfNoGit(t)

	const prNum = 300
	const issueNum = 30
	const summaryText = "All green, ready to ship."

	var verificationBody string
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return prNum, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			if issueNumber == prNum {
				return "## Verification\n\n(Populated by Implement on completion)\n\n---\n\nCloses #30", nil
			}
			return "issue body", nil
		},
		updateIssueBodyFn: func(owner, repo string, issueNumber int, body string) error {
			if issueNumber == prNum {
				verificationBody = body
			}
			return nil
		},
	}

	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Output includes both FABRIK_STAGE_COMPLETE and FABRIK_SUMMARY_BEGIN...END
			output := "Changes applied.\nFABRIK_SUMMARY_BEGIN\n" + summaryText + "\nFABRIK_SUMMARY_END\nFABRIK_STAGE_COMPLETE"
			return output, false, TokenUsage{}, nil
		},
	}

	eng := testEngineWithRepo(t, client, claude)
	eng.cfg.Yolo = true
	stgs := []*stages.Stage{
		{
			Name:          "Implement",
			Order:         1,
			CreateDraftPR: true,
			Completion:    stages.CompletionCriteria{Type: "claude"},
		},
	}
	eng.cfg.Stages = stgs
	opts := make(map[string]string)
	for _, s := range stgs {
		opts[s.Name] = "OPT_" + s.Name
	}
	eng.statusField = &gh.StatusField{FieldID: "FIELD_1", Options: opts}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	stage := &stages.Stage{Name: "Implement", Order: 1, CreateDraftPR: true}
	item := gh.ProjectItem{
		Number: issueNum,
		Status: "Implement",
		ItemID: "PVTI_30",
	}
	userComments := []gh.Comment{
		{ID: "C_1", DatabaseID: 601, Author: "testuser", Body: "looks good"},
	}

	err := eng.processComments(context.Background(), board, item, stage, userComments)
	if err != nil {
		t.Fatalf("processComments: %v", err)
	}

	if verificationBody == "" {
		t.Fatal("expected UpdateIssueBody to be called on PR for Verification update after comment processing")
	}
	if !strings.Contains(verificationBody, summaryText) {
		t.Errorf("Verification section should contain summary %q, got body: %q", summaryText, verificationBody)
	}
	if !strings.Contains(verificationBody, "Closes #30") {
		t.Error("Closes #30 must be preserved in updated body")
	}
}
