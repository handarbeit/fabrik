package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

// boardWithColumns returns a mock FetchProjectBoard func that returns a board
// with ProjectID "proj-1" and no items. Used by startup tests.
func boardWithColumns(projectID string) func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	return func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{ProjectID: projectID}, nil
	}
}

// statusFieldWithOptions returns a mock FetchStatusField func with the given
// column names (each gets a synthetic option ID).
func statusFieldWithOptions(names ...string) func(projectID string) (*gh.StatusField, error) {
	return func(projectID string) (*gh.StatusField, error) {
		opts := make(map[string]string, len(names))
		for i, n := range names {
			opts[n] = fmt.Sprintf("opt-%d", i)
		}
		return &gh.StatusField{FieldID: "field-1", Options: opts}, nil
	}
}

func TestCheckStageColumnAlignment_AllMatch(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement"),
	}
	e := testEngine(client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestCheckStageColumnAlignment_MissingStage(t *testing.T) {
	// Board only has Research and Plan — Implement is missing.
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan"),
	}
	e := testEngine(client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err == nil {
		t.Fatal("expected error for missing stage, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention mismatch, got: %v", err)
	}
}

func TestCheckStageColumnAlignment_ExtraColumns(t *testing.T) {
	// Board has all stages plus extra columns (Triage, Backlog).
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement", "Triage", "Backlog"),
	}
	var logged []string
	e := testEngine(client, &mockClaudeInvoker{})
	// Capture logf output by overriding the events channel (nil = direct print).
	// We can't easily intercept logf in plain-text mode, so just verify no error.
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("extra board columns should not be fatal, got: %v", err)
	}
	_ = logged // checked via no-error
}

func TestCheckStageColumnAlignment_FetchBoardError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return nil, fmt.Errorf("network timeout")
		},
	}
	e := testEngine(client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("FetchProjectBoard error should be non-fatal, got: %v", err)
	}
}

func TestCheckStageColumnAlignment_FetchStatusFieldError(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn: func(projectID string) (*gh.StatusField, error) {
			return nil, fmt.Errorf("project %q has no Status field", projectID)
		},
	}
	e := testEngine(client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("FetchStatusField error should be non-fatal, got: %v", err)
	}
}

func TestCheckStageColumnAlignment_CleanupStageExcluded(t *testing.T) {
	// Board has Research, Plan, Implement but NOT Done (cleanup stage) — should succeed.
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(),
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager("/tmp/test-repo"),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("cleanup stage missing from board should not be fatal, got: %v", err)
	}
}

func TestCheckStageColumnAlignment_PopulatesStatusField(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement"),
	}
	e := testEngine(client, &mockClaudeInvoker{})

	if e.statusField != nil {
		t.Fatal("statusField should be nil before check")
	}
	if err := e.checkStageColumnAlignment(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	e.mu.Lock()
	sf := e.statusField
	e.mu.Unlock()
	if sf == nil {
		t.Fatal("statusField should be populated after successful check")
	}
	if len(sf.Options) != 3 {
		t.Errorf("expected 3 options, got %d", len(sf.Options))
	}
}

func TestCheckStageColumnAlignment_CaseSensitive(t *testing.T) {
	// "research" (lowercase) should NOT match "Research" stage.
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("research", "plan", "implement"),
	}
	e := testEngine(client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err == nil {
		t.Fatal("case-sensitive mismatch should be fatal, got nil")
	}
}

func TestCheckStageColumnAlignment_EmptyProjectID(t *testing.T) {
	// Board returns empty ProjectID — should warn and skip gracefully.
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: ""}, nil
		},
	}
	e := testEngine(client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("empty ProjectID should be non-fatal, got: %v", err)
	}
}

// Ensure testStagesWithCleanup has 4 entries (3 normal + 1 cleanup).
func TestTestStagesWithCleanup(t *testing.T) {
	ss := testStagesWithCleanup()
	if len(ss) != 4 {
		t.Fatalf("expected 4 stages, got %d", len(ss))
	}
	var cleanupCount int
	for _, s := range ss {
		if s.CleanupWorktree {
			cleanupCount++
		}
	}
	if cleanupCount != 1 {
		t.Errorf("expected 1 cleanup stage, got %d", cleanupCount)
	}
}

// Verify that a stage with only cleanup stages in config and empty board passes.
func TestCheckStageColumnAlignment_OnlyCleanupStages(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions(),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			MaxConcurrent: 1,
			Stages: []*stages.Stage{
				{Name: "Done", Order: 99, CleanupWorktree: true},
			},
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager("/tmp/test-repo"),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("all-cleanup config should not be fatal, got: %v", err)
	}
}
