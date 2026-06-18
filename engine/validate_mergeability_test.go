package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestValidate_BlockedOnInput_NonMergeableScenario verifies that when the Validate
// stage emits FABRIK_BLOCKED_ON_INPUT (e.g. because the PR is non-mergeable),
// the engine applies fabrik:paused + fabrik:awaiting-input and does NOT apply
// stage:Validate:complete. This is the engine-side half of the guard introduced
// to stop false FABRIK_STAGE_COMPLETE signals from Validate on aborted-rebase /
// non-mergeable PR scenarios (issue #828).
func TestValidate_BlockedOnInput_NonMergeableScenario(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "PR mergeable: CONFLICTING — rebase required before signaling complete.\nFABRIK_BLOCKED_ON_INPUT\n", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			MaxRetries: 3,
			Stages:     testStagesWithValidate(),
		},
		client,
		claude,
		wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "Non-mergeable PR issue",
		Status: "Validate",
		ItemID: "PVTI_42",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// fabrik:paused and fabrik:awaiting-input must be added
	var addedPaused, addedAwaiting bool
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:paused" {
			addedPaused = true
		}
		if call.labelName == "fabrik:awaiting-input" {
			addedAwaiting = true
		}
	}
	if !addedPaused {
		t.Error("expected fabrik:paused to be added when Validate emits FABRIK_BLOCKED_ON_INPUT")
	}
	if !addedAwaiting {
		t.Error("expected fabrik:awaiting-input to be added when Validate emits FABRIK_BLOCKED_ON_INPUT")
	}

	// stage:Validate:complete must NOT be added
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Validate:complete" {
			t.Errorf("stage:Validate:complete must not be added when FABRIK_BLOCKED_ON_INPUT is emitted, got it in addLabelCalls")
		}
	}

	// A notification comment must be posted (awaiting-input notification)
	var foundNotification bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "testuser") && strings.Contains(c.body, "awaiting") {
			foundNotification = true
		}
	}
	if !foundNotification {
		t.Errorf("expected awaiting-input notification comment containing @testuser and 'awaiting', got: %v", client.addCommentCalls)
	}
}
