package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestAdvanceToNextStage_Success(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_1",
		Options: map[string]string{
			"Research": "OPT_1",
			"Plan":     "OPT_2",
		},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	stage := &stages.Stage{Name: "Research"}

	err := eng.advanceToNextStage(board, item, stage)
	if err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(client.updateStatusCalls))
	}
	call := client.updateStatusCalls[0]
	if call.projectID != "PVT_1" || call.optionID != "OPT_2" {
		t.Errorf("update call = %+v", call)
	}
}

func TestAdvanceToNextStage_LastStage(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_1",
		Options: map[string]string{},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Implement"} // last stage

	err := eng.advanceToNextStage(board, item, stage)
	if err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	if len(client.updateStatusCalls) != 0 {
		t.Error("should not update status for last stage")
	}
}

func TestAdvanceToNextStage_NoStatusField(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	// statusField is nil

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Research"}

	err := eng.advanceToNextStage(board, item, stage)
	if err == nil {
		t.Fatal("expected error when statusField is nil")
	}
}

func TestAdvanceToNextStage_MissingOption(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{
		FieldID: "FIELD_1",
		Options: map[string]string{
			"Research": "OPT_1",
			// Plan option is missing
		},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1}
	stage := &stages.Stage{Name: "Research"}

	err := eng.advanceToNextStage(board, item, stage)
	if err == nil {
		t.Fatal("expected error for missing option")
	}
}

