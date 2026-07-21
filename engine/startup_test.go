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
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			return false, nil
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
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			return true, nil
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

func TestCheckAllowAutoMerge_APIErrorIsNonFatal(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	client := &mockGitHubClient{
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			return false, errors.New("network error")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Should not panic; engine should log the error at warn level and continue.
	out := captureStdout(func() {
		eng.checkAllowAutoMerge("owner", "repo")
	})

	// No WARNING block should be emitted for an API error.
	if strings.Contains(out, "WARNING") {
		t.Errorf("should not print WARNING on API error; got: %q", out)
	}
}

func TestCheckAllowAutoMerge_DedupSuppressesSecondCall(t *testing.T) {
	warnings.WarningsPathOverride = filepath.Join(t.TempDir(), "warnings.json")
	t.Cleanup(func() { warnings.WarningsPathOverride = "" })
	var callCount int
	client := &mockGitHubClient{
		fetchAllowAutoMergeFn: func(owner, repo string) (bool, error) {
			callCount++
			return false, nil
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
