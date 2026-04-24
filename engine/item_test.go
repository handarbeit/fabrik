package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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

// TestExtensionLoop_ExtendTurnsLabel_AutoRemoved verifies that fabrik:extend-turns
// is removed after successful stage completion.
func TestExtensionLoop_ExtendTurnsLabel_AutoRemoved(t *testing.T) {
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
		Title:  "Auto Remove Test",
		Status: "Implement",
		ItemID: "PVTI_5",
		Labels: []string{"fabrik:extend-turns"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Verify fabrik:extend-turns was removed
	found := false
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:extend-turns" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:extend-turns to be removed on successful completion")
	}
}
