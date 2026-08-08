package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
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
	// Board has all stages plus extra columns (Triage, Backlog). Extra columns
	// are a warning, never fatal.
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement", "Triage", "Backlog"),
	}
	e := testEngine(t, client, &mockClaudeInvoker{})
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("extra board columns should not be fatal, got: %v", err)
	}
}

// TestCheckStageColumnAlignment_BacklogAndDoneNotExtra verifies that the
// "no matching stage" warning excludes the conventional unmanaged Backlog
// column and the Done column (which is backed by a cleanup stage), while still
// surfacing a genuinely unrecognized column.
func TestCheckStageColumnAlignment_BacklogAndDoneNotExtra(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement", "Done", "Backlog", "Triage"),
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
	events := make(chan tui.Event, 32)
	e.events = events

	if err := e.checkStageColumnAlignment(context.Background()); err != nil {
		t.Fatalf("extra columns should not be fatal, got: %v", err)
	}
	close(events)

	var extraWarn string
	for ev := range events {
		if le, ok := ev.(tui.LogEvent); ok && strings.Contains(le.Message, "no matching stage") {
			extraWarn = le.Message
		}
	}
	if extraWarn == "" {
		t.Fatal("expected a 'no matching stage' warning for the genuine extra column")
	}
	if !strings.Contains(extraWarn, "Triage") {
		t.Errorf("genuine extra column Triage should be warned, got: %q", extraWarn)
	}
	if strings.Contains(extraWarn, "Backlog") {
		t.Errorf("Backlog (unmanaged entry column) must not be flagged as extra, got: %q", extraWarn)
	}
	if strings.Contains(extraWarn, "Done") {
		t.Errorf("Done (backed by a cleanup stage) must not be flagged as extra, got: %q", extraWarn)
	}
}

// TestCheckStageColumnAlignment_UnmanagedStageRecognizedNoExtraWarning verifies
// that a board with a declared-unmanaged column produces no "no matching
// stage" warning for it, via declaration — not the hardcoded "Backlog" compat
// net. Uses "Icebox" (not "Backlog") so the assertion is actually load-bearing:
// with a literal "Backlog" column, engine/startup.go's hardcoded
// recognized["Backlog"] = true would keep this test green even if `unmanaged`
// were reverted entirely, since that column-name check doesn't consult
// Unmanaged either way. Naming the column something else exercises the real
// generalization this issue's Requirements calls for ("doesn't generalize to
// the other parking columns teams commonly add (Icebox, On Hold, Won't Do)").
func TestCheckStageColumnAlignment_UnmanagedStageRecognizedNoExtraWarning(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Icebox", "Research", "Plan", "Implement", "Done", "Triage"),
	}
	stagesCfg := append([]*stages.Stage{{
		Name:      "Icebox",
		Order:     -1,
		Unmanaged: true,
	}}, testStagesWithCleanup()...)
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        stagesCfg,
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	events := make(chan tui.Event, 32)
	e.events = events

	if err := e.checkStageColumnAlignment(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	close(events)

	var extraWarn string
	for ev := range events {
		if le, ok := ev.(tui.LogEvent); ok && strings.Contains(le.Message, "no matching stage") {
			extraWarn = le.Message
		}
	}
	if extraWarn == "" {
		t.Fatal("expected a 'no matching stage' warning for the genuine extra column Triage")
	}
	if !strings.Contains(extraWarn, "Triage") {
		t.Errorf("genuine extra column Triage should be warned, got: %q", extraWarn)
	}
	if strings.Contains(extraWarn, "Icebox") {
		t.Errorf("Icebox (declared unmanaged stage) must not be flagged as extra, got: %q", extraWarn)
	}
}

// TestCheckStageColumnAlignment_UnmanagedStageMissingColumnNotFatal verifies
// that a board WITHOUT a Backlog column, but with a configured unmanaged
// Backlog stage, does not fail startup (missing-column check exclusion).
func TestCheckStageColumnAlignment_UnmanagedStageMissingColumnNotFatal(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		// No "Backlog" column on the board at all.
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement", "Done"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithBacklog(),
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("unmanaged stage missing from board should not be fatal, got: %v", err)
	}
}

// TestCheckStageColumnAlignment_CompatNetWithoutBacklogYAML verifies that
// existing installs with NO local backlog.yaml (no configured Backlog stage
// at all) still produce no "no matching stage" warning for a Backlog board
// column, via the hardcoded "Backlog" compat net — while a genuine extra
// column is still warned.
func TestCheckStageColumnAlignment_CompatNetWithoutBacklogYAML(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Backlog", "Research", "Plan", "Implement", "Done", "Reviwe"),
	}
	e := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStagesWithCleanup(), // no Backlog stage configured
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	events := make(chan tui.Event, 32)
	e.events = events

	if err := e.checkStageColumnAlignment(context.Background()); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	close(events)

	var extraWarn string
	for ev := range events {
		if le, ok := ev.(tui.LogEvent); ok && strings.Contains(le.Message, "no matching stage") {
			extraWarn = le.Message
		}
	}
	if extraWarn == "" {
		t.Fatal("expected a 'no matching stage' warning for the genuine extra column Reviwe")
	}
	if !strings.Contains(extraWarn, "Reviwe") {
		t.Errorf("genuine extra/typo'd column Reviwe should be warned, got: %q", extraWarn)
	}
	if strings.Contains(extraWarn, "Backlog") {
		t.Errorf("Backlog must stay silent via the hardcoded compat net, got: %q", extraWarn)
	}
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

// TestCheckStageColumnAlignment_HoldingStageMissingMergeTrainOffWarnsButBoots
// verifies that a missing Queued column does NOT fail startup when
// merge_train is off/unset (default) — instead Fabrik boots and emits a
// startup warning naming the column and stating it must exist before
// merge_train can be enabled. This is the exact configuration produced by
// `fabrik init` (which extracts every embedded default stage, including
// queued.yaml, unconditionally) plus the default config — the majority of
// installs, not just ones that never intend to use the merge train — and it
// must not fail startup (per the severity-split correction to R2: fatal
// severity is keyed to reachability — merge_train: on — not to whether the
// stage is configured). Restoring an unconditional hard-fail (treating a
// missing holding-stage column as fatal regardless of merge_train's value)
// turns this test red.
func TestCheckStageColumnAlignment_HoldingStageMissingMergeTrainOffWarnsButBoots(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		// Board does NOT have a Queued column — this must not fail startup
		// while merge_train is off, since the column is unreachable by any
		// code path today (ADR-1072) and `fabrik init` writes queued.yaml
		// unconditionally, regardless of merge_train.
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Plan", "Implement", "Validate"),
	}
	events := make(chan tui.Event, 16)
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
	e.events = events
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("missing Queued column should not fail startup when merge_train: off, got: %v", err)
	}
	msgs := drainLogMessages(events)
	if !anyContains(msgs, `"Queued"`) {
		t.Errorf("expected a startup warning naming the missing Queued column, got: %v", msgs)
	}
	if !anyContains(msgs, "before enabling") {
		t.Errorf("expected the warning to state the column is required before enabling merge_train, got: %v", msgs)
	}
}

// TestCheckStageColumnAlignment_NonHoldingStageStillFatalWhenMergeTrainOff
// verifies the severity split is scoped to holding stages only: a missing
// non-holding, non-cleanup, non-unmanaged column (e.g. Plan) remains fatal
// regardless of merge_train's value, exactly as before this issue.
func TestCheckStageColumnAlignment_NonHoldingStageStillFatalWhenMergeTrainOff(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		// Plan is missing (non-holding) — fatal regardless of merge_train.
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Implement", "Validate", "Queued"),
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
	if err == nil {
		t.Fatal("expected error for missing Plan column, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention mismatch, got: %v", err)
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

// TestCheckStageColumnAlignment_HoldingStagePresentWhenMergeTrainOff mirrors
// TestCheckStageColumnAlignment_HoldingStagePresent but with merge_train: off —
// a board carrying every required option, including the holding stage's
// column, must still boot clean regardless of mode (Acceptance #5).
func TestCheckStageColumnAlignment_HoldingStagePresentWhenMergeTrainOff(t *testing.T) {
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
			MergeTrain:    "off",
		},
		client,
		&mockClaudeInvoker{},
		NewWorktreeManager(t.TempDir()),
	)
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("startup should succeed when Queued column present, merge_train off: %v", err)
	}
}

// TestCheckStageColumnAlignment_ErrorListsEveryRequiredStage verifies that the
// returned error enumerates every required Status option and whether it is
// present or missing — not just the first missing one (R3/Acceptance #3).
func TestCheckStageColumnAlignment_ErrorListsEveryRequiredStage(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		// Board is missing both Plan and Queued; Research, Implement, Validate present.
		fetchStatusFieldFn: statusFieldWithOptions("Research", "Implement", "Validate"),
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
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"Research (order 1): present",
		"Plan (order 2): MISSING",
		"Implement (order 3): present",
		"Validate (order 4): present",
		"Queued (order 6): MISSING",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should contain %q, got: %v", want, msg)
		}
	}
}

// TestCheckStageColumnAlignment_ErrorStatesHoldingColumnTerminalRole verifies
// that a missing holding-stage column's error text states its terminal-only
// role and does not attribute the requirement to merge_train mode (R4).
// TestCheckStageColumnAlignment_ErrorStatesHoldingColumnTerminalRole verifies
// that a missing holding-stage column's FATAL error text states its
// terminal-only role and does not attribute the requirement to merge_train
// mode (R4). Uses merge_train: on, the only mode in which a missing Queued
// column is fatal by itself under the severity split — the off/unset case is
// covered by TestCheckStageColumnAlignment_HoldingStageMissingMergeTrainOffWarnsButBoots
// below, which asserts the same R4 disambiguation appears in the deferred
// warning text instead.
func TestCheckStageColumnAlignment_ErrorStatesHoldingColumnTerminalRole(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement", "Validate"),
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
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "terminal-only") {
		t.Errorf("error should state the holding column's terminal-only role, got: %v", msg)
	}
	if !strings.Contains(msg, "never a column to move items into") {
		t.Errorf("error should disambiguate that the column is not a valid intake target, got: %v", msg)
	}
	if strings.Contains(msg, "required by merge_train: on") {
		t.Errorf("error should not attribute the requirement to merge_train mode, got: %v", msg)
	}
}

// TestCheckStageColumnAlignment_DeferredWarningStatesHoldingColumnTerminalRole
// verifies that the R4 disambiguation (terminal-only role, never a manual
// intake target) also appears in the deferred WARNING text emitted when
// merge_train is off/unset and the holding column is missing — not just in
// the fatal error text covered above.
func TestCheckStageColumnAlignment_DeferredWarningStatesHoldingColumnTerminalRole(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: boardWithColumns("proj-1"),
		fetchStatusFieldFn:  statusFieldWithOptions("Research", "Plan", "Implement", "Validate"),
	}
	events := make(chan tui.Event, 16)
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
	e.events = events
	err := e.checkStageColumnAlignment(context.Background())
	if err != nil {
		t.Fatalf("expected no error (deferred, not fatal), got: %v", err)
	}
	msgs := drainLogMessages(events)
	if !anyContains(msgs, "terminal-only") {
		t.Errorf("warning should state the holding column's terminal-only role, got: %v", msgs)
	}
	if !anyContains(msgs, "never a column to move") {
		t.Errorf("warning should disambiguate that the column is not a valid intake target, got: %v", msgs)
	}
}

// TestRunStartupTransientLabelScan_RemovesStaleLabelsFromClosedItems verifies
// that runStartupTransientLabelScan triggers label cleanup on closed store
// entries carrying transient lifecycle labels, without touching open items or
// clean closed items.
func TestRunStartupTransientLabelScan_RemovesStaleLabelsFromClosedItems(t *testing.T) {
	var removedLabels []string
	client := &mockGitHubClient{
		removeLabelFromIssueFn: func(owner, repo string, issueNumber int, labelName string) error {
			removedLabels = append(removedLabels, labelName)
			return nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Seed three items into the store:
	//   #1 — closed, carries a transient label → should be cleaned
	//   #2 — open, carries a transient label → must be skipped
	//   #3 — closed, no transient labels → must be skipped
	for _, pi := range []gh.ProjectItem{
		{ID: "I_001", Number: 1, Repo: "owner/repo", IsClosed: true, Labels: []string{"fabrik:awaiting-review", "stage:Review:complete"}},
		{ID: "I_002", Number: 2, Repo: "owner/repo", IsClosed: false, Labels: []string{"fabrik:awaiting-ci"}},
		{ID: "I_003", Number: 3, Repo: "owner/repo", IsClosed: true, Labels: []string{"stage:Validate:complete"}},
	} {
		eng.store.Apply(itemstate.IssueOpened{Item: pi})
		eng.store.Apply(itemstate.ItemDeepFetched{
			Repo:       pi.Repo,
			Number:     pi.Number,
			FreshState: pi,
		})
	}

	eng.runStartupTransientLabelScan()

	// Only the transient label from issue #1 should be removed.
	if len(removedLabels) == 0 {
		t.Fatal("expected RemoveLabelFromIssue called for closed item with stale transient label; got 0 calls")
	}
	found := false
	for _, l := range removedLabels {
		if l == "fabrik:awaiting-review" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'fabrik:awaiting-review' to be removed; removed labels: %v", removedLabels)
	}
}

func TestCheckAllowAutoMerge_DisabledEmitsWarning(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			return gh.RepoAccess{AllowAutoMerge: false, CanPush: true}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING in output; got: %q", out)
	}
	if !strings.Contains(out, "allow_auto_merge") {
		t.Errorf("expected allow_auto_merge mention in output; got: %q", out)
	}
	if !strings.Contains(out, "gh api -X PATCH repos/owner/repo") {
		t.Errorf("expected fix command in output; got: %q", out)
	}
}

func TestCheckAllowAutoMerge_EnabledIsSilent(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			return gh.RepoAccess{AllowAutoMerge: true, CanPush: true}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	if out != "" {
		t.Errorf("expected no output for enabled repo; got: %q", out)
	}
}

// TestCheckAllowAutoMerge_NoWriteAccessSkipsWarning verifies AC2/R2: a repo
// with no write access produces no allow_auto_merge warning even when
// AllowAutoMerge is false — the unfixable-without-admin-rights warning must
// not fire for a repo the operator can't administer at all.
func TestCheckAllowAutoMerge_NoWriteAccessSkipsWarning(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			return gh.RepoAccess{AllowAutoMerge: false, CanPush: false}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	if strings.Contains(out, "WARNING") {
		t.Errorf("expected no WARNING for a repo with no write access, even with allow_auto_merge disabled; got: %q", out)
	}
	if strings.Contains(out, "gh api -X PATCH") {
		t.Errorf("expected no admin-only remediation for a repo with no write access; got: %q", out)
	}
}

// TestCheckAllowAutoMerge_NoWriteAccessClearsStaleWarning verifies that a
// previously recorded allow_auto_merge warning is cleared, not left immortal,
// when a later run finds the repo's write access has been revoked. Without
// this, the !CanPush early return would skip past checkAllowAutoMerge's own
// Clear branch forever, since checkedAutoMergeRepos never lets this function
// re-run for the repo a second time in the same process (raised in PR review
// on #1347).
func TestCheckAllowAutoMerge_NoWriteAccessClearsStaleWarning(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })

	if err := warnings.Record(warnings.Entry{
		Key:    "allow_auto_merge:owner/repo",
		Type:   "allow_auto_merge",
		Title:  "allow_auto_merge disabled on owner/repo",
		Detail: "stale entry from a prior run when access was still present",
	}); err != nil {
		t.Fatalf("seeding stale warning: %v", err)
	}

	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			return gh.RepoAccess{AllowAutoMerge: false, CanPush: false}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	eng.checkAllowAutoMerge("owner", "repo")

	entries, err := warnings.Load()
	if err != nil {
		t.Fatalf("loading warnings: %v", err)
	}
	for _, e := range entries {
		if e.Key == "allow_auto_merge:owner/repo" {
			t.Errorf("expected stale allow_auto_merge warning to be cleared once access is revoked; still present: %+v", e)
		}
	}
}

func TestCheckAllowAutoMerge_APIErrorIsNonFatal(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			return gh.RepoAccess{}, errors.New("network error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Should not panic; engine should log the error at warn level and continue.
	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	// No WARNING block should be emitted for an API error. A probe error fails
	// open (CanPush: true, AllowAutoMerge: true), so no allow_auto_merge
	// warning fires either.
	if strings.Contains(out, "WARNING") {
		t.Errorf("should not print WARNING on API error; got: %q", out)
	}
}

func TestCheckAllowAutoMerge_DedupSuppressesSecondCall(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	var callCount int
	client := &mockGitHubClient{
		fetchRepoAccessFn: func(owner, repo string) (gh.RepoAccess, error) {
			callCount++
			return gh.RepoAccess{AllowAutoMerge: false, CanPush: true}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// First call should emit warning.
	out1 := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})
	if !strings.Contains(out1, "WARNING") {
		t.Errorf("first call: expected WARNING; got: %q", out1)
	}

	// Second call for the same repo should be a no-op.
	out2 := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})
	if out2 != "" {
		t.Errorf("second call: expected no output (dedup); got: %q", out2)
	}
	if callCount != 1 {
		t.Errorf("expected API to be called exactly once; got %d", callCount)
	}
}
