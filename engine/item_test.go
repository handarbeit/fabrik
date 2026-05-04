package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// implementStages returns a minimal stage list with a single Implement stage
// that has MaxTurns set so turn-limit detection works.
func implementStages(maxTurns int) []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Implement",
			Order:      1,
			MaxTurns:   maxTurns,
			Prompt:     "Implement it",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
}

// TestExtensionLoop_ProgressDetected verifies that when the first invocation hits
// max_turns and the mock makes a git commit (simulating real progress), the engine
// performs a second invocation with resume=true, concatenates output, and marks
// the stage complete.
func TestExtensionLoop_ProgressDetected(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			switch callCount {
			case 1:
				// Simulate progress: make a commit in the worktree, then return turn-limit hit.
				cmd := exec.Command("git", "commit", "--allow-empty", "-m", "progress commit")
				cmd.Dir = workDir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git commit in mock failed: %s: %v", out, err)
				}
				return "output-from-ext1\n", false, TokenUsage{TurnsUsed: 10, InputTokens: 100}, nil
			case 2:
				if !resume {
					t.Error("second invocation must use resume=true")
				}
				return "output-from-ext2\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{TurnsUsed: 8, InputTokens: 80}, nil
			default:
				t.Errorf("unexpected invocation #%d", callCount)
				return "", false, TokenUsage{}, nil
			}
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     implementStages(10),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Extend Test",
		Status: "Implement",
		ItemID: "PVTI_1",
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 claude invocations, got %d", callCount)
	}

	// Output posted should contain both invocations' output
	if len(client.addCommentCalls) == 0 {
		t.Fatal("expected at least one comment posted")
	}
	posted := client.addCommentCalls[0].body
	if !strings.Contains(posted, "output-from-ext1") {
		t.Errorf("posted comment missing output from first extension: %q", posted)
	}
	if !strings.Contains(posted, "output-from-ext2") {
		t.Errorf("posted comment missing output from second extension: %q", posted)
	}
}

// TestExtensionLoop_NoProgress verifies that when the first invocation hits max_turns
// but no git progress is detected, the engine does NOT perform a second invocation.
func TestExtensionLoop_NoProgress(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			// Hit turn limit; make NO commit — progress detection should fail.
			return "partial output\n", false, TokenUsage{TurnsUsed: 10}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     implementStages(10),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 2,
		Title:  "No Progress Test",
		Status: "Implement",
		ItemID: "PVTI_2",
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 claude invocation (no extension without progress), got %d", callCount)
	}
}

// TestExtensionLoop_HardCap verifies that extensions stop at 3× stage.MaxTurns
// even when progress is detected each time.
func TestExtensionLoop_HardCap(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	callCount := 0
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			callCount++
			// Always make a commit to simulate progress, always hit max_turns.
			cmd := exec.Command("git", "commit", "--allow-empty",
				"-m", "progress commit "+string(rune('0'+callCount)))
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit failed: %s: %v", out, err)
			}
			return "output\n", false, TokenUsage{TurnsUsed: 10}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     implementStages(10),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 3,
		Title:  "Hard Cap Test",
		Status: "Implement",
		ItemID: "PVTI_3",
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// 3× hard cap: first invocation (totalMultiple=1), second (totalMultiple=2), third (totalMultiple=3).
	// After the third invocation, totalMultiple >= 3 → no more extensions.
	if callCount != 3 {
		t.Errorf("expected 3 claude invocations (hard cap), got %d", callCount)
	}
}

// TestExtensionLoop_ExtendTurnsLabel verifies that when fabrik:extend-turns is present,
// the first invocation gets 2× the normal budget (opts.MaxTurnsOverride = 2×MaxTurns).
func TestExtensionLoop_ExtendTurnsLabel(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Verify that on the first call the override is 2×10=20
			if opts.MaxTurnsOverride != 20 {
				t.Errorf("first invocation MaxTurnsOverride = %d, want 20 (2× MaxTurns)", opts.MaxTurnsOverride)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     implementStages(10),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 4,
		Title:  "Extend Label Test",
		Status: "Implement",
		ItemID: "PVTI_4",
		Labels: []string{"fabrik:extend-turns"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
}

// TestExtensionLoop_ExtendTurnsLabel_PersistsAcrossStage verifies that fabrik:extend-turns
// is NOT removed after successful non-cleanup stage completion.
func TestExtensionLoop_ExtendTurnsLabel_PersistsAcrossStage(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     implementStages(10),
		},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 5,
		Title:  "Persist Test",
		Status: "Implement",
		ItemID: "PVTI_5",
		Labels: []string{"fabrik:extend-turns"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Verify fabrik:extend-turns was NOT removed on non-cleanup stage completion
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:extend-turns" {
			t.Error("fabrik:extend-turns must not be removed on successful non-cleanup stage completion")
		}
	}
}

// multiStageList returns a stage list with Specify→Research→Plan→Implement, all non-cleanup.
func multiStageList(maxTurns int) []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1, MaxTurns: maxTurns, Prompt: "Specify it",
			Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Research", Order: 2, MaxTurns: maxTurns, Prompt: "Research it",
			Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Plan", Order: 3, MaxTurns: maxTurns, Prompt: "Plan it",
			Completion: stages.CompletionCriteria{Type: "claude"}},
		{Name: "Implement", Order: 4, MaxTurns: maxTurns, Prompt: "Implement it",
			Completion: stages.CompletionCriteria{Type: "claude"}},
	}
}

// TestExtendTurns_PersistsAcrossMultipleStages verifies that fabrik:extend-turns is not
// removed after any of the non-cleanup stages in the Specify→Research→Plan→Implement sequence.
func TestExtendTurns_PersistsAcrossMultipleStages(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token",
			Stages: multiStageList(10),
		},
		client, claude, wm,
	)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}

	for _, stageName := range []string{"Specify", "Research", "Plan", "Implement"} {
		item := gh.ProjectItem{
			Number: 6,
			Title:  "Multi Stage Test",
			Status: stageName,
			ItemID: "PVTI_6",
			Labels: []string{"fabrik:extend-turns"},
		}
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem(%s): %v", stageName, err)
		}
		// After each non-cleanup stage, extend-turns must still be present (not removed)
		for _, call := range client.removeLabelCalls {
			if call.labelName == "fabrik:extend-turns" {
				t.Errorf("fabrik:extend-turns was removed after %s stage — must persist until Done", stageName)
			}
		}
		// Reset spy for the next iteration
		client.removeLabelCalls = nil
	}
}

// cleanupStageList returns a single Done stage with CleanupWorktree: true.
func cleanupStageList() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Done", Order: 1, CleanupWorktree: true, Prompt: ""},
	}
}

// TestExtendTurns_RemovedOnCleanupStage verifies that fabrik:extend-turns is removed
// when the cleanup (Done) stage runs.
func TestExtendTurns_RemovedOnCleanupStage(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	// No Claude invocation needed — cleanup stage never calls Claude.
	claude := &mockClaudeInvoker{}

	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token",
			Stages: cleanupStageList(),
		},
		client, claude, wm,
	)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 7,
		Title:  "Cleanup Stage Test",
		Status: "Done",
		ItemID: "PVTI_7",
		Labels: []string{"fabrik:extend-turns"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	found := false
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:extend-turns" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:extend-turns to be removed in cleanup (Done) stage")
	}
}

// TestExtendTurns_ManualRemoval_DefaultBudget verifies that when fabrik:extend-turns is absent,
// the stage runs with the default 1× budget (MaxTurnsOverride == stage.MaxTurns).
func TestExtendTurns_ManualRemoval_DefaultBudget(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	var gotOverride int
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			gotOverride = opts.MaxTurnsOverride
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token",
			Stages: implementStages(10),
		},
		client, claude, wm,
	)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// No fabrik:extend-turns label — simulates manual removal before dispatch.
	item := gh.ProjectItem{
		Number: 8,
		Title:  "No Extend Label",
		Status: "Implement",
		ItemID: "PVTI_8",
		Labels: []string{},
	}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if gotOverride != 10 {
		t.Errorf("expected MaxTurnsOverride = 10 (1×), got %d", gotOverride)
	}
}

// TestExtendTurns_MaxTurnsZero_NoOp verifies that when max_turns: 0 (unlimited), the presence
// of fabrik:extend-turns has no effect — the override passed to Claude remains 0.
func TestExtendTurns_MaxTurnsZero_NoOp(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	var gotOverride int
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			gotOverride = opts.MaxTurnsOverride
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(
		Config{
			Owner: "owner", Repo: "repo", ProjectNum: 1,
			User: "testuser", Token: "token",
			Stages: implementStages(0), // MaxTurns == 0 → unlimited
		},
		client, claude, wm,
	)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 9,
		Title:  "Zero MaxTurns",
		Status: "Implement",
		ItemID: "PVTI_9",
		Labels: []string{"fabrik:extend-turns"},
	}
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// With MaxTurns == 0, budget must remain 0 regardless of extend-turns label.
	if gotOverride != 0 {
		t.Errorf("expected MaxTurnsOverride = 0 for unlimited stage, got %d", gotOverride)
	}
}
