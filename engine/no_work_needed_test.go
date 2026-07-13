package engine

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestCheckNoWorkNeeded verifies marker detection for FABRIK_NO_WORK_NEEDED.
func TestCheckNoWorkNeeded(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"marker on its own line", "Some output\nFABRIK_NO_WORK_NEEDED\n", true},
		{"marker as last line no newline", "output\nFABRIK_NO_WORK_NEEDED", true},
		{"CRLF line ending", "output\r\nFABRIK_NO_WORK_NEEDED\r\n", true},
		{"marker followed by more lines", "FABRIK_NO_WORK_NEEDED\nmore output", true},
		{"not present", "Some output without marker", false},
		{"embedded in sentence", "Please output FABRIK_NO_WORK_NEEDED when done", false},
		{"in backticks", "`FABRIK_NO_WORK_NEEDED`", false},
		{"partial match", "FABRIK_NO_WORK_NEEDED_EXTRA", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckNoWorkNeeded(tc.output)
			if got != tc.want {
				t.Errorf("CheckNoWorkNeeded(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// testStagesWithValidateAndCleanup returns a stage set including Validate and a
// CleanupWorktree Done stage, suitable for SkipsIntermediateStages tests.
func testStagesWithValidateAndCleanup() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research"},
		{Name: "Plan", Order: 2, Prompt: "plan"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "Done", Order: 5, Prompt: "done", CleanupWorktree: true},
	}
}

// TestHandleNoWorkNeeded_MovesToDone verifies that handleNoWorkNeeded adds the
// emitting stage's completion label and moves the item to Done.
func TestHandleNoWorkNeeded_MovesToDone(t *testing.T) {
	client := &mockGitHubClient{}
	// testStagesWithCleanup: Research(1), Plan(2), Implement(3), Done(99, cleanup)
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	// Should add stage:Plan:complete label.
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Plan:complete" {
			found = true
		}
	}
	if !found {
		t.Error("expected stage:Plan:complete label to be added")
	}

	// Should call UpdateProjectItemStatus with the Done option.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected Done option ID, got %q", client.updateStatusCalls[0].optionID)
	}
	if client.updateStatusCalls[0].projectID != "PVT_1" {
		t.Errorf("expected projectID PVT_1, got %q", client.updateStatusCalls[0].projectID)
	}
}

// TestHandleNoWorkNeeded_NilStatusField logs warning and does not panic.
func TestHandleNoWorkNeeded_NilStatusField(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())
	eng.statusField = nil

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	// Should not panic.
	eng.handleNoWorkNeeded(board, item, stage)

	// Completion label still gets added before the nil check.
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Plan:complete" {
			found = true
		}
	}
	if !found {
		t.Error("expected stage:Plan:complete label even when statusField is nil")
	}

	// No status update should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update with nil statusField, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleNoWorkNeeded_NoDoneOption logs warning and does not panic.
func TestHandleNoWorkNeeded_NoDoneOption(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())
	delete(eng.statusField.Options, "Done")

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	// Should not panic.
	eng.handleNoWorkNeeded(board, item, stage)

	// No status update should happen.
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("expected no status update when Done option missing, got %d", len(client.updateStatusCalls))
	}
}

// TestHandleNoWorkNeeded_SkipsIntermediateStages verifies that all non-cleanup stages
// after the emitting stage receive a dummy completion label and a "skipped" comment,
// while the cleanup (Done) stage does not.
func TestHandleNoWorkNeeded_SkipsIntermediateStages(t *testing.T) {
	client := &mockGitHubClient{}
	// Stages: Research(1), Plan(2), Implement(3), Validate(4), Done(5, cleanup)
	eng := testEngineWithStages(t, client, testStagesWithValidateAndCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 7, ItemID: "PVTI_7"}
	// Plan emits FABRIK_NO_WORK_NEEDED; Implement (order 3) and Validate (order 4)
	// should be skipped; Done (order 5, CleanupWorktree) should NOT be skipped.
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	// Collect all labels added.
	labelSet := make(map[string]bool)
	for _, c := range client.addLabelCalls {
		labelSet[c.labelName] = true
	}

	// Emitting stage gets its completion label.
	if !labelSet["stage:Plan:complete"] {
		t.Error("expected stage:Plan:complete label")
	}
	// Implement and Validate get skipped labels.
	if !labelSet["stage:Implement:complete"] {
		t.Error("expected stage:Implement:complete skip label")
	}
	if !labelSet["stage:Validate:complete"] {
		t.Error("expected stage:Validate:complete skip label")
	}
	// Done must NOT get a skip label (it's the cleanup stage).
	if labelSet["stage:Done:complete"] {
		t.Error("expected no stage:Done:complete skip label (cleanup stage must be excluded)")
	}

	// Two "skipped" comments should be posted (one per skipped stage: Implement, Validate).
	if len(client.addCommentCalls) != 2 {
		t.Fatalf("expected 2 skipped comments, got %d", len(client.addCommentCalls))
	}
	for _, c := range client.addCommentCalls {
		if !strings.Contains(c.body, "FABRIK_NO_WORK_NEEDED emitted by Plan") {
			t.Errorf("expected comment to mention emitting stage, got: %q", c.body)
		}
	}

	// One status update to Done.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("expected Done option, got %q", client.updateStatusCalls[0].optionID)
	}
}

// TestHandleNoWorkNeeded_ClosesIssue verifies that handleNoWorkNeeded calls
// CloseIssue after successfully moving the item to Done, and that the
// ApplyIssueClosed write-through sets IsClosed in the cache.
func TestHandleNoWorkNeeded_ClosesIssue(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	// Wire up a live CacheImpl so the ApplyIssueClosed write-through is exercised.
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_5", ItemID: "PVTI_5", Number: 5, Repo: "owner/repo", Status: "Plan"},
		},
	})
	eng.readClient = cache

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	// CloseIssue must be called exactly once with the correct args.
	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected 1 CloseIssue call, got %d", len(client.closeIssueCalls))
	}
	call := client.closeIssueCalls[0]
	if call.owner != "owner" {
		t.Errorf("CloseIssue owner = %q, want %q", call.owner, "owner")
	}
	if call.repo != "repo" {
		t.Errorf("CloseIssue repo = %q, want %q", call.repo, "repo")
	}
	if call.issueNumber != 5 {
		t.Errorf("CloseIssue issueNumber = %d, want 5", call.issueNumber)
	}

	// ApplyIssueClosed write-through must have set IsClosed=true in the store.
	snap, err := eng.store.Get("owner/repo", 5)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !snap.IsClosed() {
		t.Error("want IsClosed=true in store after handleNoWorkNeeded")
	}
}

// TestHandleNoWorkNeeded_CloseIssueNotCalledOnStatusFailure verifies that
// CloseIssue is NOT called when UpdateProjectItemStatus fails — the issue
// has not reached Done and should not be closed.
func TestHandleNoWorkNeeded_CloseIssueNotCalledOnStatusFailure(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			return fmt.Errorf("status update failed")
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	if len(client.closeIssueCalls) != 0 {
		t.Errorf("expected no CloseIssue calls when status update fails, got %d", len(client.closeIssueCalls))
	}
}

// TestHandleNoWorkNeeded_ClearsAwaitingInput verifies that fabrik:awaiting-input is
// removed when handleNoWorkNeeded runs, covering the orphaned-label scenario.
func TestHandleNoWorkNeeded_ClearsAwaitingInput(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Labels: []string{"fabrik:awaiting-input"}}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	found := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-input" {
			found = true
			if c.issueNumber != 5 {
				t.Errorf("RemoveLabelFromIssue called with issueNumber %d, want 5", c.issueNumber)
			}
		}
	}
	if !found {
		t.Error("expected RemoveLabelFromIssue call for fabrik:awaiting-input, got none")
	}
}

// TestHandleNoWorkNeeded_DirectlyDoneNotNextStage verifies that the issue advances
// directly to Done, not to the next sequential stage.
func TestHandleNoWorkNeeded_DirectlyDoneNotNextStage(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 9, ItemID: "PVTI_9"}
	// Plan is order 2; advanceToNextStage would go to Implement (OPT_Implement).
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update, got %d", len(client.updateStatusCalls))
	}
	// Must be Done, not the next sequential stage.
	if client.updateStatusCalls[0].optionID != "OPT_Done" {
		t.Errorf("handleNoWorkNeeded must set status to Done, got %q", client.updateStatusCalls[0].optionID)
	}
}

// TestHandleNoWorkNeeded_AwaitingDoneMarkerWrittenFirst verifies that
// fabrik:awaiting-done is the very first AddLabelToIssue call in
// handleNoWorkNeeded — ahead of the awaiting-input clear and the completion
// label — so a fully-rate-limited invocation still leaves a durable trace (#981).
// It also verifies the marker is never removed when UpdateProjectItemStatus fails:
// clearNoWorkNeededMarker only runs after a fully successful settle pass.
func TestHandleNoWorkNeeded_AwaitingDoneMarkerWrittenFirst(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			return fmt.Errorf("rate limited")
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Labels: []string{"fabrik:awaiting-input"}}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.handleNoWorkNeeded(board, item, stage)

	if len(client.addLabelCalls) == 0 {
		t.Fatal("expected at least one AddLabelToIssue call")
	}
	if got := client.addLabelCalls[0].labelName; got != "fabrik:awaiting-done" {
		t.Errorf("first AddLabelToIssue call = %q, want fabrik:awaiting-done", got)
	}

	// The marker must remain present — clearNoWorkNeededMarker never ran because
	// the status update failed.
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-done" {
			t.Error("fabrik:awaiting-done must not be removed when the Done-move fails")
		}
	}
}

// TestSettleNoWorkNeeded_RetryThenSucceed drives the exact scenario required by
// the issue: settleNoWorkNeeded is called once with a failing
// UpdateProjectItemStatus (no CloseIssue, marker stays), then again — simulating a
// later poll with a fresh item snapshot reflecting what actually persisted from the
// first pass — with the mock now succeeding. The second call must complete the
// Done move, close the issue, and clear the marker, without re-posting any of the
// already-persisted skip labels/comments.
func TestSettleNoWorkNeeded_RetryThenSucceed(t *testing.T) {
	var statusShouldFail atomic.Bool
	statusShouldFail.Store(true)
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			if statusShouldFail.Load() {
				return fmt.Errorf("rate limited")
			}
			return nil
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Plan"}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	// First pass: status update fails.
	eng.settleNoWorkNeeded(board, item, stage)

	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 status update attempt on first pass, got %d", len(client.updateStatusCalls))
	}
	if len(client.closeIssueCalls) != 0 {
		t.Fatalf("expected no CloseIssue call on first pass, got %d", len(client.closeIssueCalls))
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-done" {
			t.Error("fabrik:awaiting-done must not be cleared after a failed first pass")
		}
	}

	// Build a fresh item snapshot reflecting what actually persisted from pass 1
	// (labels/comments added via the mock), the same way a real poll would refetch
	// full item state from GitHub.
	var labels []string
	for _, c := range client.addLabelCalls {
		labels = append(labels, c.labelName)
	}
	var comments []gh.Comment
	for _, c := range client.addCommentCalls {
		comments = append(comments, gh.Comment{Body: c.body})
	}
	retryItem := gh.ProjectItem{
		Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Plan",
		Labels: labels, Comments: comments,
	}

	// Second pass: mock now succeeds.
	statusShouldFail.Store(false)
	eng.settleNoWorkNeeded(board, retryItem, stage)

	if len(client.updateStatusCalls) != 2 {
		t.Fatalf("expected 2 total status update attempts, got %d", len(client.updateStatusCalls))
	}
	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected 1 CloseIssue call after successful retry, got %d", len(client.closeIssueCalls))
	}

	// The skip label/comment for Implement (the only intermediate stage between
	// Plan and the cleanup Done stage) must not be duplicated across both passes.
	implementLabelCount := 0
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Implement:complete" {
			implementLabelCount++
		}
	}
	if implementLabelCount != 1 {
		t.Errorf("expected exactly 1 stage:Implement:complete label across both passes (no duplication), got %d", implementLabelCount)
	}
	if len(client.addCommentCalls) != 1 {
		t.Errorf("expected exactly 1 skipped comment across both passes (no duplication), got %d", len(client.addCommentCalls))
	}

	// Marker must be cleared after the fully successful pass.
	markerCleared := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-done" {
			markerCleared = true
		}
	}
	if !markerCleared {
		t.Error("expected fabrik:awaiting-done to be removed after a fully successful settle pass")
	}
}

// TestRecordNoWorkNeededRetry_EscalatesAtMaxRetries verifies that repeated
// no-work-needed settle failures escalate (fabrik:paused added, fabrik:awaiting-done
// removed, explanatory comment posted) once MaxRetries is reached, instead of
// retrying forever.
func TestRecordNoWorkNeededRetry_EscalatesAtMaxRetries(t *testing.T) {
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			return fmt.Errorf("rate limited")
		},
	}
	eng := testEngineWithStages(t, client, testStagesWithCleanup())
	eng.cfg.MaxRetries = 2

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Plan",
		Labels: []string{"stage:Plan:complete", "stage:Implement:complete"},
		Comments: []gh.Comment{
			{Body: "🏭 **Fabrik — skipped: no work needed**\n\n_Skipped: no work needed (FABRIK_NO_WORK_NEEDED emitted by Plan)._"},
		},
	}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	// Drive MaxRetries settle failures.
	for i := 0; i < eng.cfg.MaxRetries; i++ {
		eng.settleNoWorkNeeded(board, item, stage)
	}

	pausedAdded := false
	markerRemoved := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			pausedAdded = true
		}
	}
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-done" {
			markerRemoved = true
		}
	}
	if !pausedAdded {
		t.Error("expected fabrik:paused to be added after MaxRetries settle failures")
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-done to be removed on escalation")
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected an explanatory escalation comment to be posted")
	}
}

// TestHandleNoWorkNeeded_NoDispatchWhileAwaitingDone drives the exact scenario
// required by the issue: a simulated Done-move failure after FABRIK_NO_WORK_NEEDED
// must result in no further stage dispatch, a successful retry on a later poll,
// and the issue closing without re-entering the pipeline.
func TestHandleNoWorkNeeded_NoDispatchWhileAwaitingDone(t *testing.T) {
	var statusShouldFail atomic.Bool
	statusShouldFail.Store(true)
	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(_, _, _, _ string) error {
			if statusShouldFail.Load() {
				return fmt.Errorf("rate limited")
			}
			return nil
		},
	}
	claude := &mockClaudeInvoker{}
	stgs := testStagesWithCleanup()
	eng := testEngineWithStages(t, client, stgs)
	eng.claude = claude

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Research"}
	stage := &stages.Stage{Name: "Research", Order: 1}

	// Research emits FABRIK_NO_WORK_NEEDED; the Done-move fails (rate limit).
	eng.handleNoWorkNeeded(board, item, stage)
	if len(client.closeIssueCalls) != 0 {
		t.Fatalf("expected no CloseIssue call while the Done-move is still failing, got %d", len(client.closeIssueCalls))
	}

	// Refetch: the marker is present, item.Status is unchanged (still "Research").
	var labels []string
	for _, c := range client.addLabelCalls {
		labels = append(labels, c.labelName)
	}
	var comments []gh.Comment
	for _, c := range client.addCommentCalls {
		comments = append(comments, gh.Comment{Body: c.body})
	}
	pendingItem := gh.ProjectItem{
		Number: 5, ItemID: "PVTI_5", Repo: "owner/repo", Status: "Research",
		Labels: labels, Comments: comments,
	}

	// Dispatch must be suppressed for every configured stage while the marker is
	// present, regardless of which column the item is (still) sitting in.
	for _, s := range stgs {
		if s.CleanupWorktree {
			continue
		}
		itemAtStage := pendingItem
		itemAtStage.Status = s.Name
		if eng.itemMayNeedWork(itemAtStage) {
			t.Errorf("itemMayNeedWork must return false for stage %q while fabrik:awaiting-done is present", s.Name)
		}
		if eng.itemNeedsWork(itemAtStage) {
			t.Errorf("itemNeedsWork must return false for stage %q while fabrik:awaiting-done is present", s.Name)
		}
	}

	// Later poll: the settle scan retries with the mock now succeeding.
	statusShouldFail.Store(false)
	retryStage := stages.FindStage(stgs, pendingItem.Status)
	if retryStage == nil {
		t.Fatal("expected to resolve the emitting stage from item.Status on retry")
	}
	eng.settleNoWorkNeeded(board, pendingItem, retryStage)

	if len(client.closeIssueCalls) != 1 {
		t.Fatalf("expected 1 CloseIssue call after the retried settle succeeds, got %d", len(client.closeIssueCalls))
	}
	if len(claude.calls) != 0 {
		t.Errorf("expected Claude to never be invoked for Plan/Implement/etc while the no-work-needed decision was pending, got %d invocation(s)", len(claude.calls))
	}

	markerRemoved := false
	for _, c := range client.removeLabelCalls {
		if c.labelName == "fabrik:awaiting-done" {
			markerRemoved = true
		}
	}
	if !markerRemoved {
		t.Error("expected fabrik:awaiting-done to be cleared once the issue reaches Done")
	}
}
