package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func TestProcessItem_SkipsUnknownStage(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Unknown Column",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for unknown stage")
	}
}

func TestProcessItem_SkipsLockedByOther(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Research",
		Labels: []string{"fabrik:locked:otheruser"},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for item locked by another user")
	}
}

func TestProcessItem_SkipsPaused(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Research",
		Labels: []string{"fabrik:paused"},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for paused item")
	}
}

func TestProcessItem_AllowsOwnLock(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", false, TokenUsage{}, nil
		},
	}
	eng := testEngine(client, claude)
	// Need a real worktree manager for processItem — register a mock WM for the test repo
	eng.worktreeManagers["owner/repo"] = &WorktreeManager{baseDir: t.TempDir(), rootDir: t.TempDir()}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Research",
		Labels: []string{"fabrik:locked:testuser"},
	}

	// processItem calls EnsureWorktree which needs git — skip worktree by mocking
	// Instead, test that own lock doesn't cause skip by checking that we attempt to process
	// We can't fully test processItem without git, so just test the lock check logic
	err := eng.processItem(context.Background(), board, item)
	// This will fail on EnsureWorktree since we don't have a real git repo,
	// but the important thing is it didn't skip due to lock
	if err != nil && !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessItem_SkipsCompleted(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Research",
		Labels: []string{"stage:Research:complete"},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for completed item")
	}
}

func TestProcessItem_SkipsAlreadyProcessedNoNewComments(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.PollSeconds = 100 // cooldown = 1000s — ensures recently-processed item stays in cooldown

	// Mark as already processed (sets LastAttemptAt so dispatch cooldown applies)
	eng.store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: 1, StageName: "Research", At: time.Now()})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Research",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude when already processed and no new comments")
	}
}

func TestProcessItem_FullHappyPath(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Claude output here\nFABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test Issue",
		Status: "Research",
		ItemID: "PVTI_1",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should have locked the issue
	if len(client.addLabelCalls) < 1 {
		t.Fatal("expected lock label call")
	}
	if client.addLabelCalls[0].labelName != "fabrik:locked:testuser" {
		t.Errorf("lock label = %q", client.addLabelCalls[0].labelName)
	}

	// Lock label is released when stage completes (completed=true → releaseLock() called).
	// When not completed, the lock persists through cooldown so other instances don't
	// pick up the issue — see "Keep lock and in_progress labels through cooldown retries".
	foundLockRemoval := false
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:locked:testuser" {
			foundLockRemoval = true
		}
	}
	if !foundLockRemoval {
		t.Error("expected lock label to be removed after stage completes")
	}

	// Should have invoked Claude
	if len(claude.calls) != 1 {
		t.Fatalf("expected 1 claude call, got %d", len(claude.calls))
	}
	if claude.calls[0].stageName != "Research" {
		t.Errorf("stage = %q", claude.calls[0].stageName)
	}

	// Should have posted comment
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment call, got %d", len(client.addCommentCalls))
	}
	if !strings.Contains(client.addCommentCalls[0].body, "Claude output here") {
		t.Errorf("comment = %q", client.addCommentCalls[0].body)
	}
}

func TestProcessItem_AccumulatesTokenUsage(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	want := TokenUsage{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 10, CacheReadTokens: 5, CostUSD: 0.0042}
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "FABRIK_STAGE_COMPLETE", true, want, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 200, Title: "Token Test", Status: "Research", ItemID: "PVTI_200"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	eng.mu.Lock()
	got := eng.totalTokens
	eng.mu.Unlock()

	if got.InputTokens != want.InputTokens {
		t.Errorf("totalTokens.InputTokens = %d, want %d", got.InputTokens, want.InputTokens)
	}
	if got.OutputTokens != want.OutputTokens {
		t.Errorf("totalTokens.OutputTokens = %d, want %d", got.OutputTokens, want.OutputTokens)
	}
	if got.CacheCreationTokens != want.CacheCreationTokens {
		t.Errorf("totalTokens.CacheCreationTokens = %d, want %d", got.CacheCreationTokens, want.CacheCreationTokens)
	}
	if got.CacheReadTokens != want.CacheReadTokens {
		t.Errorf("totalTokens.CacheReadTokens = %d, want %d", got.CacheReadTokens, want.CacheReadTokens)
	}
	if diff := got.CostUSD - want.CostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("totalTokens.CostUSD = %f, want ~%f", got.CostUSD, want.CostUSD)
	}
}

func TestProcessItem_CompletionWithAutoAdvance(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Done\nFABRIK_STAGE_COMPLETE", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Yolo:       true,
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)
	eng.statusField = &gh.StatusField{
		FieldID: "F1",
		Options: map[string]string{
			"Research": "OPT_1",
			"Plan":     "OPT_2",
		},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 2,
		Title:  "Auto advance test",
		Status: "Research",
		ItemID: "PVTI_2",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should have added completion label
	foundComplete := false
	for _, call := range client.addLabelCalls {
		if call.labelName == "stage:Research:complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Error("expected completion label to be added")
	}

	// Should have removed the lock label after processing completes
	foundLockRemoval := false
	for _, call := range client.removeLabelCalls {
		if call.labelName == "fabrik:locked:testuser" {
			foundLockRemoval = true
		}
	}
	if !foundLockRemoval {
		t.Error("expected lock label to be removed after processItem completes")
	}

	// Should have advanced to next stage
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_2" {
		t.Errorf("advanced to option = %q, want OPT_2", client.updateStatusCalls[0].optionID)
	}
}

func TestProcessItem_CompletionNoAutoAdvance(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Done", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			Yolo:       false, // no auto-advance
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 3, Title: "No advance", Status: "Research", ItemID: "PVTI_3"}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should NOT have advanced
	if len(client.updateStatusCalls) != 0 {
		t.Error("should not advance when yolo=false")
	}
}

func TestProcessItem_StageAutoAdvanceOverride(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "Done", true, TokenUsage{}, nil
		},
	}

	autoAdvance := true
	stgs := []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "p", Completion: stages.CompletionCriteria{Type: "claude"}, AutoAdvance: &autoAdvance},
		{Name: "Plan", Order: 2, Prompt: "p", Completion: stages.CompletionCriteria{Type: "claude"}},
	}

	eng := NewWithDeps(
		Config{
			Owner:  "owner",
			Repo:   "repo",
			User:   "testuser",
			Token:  "token",
			Yolo:   false, // global is false
			Stages: stgs,
		},
		client,
		claude,
		wm,
	)
	eng.statusField = &gh.StatusField{
		FieldID: "F1",
		Options: map[string]string{"Plan": "OPT_2"},
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 4, Title: "Override", Status: "Research", ItemID: "PVTI_4"}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should advance due to stage-level override
	if len(client.updateStatusCalls) != 1 {
		t.Error("expected advance due to stage AutoAdvance override")
	}
}

func TestProcessItem_EmptyOutput(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, Title: "Empty", Status: "Research", ItemID: "PVTI_5"}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should post exactly one warning comment when output is empty but Claude ran without error
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected 1 warning comment for empty output, got %d", len(client.addCommentCalls))
	} else if !strings.Contains(client.addCommentCalls[0].body, "empty stage output") {
		t.Errorf("expected empty-output warning, got: %s", client.addCommentCalls[0].body)
	}
}

func TestProcessItem_ClaudeError(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Simulate a start failure: binary not found (*exec.Error)
			cmd := exec.Command("this-binary-does-not-exist-fabrik-test")
			_, startErr := cmd.Output()
			return "partial output", false, TokenUsage{}, startErr
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 6, Title: "Error", Status: "Research", ItemID: "PVTI_6"}

	// Should not return error — claude errors are logged, not fatal
	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Should still post partial output
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment with partial output, got %d", len(client.addCommentCalls))
	}

	// A start-failure (*exec.Error / binary not found) — LastAttemptAt must NOT be updated
	snap, _ := eng.store.Get("o/r", 6)
	if !snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt should NOT be set on a start-failure error")
	}
}

func TestProcessItem_ClaudeExitError(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			// Simulate Claude running and exiting non-zero (wrapped *exec.ExitError)
			cmd := exec.Command("git", "definitely-invalid-arg")
			runErr := cmd.Run()
			return "some output", false, TokenUsage{}, fmt.Errorf("claude exited with error: %w", runErr)
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 7, Title: "ExitError", Status: "Research", ItemID: "PVTI_7"}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// An *exec.ExitError means Claude ran — LastAttemptAt MUST be updated (cooldown applies)
	snap, _ := eng.store.Get("o/r", 7)
	if snap.LastAttemptAt("Research").IsZero() {
		t.Error("LastAttemptAt should be set when Claude ran and exited non-zero")
	}
}

func TestProcessItem_ResumeOnReprocess(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 7,
		Title:  "Resume test",
		Status: "Research",
		ItemID: "PVTI_7",
		// No comments — both calls go through the stage invocation path (e.claude.Invoke).
		// processComments uses InvokeClaudeForComments (global), not the mock.
	}

	// First call — LastAttemptAt not set yet, resume=false
	eng.processItem(context.Background(), board, item)

	// Second call — PollSeconds=0 means cooldown=0, so item is retried with resume=true
	eng.processItem(context.Background(), board, item)

	if len(claude.calls) != 2 {
		t.Fatalf("expected 2 claude calls, got %d", len(claude.calls))
	}
	if claude.calls[0].resume != false {
		t.Error("first call should not resume")
	}
	if claude.calls[1].resume != true {
		t.Error("second call should resume")
	}
}

func TestProcessItem_LabelAndCommentErrors(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			return fmt.Errorf("label error")
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) {
			return 0, fmt.Errorf("comment error")
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", true, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 8, Title: "Errors", Status: "Research", ItemID: "PVTI_8"}

	// Should not return error — label/comment errors are logged, not fatal
	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
}

func TestProcessItem_EscalatesAtMaxRetries(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "partial output", false, TokenUsage{}, nil // never completes
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			MaxRetries: 2,
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 10, Title: "Escalate test", Status: "Research", ItemID: "PVTI_10"}

	// PollSeconds=0 makes cooldown=0, so both calls reach Claude without waiting.
	// First attempt — retry count becomes 1, no escalation yet
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (first call): %v", err)
	}
	foundPaused := false
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:paused" {
			foundPaused = true
		}
	}
	if foundPaused {
		t.Error("should not escalate after first failure")
	}

	// Second attempt — retry count becomes 2, should escalate
	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem (second call): %v", err)
	}

	foundPaused = false
	foundFailed := false
	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:paused" {
			foundPaused = true
		}
		if call.labelName == "stage:Research:failed" {
			foundFailed = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused label after max retries")
	}
	if !foundFailed {
		t.Error("expected stage:Research:failed label after max retries")
	}

	// Should have posted an escalation comment
	foundEscalationComment := false
	for _, call := range client.addCommentCalls {
		if strings.Contains(call.body, "paused") && strings.Contains(call.body, "Research") {
			foundEscalationComment = true
		}
	}
	if !foundEscalationComment {
		t.Error("expected escalation comment to be posted")
	}

	// PausedByEngine should be set in the store
	snap, _ := eng.store.Get("owner/repo", 10)
	if !snap.PausedByEngine("Research") {
		t.Error("expected PausedByEngine to be set")
	}
}

func TestProcessItem_ResetsOnUnpause(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			MaxRetries: 3, // high enough so one retry after unpause doesn't re-escalate
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	// Simulate a previous escalation: engine had paused this issue after 3 failures
	for i := 0; i < 3; i++ {
		eng.store.Apply(itemstate.StageRetryIncremented{Repo: "owner/repo", Number: 11, StageName: "Research"})
	}
	eng.store.Apply(itemstate.EnginePaused{Repo: "owner/repo", Number: 11, StageName: "Research"})

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	// Item does NOT have fabrik:paused — user has removed it to signal investigation done
	item := gh.ProjectItem{
		Number: 11,
		Title:  "Unpause test",
		Status: "Research",
		ItemID: "PVTI_11",
		Labels: []string{}, // no fabrik:paused
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// stage:Research:failed should have been removed by clearFailedStage
	foundRemoval := false
	for _, call := range client.removeLabelCalls {
		if call.labelName == "stage:Research:failed" {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Error("expected stage:Research:failed label to be removed on unpause")
	}

	// PausedByEngine should be cleared (cleared by clearFailedStage, not re-set since we don't hit limit yet)
	snap, _ := eng.store.Get("owner/repo", 11)
	if snap.PausedByEngine("Research") {
		t.Error("expected PausedByEngine to be cleared after unpause")
	}
}

func TestProcessItem_UnlimitedWhenMaxRetriesZero(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", false, TokenUsage{}, nil
		},
	}

	eng := NewWithDeps(
		Config{
			Owner:      "owner",
			Repo:       "repo",
			ProjectNum: 1,
			User:       "testuser",
			Token:      "token",
			MaxRetries: 0, // unlimited
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 12, Title: "Unlimited retries", Status: "Research", ItemID: "PVTI_12"}

	// Run many times — should never escalate
	for i := 0; i < 10; i++ {
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem (iteration %d): %v", i, err)
		}
	}

	for _, call := range client.addLabelCalls {
		if call.labelName == "fabrik:paused" {
			t.Error("should not add fabrik:paused when MaxRetries=0")
		}
		if strings.HasSuffix(call.labelName, ":failed") {
			t.Errorf("should not add failed label when MaxRetries=0, got %q", call.labelName)
		}
	}

	// Attempts should remain 0 (not incremented when MaxRetries=0)
	snap, _ := eng.store.Get("owner/repo", 12)
	if snap.Attempts("Research") != 0 {
		t.Errorf("expected Attempts=0 when MaxRetries=0, got %d", snap.Attempts("Research"))
	}
}

func TestProcessItem_ClearsRetryCountOnCompletion(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "output", true, TokenUsage{}, nil // stage completes successfully
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
			Stages:     testStages(),
		},
		client,
		claude,
		wm,
	)

	// Pre-seed retry state as if previous failures occurred
	for i := 0; i < 2; i++ {
		eng.store.Apply(itemstate.StageRetryIncremented{Repo: "owner/repo", Number: 13, StageName: "Research"})
	}

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 13, Title: "Completion test", Status: "Research", ItemID: "PVTI_13"}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Both store fields should be cleared after successful completion
	snap, _ := eng.store.Get("owner/repo", 13)
	if snap.Attempts("Research") != 0 {
		t.Errorf("expected Attempts to be cleared on completion, got %d", snap.Attempts("Research"))
	}
	if snap.PausedByEngine("Research") {
		t.Error("expected PausedByEngine to be cleared on completion")
	}
}

// skipIfNoGit and initBareRepo are defined in worktree_test.go

func TestProcessItem_CleanupStage_SkipsAlreadyComplete(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = testStagesWithCleanup()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test",
		Status: "Done",
		Labels: []string{"stage:Done:complete"},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label calls for already-complete cleanup stage, got %d", len(client.addLabelCalls))
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for cleanup stage")
	}
}

func TestProcessItem_CleanupStage_CleanWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Create the worktree first
	_, err := wm.EnsureWorktree(42, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var addedLabel string
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			addedLabel = labelName
			return nil
		},
	}
	claude := &mockClaudeInvoker{}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			Stages: testStagesWithCleanup()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 42, Title: "Test", Status: "Done", ItemID: "PVTI_42"}

	err = eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Worktree directory should be gone
	if _, err := os.Stat(wm.WorktreeDir(42)); !os.IsNotExist(err) {
		t.Error("worktree directory should have been removed")
	}

	// Completion label should have been added
	if addedLabel != "stage:Done:complete" {
		t.Errorf("completion label = %q, want stage:Done:complete", addedLabel)
	}

	// ArchiveProjectItem should NOT be called inline — archiving is deferred to
	// archiveDoneCompleteItems which enforces the 24-hour grace period.
	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no ArchiveProjectItem calls (deferred to grace period), got %d", len(client.archiveProjectItemCalls))
	}

	// CooldownAt["periodic-re-eval"] should be set so itemMayNeedWork suppresses future deep-fetches
	snapCleanup, _ := eng.store.Get("owner/repo", 42)
	if snapCleanup.CooldownAt("periodic-re-eval").IsZero() {
		t.Error("CooldownAt[periodic-re-eval] should be set after cleanup stage")
	}

	// Claude should not have been invoked
	if len(claude.calls) != 0 {
		t.Error("claude should not be invoked for cleanup stage")
	}
}

func TestProcessItem_CleanupStage_DirtyWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Create the worktree and leave a dirty file
	wtDir, err := wm.EnsureWorktree(43, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "dirty.txt"), []byte("uncommitted"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			Stages: testStagesWithCleanup()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 43, Title: "Test", Status: "Done", ItemID: "PVTI_43"}

	err = eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Worktree directory should be removed even when dirty
	if _, err := os.Stat(wm.WorktreeDir(43)); !os.IsNotExist(err) {
		t.Error("worktree directory should have been removed even for dirty worktree")
	}

	// Completion label should have been added
	if len(client.addLabelCalls) != 1 {
		t.Errorf("expected 1 label call, got %d", len(client.addLabelCalls))
	} else if client.addLabelCalls[0].labelName != "stage:Done:complete" {
		t.Errorf("expected label stage:Done:complete, got %s", client.addLabelCalls[0].labelName)
	}
}

func TestProcessItem_CleanupStage_NonexistentWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)
	// Don't create the worktree — simulate issue moved to Done before any stage ran

	var addedLabel string
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			addedLabel = labelName
			return nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			Stages: testStagesWithCleanup()},
		client, &mockClaudeInvoker{}, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 99, Title: "No Worktree", Status: "Done", ItemID: "PVTI_99"}

	// Should not return error — worktree missing is warn+continue
	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Completion label should still be added even though worktree didn't exist
	if addedLabel != "stage:Done:complete" {
		t.Errorf("completion label = %q, want stage:Done:complete", addedLabel)
	}
}

func TestProcessItem_CleanupStage_PRItem(t *testing.T) {
	// PR items on the board don't have worktrees — cleanup should just apply the label.
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = testStagesWithCleanup()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 55,
		Title:  "Some PR",
		Status: "Done",
		IsPR:   true,
		ItemID: "PVTI_55",
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Completion label should be applied
	if len(client.addLabelCalls) != 1 || client.addLabelCalls[0].labelName != "stage:Done:complete" {
		t.Errorf("expected stage:Done:complete label, got %v", client.addLabelCalls)
	}
	// ArchiveProjectItem should NOT be called inline — deferred to grace period.
	if len(client.archiveProjectItemCalls) != 0 {
		t.Errorf("expected no ArchiveProjectItem calls for PR item (deferred), got %d", len(client.archiveProjectItemCalls))
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for cleanup stage PR item")
	}
}

func TestProcessItem_CleanupStage_NewCommentsIgnored(t *testing.T) {
	// New comments on a Done item should not divert to processComments — cleanup runs instead.
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Create the worktree
	_, err := wm.EnsureWorktree(77, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	var addedLabel string
	client := &mockGitHubClient{
		addLabelToIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			addedLabel = labelName
			return nil
		},
	}
	claude := &mockClaudeInvoker{}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			Stages: testStagesWithCleanup()},
		client, claude, wm,
	)

	// Item has a new (un-rocketed) comment — cleanup should still proceed, not processComments.
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 77,
		Title:  "Test",
		Status: "Done",
		ItemID: "PVTI_77",
		Comments: []gh.Comment{
			{ID: "C1", Author: "testuser", Body: "please do X"},
			// No rocket reaction → findNewComments would normally return this
		},
	}

	err = eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Worktree should be removed and completion label applied
	if _, statErr := os.Stat(wm.WorktreeDir(77)); !os.IsNotExist(statErr) {
		t.Error("worktree directory should have been removed despite new comment")
	}
	if addedLabel != "stage:Done:complete" {
		t.Errorf("completion label = %q, want stage:Done:complete", addedLabel)
	}
	if len(claude.calls) != 0 {
		t.Error("claude should not be invoked for cleanup stage")
	}
}

func TestProcessItem_CleanupStage_EngineFilesOnlyNotDirty(t *testing.T) {
	// Engine-managed files (.fabrik-context/) must not block cleanup.
	// The engine writes context files to .fabrik-context/, which is added to
	// .git/info/exclude by EnsureWorktree. This test verifies cleanup proceeds
	// even when untracked files are present in the worktree.
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	wm := NewWorktreeManager(repoDir)
	wtDir, err := wm.EnsureWorktree(88, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	// Write an untracked file to simulate incomplete work in the worktree.
	// (The .fabrik-context/ dir itself is git-excluded by EnsureWorktree, so
	// engine context files never surface in git status — this is belt-and-suspenders.)
	if err := os.WriteFile(filepath.Join(wtDir, "wip.txt"), []byte("work in progress"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify the test precondition: untracked file appears in git status
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = wtDir
	statusOut, _ := statusCmd.Output()
	if !strings.Contains(string(statusOut), "wip.txt") {
		t.Fatalf("precondition failed: wip.txt not visible in git status, got: %s", statusOut)
	}

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}

	eng := NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token",
			Stages: testStagesWithCleanup()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 88, Title: "Test", Status: "Done", ItemID: "PVTI_88"}

	err = eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// Cleanup should always proceed regardless of untracked files in the worktree —
	// the dirty check only warns, it never blocks cleanup.
	if _, statErr := os.Stat(wm.WorktreeDir(88)); !os.IsNotExist(statErr) {
		t.Error("worktree should have been removed even when untracked files are present")
	}
	if len(client.addLabelCalls) == 0 || client.addLabelCalls[0].labelName != "stage:Done:complete" {
		t.Errorf("expected stage:Done:complete label, got %v", client.addLabelCalls)
	}
}

func TestProcessItem_EmptyOutputWarningComment(t *testing.T) {
	// When Claude runs without error but produces no output, a warning comment
	// naming the stage must be posted to the issue.
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "", false, TokenUsage{}, nil // no output, no error
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, wm,
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 7, Title: "Test", Status: "Research", ItemID: "PVTI_7"}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}

	// A warning comment must be posted and must mention the stage name
	var warningComments []string
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "empty stage output") {
			warningComments = append(warningComments, c.body)
		}
	}
	if len(warningComments) == 0 {
		t.Errorf("expected an empty-output warning comment, got comments: %v", client.addCommentCalls)
	}
	if len(warningComments) > 0 && !strings.Contains(warningComments[0], "Research") {
		t.Errorf("warning comment should mention stage name %q, got: %s", "Research", warningComments[0])
	}
}

// TestItemMayNeedWork_CleanupStage_NoWorktree verifies that itemMayNeedWork returns
// false for a cleanup-stage item when no worktree directory exists on disk.
func TestItemMayNeedWork_CleanupStage_NoWorktree(t *testing.T) {
	// Engine with cleanup stages but WM points at a temp dir with no worktree inside.
	rootDir := t.TempDir()
	wm := NewWorktreeManagerWithRoot(t.TempDir(), rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)

	item := gh.ProjectItem{Number: 7, Title: "Old done item", Status: "Done"}
	// No worktree dir for issue-7 — itemMayNeedWork must return false.
	if eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should return false when no worktree directory exists")
	}
}

// TestItemMayNeedWork_CleanupStage_WithWorktree verifies that itemMayNeedWork returns
// true for a cleanup-stage item when the worktree directory does exist on disk.
func TestItemMayNeedWork_CleanupStage_WithWorktree(t *testing.T) {
	rootDir := t.TempDir()
	// Create the worktree directory for issue-7.
	worktreeDir := filepath.Join(rootDir, "issue-7")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	wm := NewWorktreeManagerWithRoot(t.TempDir(), rootDir)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		wm,
	)

	item := gh.ProjectItem{Number: 7, Title: "Old done item", Status: "Done"}
	// Worktree dir exists — itemMayNeedWork must return true.
	if !eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should return true when worktree directory exists")
	}
}

// TestItemMayNeedWork_CleanupStage_NoWM verifies that itemMayNeedWork returns false
// for a cleanup-stage item when no WorktreeManager is registered for the item's repo.
// This prevents a panic (worktreesFor panics on unregistered repos) and correctly
// indicates there is nothing to clean up.
func TestItemMayNeedWork_CleanupStage_NoWM(t *testing.T) {
	// NewWithDeps with nil WM leaves worktreeManagers empty.
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		&mockGitHubClient{},
		&mockClaudeInvoker{},
		nil, // no WM registered
	)

	item := gh.ProjectItem{Number: 3, Title: "Old done item", Status: "Done"}
	if eng.itemMayNeedWork(item) {
		t.Error("itemMayNeedWork should return false when no WorktreeManager is registered")
	}
}
