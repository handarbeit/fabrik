package engine

import (
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestCheckNoWorkNeeded verifies marker detection for FABRIK_NO_WORK_NEEDED.
func TestCheckNoWorkNeeded(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"marker on its own line", "Some output\nFABRIK_NO_WORK_NEEDED\n", true},
		{"marker as last line no newline", "output\nFABRIK_NO_WORK_NEEDED", true},
		{"CRLF line ending", "output\r\nFABRIK_NO_WORK_NEEDED\r\n", true},
		{"marker followed by more lines", "FABRIK_NO_WORK_NEEDED\nmore output", true},
		{"not present", "Some output without marker", false},
		{"embedded in sentence", "Please output FABRIK_NO_WORK_NEEDED when done", false},
		{"in backticks", "`FABRIK_NO_WORK_NEEDED`", false},
		{"partial match", "FABRIK_NO_WORK_NEEDED_EXTRA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckNoWorkNeeded(tc.output)
			if got != tc.want {
				t.Errorf("CheckNoWorkNeeded(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// testStagesWithCleanup returns stages with the Done stage marked as CleanupWorktree,
// for use in handleNoWorkNeeded tests.
func testStagesWithCleanup() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research"},
		{Name: "Plan", Order: 2, Prompt: "plan"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "Done", Order: 5, Prompt: "done", CleanupWorktree: true},
	}
}

// TestHandleNoWorkNeeded_MovesToDone verifies that handleNoWorkNeeded adds the
// emitting stage's completion label and moves the item to Done.
func TestHandleNoWorkNeeded_MovesToDone(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

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

// TestHandleNoWorkNeeded_NilStatusField logs warning and does not panic.
func TestHandleNoWorkNeeded_NilStatusField(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithCleanup())
	eng.statusField = nil

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	// Should not panic.
	eng.handleNoWorkNeeded(board, item, stage)

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

// TestHandleNoWorkNeeded_NoDoneOption logs warning and does not panic.
func TestHandleNoWorkNeeded_NoDoneOption(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithCleanup())
	delete(eng.statusField.Options, "Done")

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	// Should not panic.
	eng.handleNoWorkNeeded(board, item, stage)

	// No status update should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update when Done option missing, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleNoWorkNeeded_SkipsIntermediateStages verifies that all non-cleanup stages
// after the emitting stage receive a dummy completion label and a "skipped" comment,
// while the cleanup (Done) stage does not.
func TestHandleNoWorkNeeded_SkipsIntermediateStages(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 7, ItemID: "PVTI_7"}
	// Plan emits FABRIK_NO_WORK_NEEDED; Implement (order 3) and Validate (order 4)
	// should be skipped; Done (order 5, CleanupWorktree) should NOT be skipped.
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	// Collect all labels added.
	labelSet := make(map[string]bool)
	for _, c := range client.addLabelCalls {
		labelSet[c.labelName] = true
	}

	// Emitting stage gets its completion label.
	if !labelSet["stage:Plan:complete"] {
		t.Error("expected stage:Plan:complete label")
	}
	// Implement and Validate get skipped labels.
	if !labelSet["stage:Implement:complete"] {
		t.Error("expected stage:Implement:complete skip label")
	}
	if !labelSet["stage:Validate:complete"] {
		t.Error("expected stage:Validate:complete skip label")
	}
	// Done must NOT get a skip label (it's the cleanup stage).
	if labelSet["stage:Done:complete"] {
		t.Error("expected no stage:Done:complete skip label (cleanup stage must be excluded)")
	}

	// Two "skipped" comments should be posted (one per skipped stage: Implement, Validate).
	if len(client.addCommentCalls) != 2 {
		t.Fatalf("expected 2 skipped comments, got %d", len(client.addCommentCalls))
	}
	for _, c := range client.addCommentCalls {
		if !strings.Contains(c.body, "FABRIK_NO_WORK_NEEDED emitted by Plan") {
			t.Errorf("expected comment to mention emitting stage, got: %q", c.body)
		}
	}

	// One status update to Done.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected Done option, got %q", client.updateStatusCalls[0].optionID)
	}
}

// TestHandleNoWorkNeeded_DirectlyDoneNotNextStage verifies that the issue advances
// directly to Done, not to the next sequential stage.
func TestHandleNoWorkNeeded_DirectlyDoneNotNextStage(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 9, ItemID: "PVTI_9"}
	// Plan is order 2; advanceToNextStage would go to Implement (OPT_Implement).
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	// Must be Done, not the next sequential stage.
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("handleNoWorkNeeded must set status to Done, got %q", client.updateStatusCalls[0].optionID)
	}
}
