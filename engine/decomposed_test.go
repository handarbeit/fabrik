// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestCheckDecomposed verifies marker detection for FABRIK_DECOMPOSED.
func TestCheckDecomposed(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"marker on its own line", "Some output\nFABRIK_DECOMPOSED\n", true},
		{"marker as last line no newline", "output\nFABRIK_DECOMPOSED", true},
		{"CRLF line ending", "output\r\nFABRIK_DECOMPOSED\r\n", true},
		{"marker followed by more lines", "FABRIK_DECOMPOSED\nmore output", true},
		{"not present", "Some output without marker", false},
		{"embedded in sentence", "Please output FABRIK_DECOMPOSED when done", false},
		{"in backticks", "`FABRIK_DECOMPOSED`", false},
		{"partial match", "FABRIK_DECOMPOSED_EXTRA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDecomposed(tc.output)
			if got != tc.want {
				t.Errorf("CheckDecomposed(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestHandleDecomposed_MovesToDone verifies that handleDecomposed adds the
// completion label and moves the item to Done.
func TestHandleDecomposed_MovesToDone(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan"}

	eng.handleDecomposed(board, item, stage)

	// Should add stage:Plan:complete label.
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Plan:complete" {
			found = true
		}
	}
	if !found {
		t.Error("expected stage:Plan:complete label to be added")
	}

	// Should call UpdateProjectItemStatus with the Done option.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected Done option ID, got %q", client.updateStatusCalls[0].optionID)
	}
	if client.updateStatusCalls[0].projectID != "PVT_1" {
		t.Errorf("expected projectID PVT_1, got %q", client.updateStatusCalls[0].projectID)
	}
}

// TestHandleDecomposed_NilStatusField logs warning and does not panic.
func TestHandleDecomposed_NilStatusField(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())
	eng.statusField = nil

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan"}

	// Should not panic.
	eng.handleDecomposed(board, item, stage)

	// Completion label still gets added before the nil check.
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Plan:complete" {
			found = true
		}
	}
	if !found {
		t.Error("expected stage:Plan:complete label even when statusField is nil")
	}

	// No status update should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update with nil statusField, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleDecomposed_NoDoneOption logs warning and does not panic.
func TestHandleDecomposed_NoDoneOption(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())
	// Remove Done from the options map.
	delete(eng.statusField.Options, "Done")

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan"}

	// Should not panic.
	eng.handleDecomposed(board, item, stage)

	// No status update should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update when Done option missing, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleDecomposed_DoesNotCallAdvanceToNextStage verifies that decomposed
// issues go directly to Done without touching advanceToNextStage logic.
// We verify this by confirming the status option is exactly "OPT_Done" (not
// "OPT_Implement" or any other next-stage option).
func TestHandleDecomposed_DirectlyDoneNotNextStage(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithValidate())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// Plan is order 2; next stage via advanceToNextStage would be Implement (OPT_Implement).
	item := gh.ProjectItem{Number: 7, ItemID: "PVTI_7"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleDecomposed(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	// Must be Done, not the next sequential stage.
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("handleDecomposed must set status to Done, got %q", client.updateStatusCalls[0].optionID)
	}
}
