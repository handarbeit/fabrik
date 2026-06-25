package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
		NewWorktreeManager(t.TempDir()),
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
	e := testEngine(t, client, &mockClaudeInvoker{})

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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
	e := testEngine(t, client, &mockClaudeInvoker{})
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
		NewWorktreeManager(t.TempDir()),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("all-cleanup config should not be fatal, got: %v", err)
	}
}

// drainLogEvents drains up to n events from ch, returning all LogEvent messages.
// Used by drift scan tests to capture log output without blocking.
func drainLogEvents(ch chan tui.Event, n int) []string {
	var msgs []string
	for i := 0; i < n; i++ {
		select {
		case ev := <-ch:
			if le, ok := ev.(tui.LogEvent); ok {
				msgs = append(msgs, le.Message)
			}
		default:
			return msgs
		}
	}
	return msgs
}

// boardWithItems returns a mock FetchProjectBoard func that returns a board with
// the given items.
func boardWithItems(projectID string, items ...gh.ProjectItem) func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
	return func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{ProjectID: projectID, Items: items}, nil
	}
}

// TestDriftScan_DriftedItem_WarnsOnce verifies that an item with a cleanup-stage
// complete label (stage:Done:complete) whose board column is NOT the cleanup column
// triggers exactly one "board drift detected" warning.
func TestDriftScan_DriftedItem_WarnsOnce(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithItems("proj-1",
			gh.ProjectItem{
				Number: 1,
				ItemID: "PVTI_1",
				Status: "Validate", // drifted: has stage:Done:complete but stuck at Validate
				Labels: []string{"stage:Done:complete"},
			},
		),
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	// Wire a buffered events channel to capture logf output.
	ch := make(chan tui.Event, 64)
	e.events = ch

	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("drift scan should be non-fatal, got: %v", err)
	}

	msgs := drainLogEvents(ch, 64)
	var driftWarnings int
	for _, m := range msgs {
		if strings.Contains(m, "board drift detected") {
			driftWarnings++
		}
	}
	if driftWarnings != 1 {
		t.Errorf("expected exactly 1 'board drift detected' warning, got %d; messages: %v", driftWarnings, msgs)
	}
}

// TestDriftScan_CleanBoard_NoWarning verifies that an item with stage:Done:complete
// already at the Done column does NOT trigger a drift warning.
func TestDriftScan_CleanBoard_NoWarning(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithItems("proj-1",
			gh.ProjectItem{
				Number: 2,
				ItemID: "PVTI_2",
				Status: "Done", // correctly at the cleanup column
				Labels: []string{"stage:Done:complete"},
			},
		),
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	ch := make(chan tui.Event, 64)
	e.events = ch

	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := drainLogEvents(ch, 64)
	for _, m := range msgs {
		if strings.Contains(m, "board drift detected") {
			t.Errorf("unexpected drift warning for clean board: %q", m)
		}
	}
}

// TestDriftScan_NonCleanupStage_NoWarning verifies that an item with a
// non-cleanup stage complete label (stage:Implement:complete) does NOT trigger
// a drift warning — only cleanup stages are scanned.
func TestDriftScan_NonCleanupStage_NoWarning(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithItems("proj-1",
			gh.ProjectItem{
				Number: 3,
				ItemID: "PVTI_3",
				Status: "Implement",
				Labels: []string{"stage:Implement:complete"},
			},
		),
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			MaxConcurrent: 1,
			Stages:        testStagesWithCleanup(),
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	ch := make(chan tui.Event, 64)
	e.events = ch

	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := drainLogEvents(ch, 64)
	for _, m := range msgs {
		if strings.Contains(m, "board drift detected") {
			t.Errorf("unexpected drift warning for non-cleanup stage: %q", m)
		}
	}
}

// testStagesWithQueued returns stages including a Queued holding stage, for merge-train tests.
func testStagesWithQueued() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research"},
		{Name: "Plan", Order: 2, Prompt: "plan"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "Queued", Order: 6, HoldingStage: true},
		{Name: "Done", Order: 99, CleanupWorktree: true},
	}
}

// TestCheckStageColumnAlignment_HoldingStageRequiredWhenMergeTrainOn verifies
// that a missing Queued column is a fatal error when merge_train is on.
func TestCheckStageColumnAlignment_HoldingStageRequiredWhenMergeTrainOn(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		// Board has all stages except Queued.
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement", "Validate"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithQueued(),
			MergeTrain:    "on",
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err == nil {
		t.Fatal("expected error for missing Queued column when merge_train: on, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention mismatch, got: %v", err)
	}
}

// TestCheckStageColumnAlignment_HoldingStageExcludedWhenMergeTrainOff verifies
// that a missing Queued column is not a fatal error when merge_train is off (default).
func TestCheckStageColumnAlignment_HoldingStageExcludedWhenMergeTrainOff(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		// Board does NOT have a Queued column — fine when merge_train is off.
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement", "Validate"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithQueued(),
			MergeTrain:    "off",
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("holding stage missing from board should not be fatal when merge_train: off, got: %v", err)
	}
}

// TestCheckStageColumnAlignment_HoldingStagePresent verifies that when merge_train: on
// and the Queued column exists on the board, startup succeeds.
func TestCheckStageColumnAlignment_HoldingStagePresent(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement", "Validate", "Queued"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithQueued(),
			MergeTrain:    "on",
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("startup should succeed when Queued column present: %v", err)
	}
}
