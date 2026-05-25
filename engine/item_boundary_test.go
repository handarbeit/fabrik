package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// boundaryStages returns a minimal Implement stage for boundary audit tests.
func boundaryStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Implement",
			Order:      1,
			Prompt:     "Implement it",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
}

// boundaryEngine creates an engine with a primary WorktreeManager backed by
// primaryDir. A secondary bare-style repo dir is registered under "other/repo"
// so the cross-repo audit has two repos to compare.
func boundaryEngine(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker, primaryDir, secondaryDir string) *Engine {
	t.Helper()
	wm := NewWorktreeManager(primaryDir)
	eng := NewWithDeps(Config{
		Owner:                 "owner",
		Repo:                  "repo",
		User:                  "testuser",
		Token:                 "token",
		Stages:                boundaryStages(),
		WorktreeBoundaryAudit: true,
	}, client, claude, wm)

	// Register the secondary repo so it participates in the cross-repo audit.
	secondaryWM := NewWorktreeManager(secondaryDir)
	eng.mu.Lock()
	eng.worktreeManagers["other/repo"] = secondaryWM
	eng.mu.Unlock()
	return eng
}

// TestBoundaryAudit_ViolationDetected verifies that when the mock Claude invoker
// mutates a ref in a secondary repo, the engine detects the violation, posts a
// comment naming the mutation, and adds stage:Implement:failed.
func TestBoundaryAudit_ViolationDetected(t *testing.T) {
	skipIfNoGit(t)

	primaryDir := initBareRepo(t)
	secondaryDir := initBareRepo(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Simulate cross-repo mutation: create a new branch in the secondary repo.
			cmd := exec.Command("git", "branch", "evil-branch")
			cmd.Dir = secondaryDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git branch in secondary repo failed: %s: %v", out, err)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := boundaryEngine(t, client, claude, primaryDir, secondaryDir)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, Title: "Test", Status: "Implement", ItemID: "PVTI_1"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem returned unexpected error: %v", err)
	}

	// A boundary violation comment must have been posted.
	client.mu.Lock()
	var violationComment string
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "boundary violation") {
			violationComment = c.body
		}
	}
	client.mu.Unlock()

	if violationComment == "" {
		t.Fatal("expected a boundary violation comment to be posted")
	}
	if !strings.Contains(violationComment, "evil-branch") {
		t.Errorf("violation comment should mention the mutated branch, got: %q", violationComment)
	}

	// stage:Implement:failed and fabrik:paused must both be added.
	client.mu.Lock()
	var failedAdded, pausedAdded bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "stage:Implement:failed":
			failedAdded = true
		case "fabrik:paused":
			pausedAdded = true
		}
	}
	client.mu.Unlock()

	if !failedAdded {
		t.Error("expected stage:Implement:failed label to be added after violation")
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused label to be added after violation (prevents auto-retry)")
	}

	// No stage:Implement:complete label should have been added.
	client.mu.Lock()
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			t.Errorf("stage:Implement:complete must NOT be added on a violation, but was")
		}
	}
	client.mu.Unlock()
}

// TestBoundaryAudit_NoViolation verifies that when Claude only modifies the
// active repo (the primary worktree), no violation is reported and the stage
// completes normally.
func TestBoundaryAudit_NoViolation(t *testing.T) {
	skipIfNoGit(t)

	primaryDir := initBareRepo(t)
	secondaryDir := initBareRepo(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Only commit in the primary worktree — no cross-repo mutation.
			cmd := exec.Command("git", "commit", "--allow-empty", "-m", "normal work")
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git commit in worktree failed: %s: %v", out, err)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := boundaryEngine(t, client, claude, primaryDir, secondaryDir)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, Title: "Test", Status: "Implement", ItemID: "PVTI_1"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// No boundary violation comment should be posted.
	client.mu.Lock()
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "boundary violation") {
			t.Errorf("unexpected boundary violation comment: %q", c.body)
		}
	}
	client.mu.Unlock()

	// stage:Implement:complete should be added (normal completion path).
	client.mu.Lock()
	var completionAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			completionAdded = true
		}
	}
	client.mu.Unlock()

	if !completionAdded {
		t.Error("expected stage:Implement:complete to be added on a clean run")
	}
}

// TestBoundaryAudit_ReadOnlyStageSkipped verifies that read-only stages bypass
// the boundary audit entirely — mutations in a secondary repo are not detected.
func TestBoundaryAudit_ReadOnlyStageSkipped(t *testing.T) {
	skipIfNoGit(t)

	primaryDir := initBareRepo(t)
	secondaryDir := initBareRepo(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Create a branch in secondary — would be a violation for a write stage.
			cmd := exec.Command("git", "branch", "leaked-branch")
			cmd.Dir = secondaryDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git branch in secondary repo: %s: %v", out, err)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	wm := NewWorktreeManager(primaryDir)
	readOnlyStages := []*stages.Stage{
		{
			Name:       "Research",
			Order:      1,
			Prompt:     "Research it",
			ReadOnly:   true,
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
	eng := NewWithDeps(Config{
		Owner:                 "owner",
		Repo:                  "repo",
		User:                  "testuser",
		Token:                 "token",
		Stages:                readOnlyStages,
		WorktreeBoundaryAudit: true,
	}, client, claude, wm)

	// Register secondary repo so it would be audited if audit ran.
	eng.mu.Lock()
	eng.worktreeManagers["other/repo"] = NewWorktreeManager(secondaryDir)
	eng.mu.Unlock()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, Title: "Test", Status: "Research", ItemID: "PVTI_1"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// No violation comment must be posted — audit does not run for read-only stages.
	client.mu.Lock()
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "boundary violation") {
			t.Errorf("read-only stage should not trigger boundary audit, got: %q", c.body)
		}
	}
	client.mu.Unlock()
}

// TestBoundaryAudit_UnrestrictedLabelSkipped verifies that when the
// fabrik:unrestricted label is on the issue, the boundary audit is bypassed.
func TestBoundaryAudit_UnrestrictedLabelSkipped(t *testing.T) {
	skipIfNoGit(t)

	primaryDir := initBareRepo(t)
	secondaryDir := initBareRepo(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Mutate secondary repo — would trigger violation if audit ran.
			cmd := exec.Command("git", "branch", "unrestricted-branch")
			cmd.Dir = secondaryDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git branch in secondary repo: %s: %v", out, err)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := boundaryEngine(t, client, claude, primaryDir, secondaryDir)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number:  1,
		Title:   "Test",
		Status:  "Implement",
		ItemID:  "PVTI_1",
		Labels:  []string{"fabrik:unrestricted"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// No violation comment — audit is bypassed for fabrik:unrestricted.
	client.mu.Lock()
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "boundary violation") {
			t.Errorf("unrestricted label should bypass boundary audit, got: %q", c.body)
		}
	}
	client.mu.Unlock()
}

// TestBoundaryAudit_FlagDisabled verifies that when WorktreeBoundaryAudit is false
// (the default), cross-repo ref mutations are not detected and the stage completes
// normally without posting a boundary violation comment.
func TestBoundaryAudit_FlagDisabled(t *testing.T) {
	skipIfNoGit(t)

	primaryDir := initBareRepo(t)
	secondaryDir := initBareRepo(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Simulate cross-repo mutation — would trigger a violation when audit is on.
			cmd := exec.Command("git", "branch", "cross-repo-branch")
			cmd.Dir = secondaryDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git branch in secondary repo failed: %s: %v", out, err)
			}
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	// Build engine with WorktreeBoundaryAudit: false (the default — audit is off).
	wm := NewWorktreeManager(primaryDir)
	eng := NewWithDeps(Config{
		Owner:                 "owner",
		Repo:                  "repo",
		User:                  "testuser",
		Token:                 "token",
		Stages:                boundaryStages(),
		WorktreeBoundaryAudit: false,
	}, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["other/repo"] = NewWorktreeManager(secondaryDir)
	eng.mu.Unlock()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, Title: "Test", Status: "Implement", ItemID: "PVTI_1"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem returned unexpected error: %v", err)
	}

	// No boundary violation comment should be posted — audit is disabled.
	client.mu.Lock()
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "boundary violation") {
			t.Errorf("audit-disabled engine must not post boundary violation, got: %q", c.body)
		}
	}
	client.mu.Unlock()

	// Stage should complete normally.
	client.mu.Lock()
	var completionAdded bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			completionAdded = true
		}
	}
	client.mu.Unlock()

	if !completionAdded {
		t.Error("expected stage:Implement:complete to be added when audit is disabled and stage signals completion")
	}
}
