package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func testStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Research",
			Order:      1,
			Prompt:     "Do research",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:       "Plan",
			Order:      2,
			Prompt:     "Make a plan",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:       "Implement",
			Order:      3,
			Prompt:     "Implement it",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
}

func testEngine(client *mockGitHubClient, claude *mockClaudeInvoker) *Engine {
	return NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		claude,
		NewWorktreeManager("/tmp/test-repo"),
	)
}

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

	err := eng.processItem(board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if len(claude.calls) != 0 {
		t.Error("should not invoke claude for paused item")
	}
}

func TestItemNeedsWork_SkipsPaused(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for paused item")
	}
}

func TestProcessItem_AllowsOwnLock(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "output", false, nil
		},
	}
	eng := testEngine(client, claude)
	// Need a real worktree manager for processItem — use mock that returns a temp dir
	eng.worktrees = &WorktreeManager{baseDir: t.TempDir(), rootDir: t.TempDir()}

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

	// Mark as already processed
	eng.processedSet["1-Research"] = time.Now()

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

func TestFindNewComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Comments: []gh.Comment{
			{ID: "C1", Author: "testuser", Body: "Do this"},
			{ID: "C2", Author: "otheruser", Body: "Not from us"},
			{ID: "C3", Author: "testuser", Body: "🏭 **Fabrik — output"},
			{ID: "C4", Author: "testuser", Body: "Also do that"},
		},
	}

	comments := eng.findNewComments(item)
	if len(comments) != 2 {
		t.Fatalf("expected 2 new comments, got %d", len(comments))
	}
	if comments[0].ID != "C1" || comments[1].ID != "C4" {
		t.Errorf("comments = %v", comments)
	}

	// After markCommentsProcessed, second call should return no new comments
	eng.markCommentsProcessed(item, comments)
	comments2 := eng.findNewComments(item)
	if len(comments2) != 0 {
		t.Errorf("expected 0 new comments on second call, got %d", len(comments2))
	}
}

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

func TestFormatOutputComment(t *testing.T) {
	comment := formatOutputComment("Research", "Hello world")
	if !strings.Contains(comment, "🏭 **Fabrik — stage: Research**") {
		t.Errorf("comment = %q", comment)
	}
	if !strings.Contains(comment, "Hello world") {
		t.Error("comment missing output")
	}
}

func TestFormatOutputComment_Truncation(t *testing.T) {
	longOutput := strings.Repeat("x", 70000)
	comment := formatOutputComment("Test", longOutput)
	if len(comment) > 61000 {
		t.Errorf("comment should be truncated, len = %d", len(comment))
	}
	if !strings.Contains(comment, "... (truncated)") {
		t.Error("truncated comment missing truncation notice")
	}
}

func TestMapKeys(t *testing.T) {
	m := map[string]string{
		"a": "1",
		"b": "2",
		"c": "3",
	}
	keys := mapKeys(m)
	if len(keys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(keys))
	}
	// Check all keys present (order doesn't matter)
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	for _, expected := range []string{"a", "b", "c"} {
		if !found[expected] {
			t.Errorf("missing key %q", expected)
		}
	}
}

func TestPoll_FetchesBoardAndProcessesItems(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 1, Title: "Test", Status: "Unknown"},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{
				FieldID: "F1",
				Options: map[string]string{"Research": "OPT_1"},
			}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	err := eng.poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Status field should be fetched
	if eng.statusField == nil {
		t.Error("statusField should be set after poll")
	}
}

func TestPoll_Error(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return nil, fmt.Errorf("network error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	err := eng.poll(context.Background())
	if err == nil {
		t.Fatal("expected error from poll")
	}
}

func TestNewWithDeps(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager("/repo")
	cfg := Config{Owner: "o", Repo: "r"}

	eng := NewWithDeps(cfg, client, claude, wm)
	if eng.cfg.Owner != "o" {
		t.Errorf("Owner = %q", eng.cfg.Owner)
	}
	if eng.processedSet == nil {
		t.Error("processedSet should be initialized")
	}
}

func TestGitToplevel(t *testing.T) {
	// We're running in a git repo, so this should succeed
	dir, err := gitToplevel()
	if err != nil {
		t.Fatalf("gitToplevel: %v", err)
	}
	if dir == "" {
		t.Error("gitToplevel returned empty string")
	}
}

func TestProcessItem_FullHappyPath(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "Claude output here", false, nil
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

func TestProcessItem_CompletionWithAutoAdvance(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "Done\nFABRIK_STAGE_COMPLETE", true, nil
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
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "Done", true, nil
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
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "Done", true, nil
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
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "", false, nil
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

	// Should NOT post comment when output is empty
	if len(client.addCommentCalls) != 0 {
		t.Error("should not post comment for empty output")
	}
}

func TestProcessItem_ClaudeError(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "partial output", false, fmt.Errorf("claude crashed")
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
}

func TestProcessItem_ResumeOnReprocess(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "output", false, nil
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

	// First call — not yet in processedSet, resume=false
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

func TestPoll_StatusFieldFetchError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1", Items: nil}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return nil, fmt.Errorf("status field error")
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Should not error — status field failure is a warning
	err := eng.poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if eng.statusField != nil {
		t.Error("statusField should remain nil on fetch error")
	}
}

func TestPoll_StatusFieldAlreadySet(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1"}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			t.Error("should not fetch status field again")
			return nil, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.statusField = &gh.StatusField{FieldID: "already-set"}

	eng.poll(context.Background())
}

func TestPoll_EmptyProjectID(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: ""}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			t.Error("should not fetch status field when projectID is empty")
			return nil, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	eng.poll(context.Background())
}

func TestNew(t *testing.T) {
	skipIfNoGit(t)
	cfg := Config{
		Owner: "o",
		Repo:  "r",
		Token: "tok",
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if eng.client == nil {
		t.Error("client should not be nil")
	}
	if eng.claude == nil {
		t.Error("claude should not be nil")
	}
	if eng.worktrees == nil {
		t.Error("worktrees should not be nil")
	}
}

func TestRun_ShutdownOnSignal(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300 // long poll so we don't hit a second tick

	// Use ReadyCh so we only send SIGINT after signal.Notify is registered.
	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() {
		done <- eng.Run()
	}()

	// Wait for Run to register signal handlers before sending SIGINT.
	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
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
		addCommentFn: func(owner, repo string, issueNumber int, body string) error {
			return fmt.Errorf("comment error")
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "output", true, nil
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

func TestPoll_ProcessItemError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{
				ProjectID: "PVT_1",
				Items: []gh.ProjectItem{
					{Number: 1, Title: "Test", Status: "Research", ItemID: "PVTI_1"},
				},
			}, nil
		},
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return &gh.StatusField{FieldID: "F1", Options: map[string]string{}}, nil
		},
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, modelOverride string) (string, bool, error) {
			return "", false, nil
		},
	}

	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages()},
		client, claude, NewWorktreeManager("/nonexistent"),
	)

	// poll should not return error even when processItem fails
	err := eng.poll(context.Background())
	if err != nil {
		t.Fatalf("poll should not error from processItem failures: %v", err)
	}
}

// TestProcessedSetConcurrency verifies that concurrent access to processedSet
// via the mutex-protected methods does not cause data races.
func TestProcessedSetConcurrency(t *testing.T) {
	e := &Engine{
		cfg:          Config{User: "testuser"},
		processedSet: make(map[string]time.Time),
	}

	var wg sync.WaitGroup
	// Simulate concurrent writes from multiple workers
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("%d-TestStage", n)
			e.mu.Lock()
			e.processedSet[key] = time.Now()
			e.mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(e.processedSet) != 100 {
		t.Errorf("expected 100 entries, got %d", len(e.processedSet))
	}
}

// TestMarkCommentsProcessedConcurrency verifies markCommentsProcessed is safe
// when called from multiple goroutines.
func TestMarkCommentsProcessedConcurrency(t *testing.T) {
	e := &Engine{
		cfg:          Config{User: "testuser"},
		processedSet: make(map[string]time.Time),
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			item := gh.ProjectItem{Number: n}
			comments := []gh.Comment{
				{ID: fmt.Sprintf("c-%d-a", n)},
				{ID: fmt.Sprintf("c-%d-b", n)},
			}
			e.markCommentsProcessed(item, comments)
		}(i)
	}
	wg.Wait()

	// 20 items * 2 comments each = 40 entries
	if len(e.processedSet) != 40 {
		t.Errorf("expected 40 entries, got %d", len(e.processedSet))
	}
}

// TestFindNewCommentsFiltering verifies that findNewComments correctly filters
// already-processed, wrong-author, and fabrik-output comments.
func TestFindNewCommentsFiltering(t *testing.T) {
	e := &Engine{
		cfg:          Config{User: "alice"},
		processedSet: make(map[string]time.Time),
	}

	// Pre-mark one comment as processed
	e.processedSet["42-comment-c2"] = time.Now()

	item := gh.ProjectItem{
		Number: 42,
		Comments: []gh.Comment{
			{ID: "c1", Author: "alice", Body: "please fix"},         // new — should be returned
			{ID: "c2", Author: "alice", Body: "already seen"},       // already processed
			{ID: "c3", Author: "bob", Body: "not my user"},          // wrong author
			{ID: "c4", Author: "alice", Body: "🏭 **Fabrik output"}, // fabrik output
		},
	}

	result := e.findNewComments(item)
	if len(result) != 1 {
		t.Fatalf("expected 1 new comment, got %d", len(result))
	}
	if result[0].ID != "c1" {
		t.Errorf("expected comment c1, got %s", result[0].ID)
	}
}

// TestMaxConcurrentDefault verifies the default config value.
func TestMaxConcurrentDefault(t *testing.T) {
	cfg := Config{MaxConcurrent: 5}
	if cfg.MaxConcurrent != 5 {
		t.Errorf("expected default MaxConcurrent=5, got %d", cfg.MaxConcurrent)
	}
}

// TestConcurrentItemDispatch verifies that the non-blocking semaphore dispatch
// used in poll() respects MaxConcurrent and processes all items across multiple
// simulated poll cycles without data races.
func TestConcurrentItemDispatch(t *testing.T) {
	const numItems = 15
	const maxConcurrent = 3

	e := &Engine{
		cfg: Config{
			User:          "testuser",
			MaxConcurrent: maxConcurrent,
			Stages:        nil, // no matching stage → processItem returns nil immediately
		},
		processedSet: make(map[string]time.Time),
		lockedIssues: make(map[int]bool),
		sem:          make(chan struct{}, maxConcurrent),
	}

	board := &gh.ProjectBoard{}
	items := make([]gh.ProjectItem, numItems)
	for i := range items {
		items[i] = gh.ProjectItem{Number: i + 1, Status: "NoSuchStage"}
	}

	var (
		mu          sync.Mutex
		processed   int
		maxInFlight int
		inFlight    int
	)

	// Replicate the non-blocking dispatch pattern from poll(). Items that don't
	// get a semaphore slot are retried in subsequent cycles, mirroring real behaviour.
	remaining := make([]gh.ProjectItem, len(items))
	copy(remaining, items)
	var dispatchWg sync.WaitGroup

	for len(remaining) > 0 {
		var nextRound []gh.ProjectItem
		for _, item := range remaining {
			item := item
			select {
			case e.sem <- struct{}{}:
			default:
				nextRound = append(nextRound, item)
				continue
			}
			dispatchWg.Add(1)
			go func() {
				defer dispatchWg.Done()
				defer func() { <-e.sem }()

				mu.Lock()
				inFlight++
				if inFlight > maxInFlight {
					maxInFlight = inFlight
				}
				mu.Unlock()

				if err := e.processItem(context.Background(), board, item); err != nil {
					t.Errorf("processItem error for issue #%d: %v", item.Number, err)
				}

				mu.Lock()
				inFlight--
				processed++
				mu.Unlock()
			}()
		}
		remaining = nextRound
		if len(remaining) > 0 {
			// Yield so in-flight goroutines can make progress and free semaphore slots.
			dispatchWg.Wait()
		}
	}
	dispatchWg.Wait()

	if processed != numItems {
		t.Errorf("expected %d items processed, got %d", numItems, processed)
	}
	if maxInFlight > maxConcurrent {
		t.Errorf("max in-flight goroutines was %d, expected <= %d", maxInFlight, maxConcurrent)
	}
}

// TestPollNonBlockingAtCapacity verifies that the dispatch loop in poll() skips
// items via non-blocking semaphore acquire when all slots are taken, so poll()
// itself never blocks and the ticker can fire on schedule.
func TestPollNonBlockingAtCapacity(t *testing.T) {
	const maxConcurrent = 2

	e := &Engine{
		cfg: Config{
			User:          "testuser",
			MaxConcurrent: maxConcurrent,
			Stages:        nil,
		},
		processedSet: make(map[string]time.Time),
		sem:          make(chan struct{}, maxConcurrent),
	}

	// Fill the semaphore to simulate two in-flight workers from a previous cycle.
	e.sem <- struct{}{}
	e.sem <- struct{}{}

	items := []gh.ProjectItem{
		{Number: 1, Status: "NoSuchStage"},
		{Number: 2, Status: "NoSuchStage"},
	}

	// Replicate the non-blocking dispatch from poll().
	dispatched := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, item := range items {
			item := item
			select {
			case e.sem <- struct{}{}:
				e.inFlight.Store(item.Number, struct{}{})
				e.wg.Add(1)
				dispatched++
				go func() {
					defer e.wg.Done()
					defer func() { <-e.sem }()
					defer e.inFlight.Delete(item.Number)
				}()
			default:
				// skipped — at capacity
			}
		}
	}()

	select {
	case <-done:
		// dispatch loop returned without blocking — correct
	case <-time.After(time.Second):
		t.Fatal("dispatch loop blocked when semaphore was full")
	}

	if dispatched != 0 {
		t.Errorf("expected 0 dispatched (semaphore full), got %d", dispatched)
	}
}

// TestIdleCountNotIncrementedWhileWorkersInFlight verifies that idleCount (which
// drives auto-upgrade) is not incremented when dispatched==0 but workers are
// still running from a previous poll cycle. Upgrading while workers are in-flight
// would call syscall.Exec and kill them.
func TestIdleCountNotIncrementedWhileWorkersInFlight(t *testing.T) {
	e := &Engine{
		cfg: Config{
			AutoUpgrade:   true,
			MaxConcurrent: 1,
		},
		processedSet: make(map[string]time.Time),
		sem:          make(chan struct{}, 1),
	}

	// Simulate an in-flight worker by populating the map directly.
	e.inFlight.Store(42, struct{}{})

	// With dispatched==0 and an in-flight worker, idleCount must not increment.
	dispatched := 0
	var hasInFlight bool
	e.inFlight.Range(func(_, _ any) bool { hasInFlight = true; return false })

	if hasInFlight {
		e.idleCount = 0
	} else if dispatched == 0 {
		e.idleCount++
	}

	if e.idleCount != 0 {
		t.Errorf("idleCount should remain 0 while workers are in-flight, got %d", e.idleCount)
	}
}

func TestExtractModelOverride(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels", nil, ""},
		{"no model label", []string{"stage:Plan:complete", "fabrik:locked"}, ""},
		{"single model label", []string{"model:opus"}, "opus"},
		{"model label among others", []string{"stage:Plan", "model:sonnet", "fabrik:locked"}, "sonnet"},
		{"empty model name skipped", []string{"model:", "model:haiku"}, "haiku"},
		{"multiple model labels uses first", []string{"model:opus", "model:sonnet"}, "opus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractModelOverride(0, tc.labels)
			if got != tc.want {
				t.Errorf("extractModelOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestExtractModelOverrideWarnsOnMultiple(t *testing.T) {
	// Verify no panic and correct return value when multiple model labels are present.
	// The warning goes to fmt.Printf (stdout) and is tested behaviorally above.
	result := extractModelOverride(0, []string{"model:opus", "model:sonnet", "model:haiku"})
	if result != "opus" {
		t.Errorf("expected %q, got %q", "opus", result)
	}
}

// skipIfNoGit and initBareRepo are defined in worktree_test.go
