package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// testEngineForMerge returns a minimal engine wired for attemptMergeOnValidate tests.
func testEngineForMerge(t *testing.T, client *mockGitHubClient) *Engine {
	t.Helper()
	stgs := testStagesWithValidate()
	return testEngineWithStages(t, client, stgs)
}

// TestAttemptMergeOnValidate_YoloEnablesAutoMerge verifies that for a yolo item
// at Validate completion, EnablePullRequestAutoMerge is called (not MergePR),
// fabrik:auto-merge-enabled is applied, and (true, nil) is returned.
func TestAttemptMergeOnValidate_YoloEnablesAutoMerge(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected autoMergeEnabled=true, got false")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called in yolo auto-merge path, got %d call(s)", len(client.mergePRCalls))
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if client.enablePullRequestAutoMergeCalls[0].prNumber != 10 {
		t.Errorf("EnablePullRequestAutoMerge called with PR %d, want 10", client.enablePullRequestAutoMergeCalls[0].prNumber)
	}
	foundLabel := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Error("expected fabrik:auto-merge-enabled label to be applied")
	}
}

// TestAttemptMergeOnValidate_CruiseSkipsAutoMerge verifies that cruise > yolo:
// a cruise-labelled item returns (false, nil) without calling EnablePullRequestAutoMerge.
func TestAttemptMergeOnValidate_CruiseSkipsAutoMerge(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error for cruise item: %v", err)
	}
	if enabled {
		t.Error("expected autoMergeEnabled=false for cruise item")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called for cruise items, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_AlreadyLabeled_Idempotent verifies that when
// fabrik:auto-merge-enabled is already present, the function returns (true, nil)
// without calling EnablePullRequestAutoMerge a second time.
func TestAttemptMergeOnValidate_AlreadyLabeled_Idempotent(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:auto-merge-enabled"}}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error for already-labeled item: %v", err)
	}
	if !enabled {
		t.Error("expected autoMergeEnabled=true for already-labeled item")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called again for idempotency, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_NoPR_SkipsAutoMerge verifies that when no linked PR
// exists, the function returns (false, nil) without error.
func TestAttemptMergeOnValidate_NoPR_SkipsAutoMerge(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("expected nil when no PR, got %v", err)
	}
	if enabled {
		t.Error("expected autoMergeEnabled=false when no linked PR")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called when no PR, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_FetchLinkedPRError_ReturnsError verifies that a
// transient FetchLinkedPR API error returns (false, err) rather than (false, nil),
// preventing advancement past Validate without enabling auto-merge.
func TestAttemptMergeOnValidate_FetchLinkedPRError_ReturnsError(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errors.New("network error")
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	_, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when FetchLinkedPR fails, got nil (would incorrectly allow advancement)")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called on FetchLinkedPR error, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_FallsBackToDirectMergeWhenClean verifies that when
// EnablePullRequestAutoMerge returns ErrAutoMergeAlreadyClean, the function falls
// back to a direct MergePR call, applies fabrik:auto-merge-enabled, and returns (true, nil).
func TestAttemptMergeOnValidate_FallsBackToDirectMergeWhenClean(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha42"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return fmt.Errorf("%w: GraphQL error: Pull request is in clean status", gh.ErrAutoMergeAlreadyClean)
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected autoMergeEnabled=true for already-clean fallback")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once as fallback, got %d", len(client.mergePRCalls))
	}
	if client.mergePRCalls[0].prNumber != 42 {
		t.Errorf("MergePR called with PR %d, want 42", client.mergePRCalls[0].prNumber)
	}
	foundLabel := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Error("expected fabrik:auto-merge-enabled label to be applied after direct merge fallback")
	}
}

// TestAttemptMergeOnValidate_FallsBackToDirectMergeWhenUnstable verifies that when
// EnablePullRequestAutoMerge returns a non-sentinel error (e.g. UNSTABLE status),
// the function falls back to a direct MergePR call, applies fabrik:auto-merge-enabled,
// and returns (true, nil). This is AC#2.
func TestAttemptMergeOnValidate_FallsBackToDirectMergeWhenUnstable(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha42"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return errors.New("GraphQL error: Pull request is in unstable status")
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected autoMergeEnabled=true for unstable-status fallback")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once as fallback, got %d", len(client.mergePRCalls))
	}
	if client.mergePRCalls[0].prNumber != 42 {
		t.Errorf("MergePR called with PR %d, want 42", client.mergePRCalls[0].prNumber)
	}
	foundLabel := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Error("expected fabrik:auto-merge-enabled label to be applied after direct merge fallback")
	}
}

// TestAttemptMergeOnValidate_DirectMergeAlsoFails verifies that when
// EnablePullRequestAutoMerge returns an arbitrary error AND MergePR also fails
// (e.g. ErrNotMergeable from a DIRTY PR), the function returns (false, err) and
// does NOT apply the fabrik:auto-merge-enabled label. This is AC#3.
func TestAttemptMergeOnValidate_DirectMergeAlsoFails(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha42"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return errors.New("GraphQL error: Pull request is in unstable status")
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return gh.ErrNotMergeable
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when MergePR also fails, got nil")
	}
	if enabled {
		t.Fatal("expected autoMergeEnabled=false when both auto-merge and direct merge fail")
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once as fallback, got %d", len(client.mergePRCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			t.Error("fabrik:auto-merge-enabled label must NOT be applied when direct merge fails")
		}
	}
}

// TestHandleStageComplete_WaitForCI_SkipsMergeAndReturns verifies Approach A': when
// wait_for_ci is true, handleStageComplete adds fabrik:awaiting-ci, does NOT add
// stage:Validate:complete, and does NOT call attemptMergeOnValidate.
// The completion label is deferred to checkCIGate in the catch-up loop (ADR 032).
func TestHandleStageComplete_WaitForCI_SkipsMergeAndReturns(t *testing.T) {
	autoMergeCalled := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha8"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			autoMergeCalled = true
			return nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)

	tr := true
	validateStage := &stages.Stage{Name: "Validate", WaitForCI: &tr}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:yolo"}}

	eng.handleStageComplete(context.Background(), board, item, validateStage)

	if autoMergeCalled {
		t.Error("EnablePullRequestAutoMerge must not be called when wait_for_ci is true")
	}
	// Completion label must NOT be added — deferred to checkCIGate (ADR 032).
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must not be added by handleStageComplete when wait_for_ci: true")
		}
	}
	// fabrik:awaiting-ci must be added as the in-flight durable marker.
	foundCI := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			foundCI = true
		}
	}
	if !foundCI {
		t.Error("fabrik:awaiting-ci must be added when wait_for_ci: true")
	}
}

// ---------------------------------------------------------------------------
// Layer 0 write-through tests
// ---------------------------------------------------------------------------

// TestAdvanceToNextStage_WritesThrough_Cache verifies Layer 0: after a successful
// advanceToNextStage call, the in-memory cache reflects the new status immediately,
// without waiting for a Layer 2 sweep.
func TestAdvanceToNextStage_WritesThrough_Cache(t *testing.T) {
	const (
		repo      = "owner/repo"
		issueNum  = 42
		itemID    = "PVTI_42"
		projectID = "PID_1"
	)

	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, fieldID, optionID string) error {
			return nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)

	// Replace readClient with a CacheImpl bootstrapped with the test item in Research.
	cache := boardcache.NewCacheImpl(boardcache.NewGitHubAdapter(client), eng.store, func(format string, args ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: projectID,
		Items: []gh.ProjectItem{
			{
				ID:     "I_42",
				ItemID: itemID,
				Repo:   repo,
				Number: issueNum,
				Status: "Research",
			},
		},
	})
	eng.readClient = cache

	board := &gh.ProjectBoard{ProjectID: projectID}
	item := gh.ProjectItem{
		ID:     "I_42",
		ItemID: itemID,
		Repo:   repo,
		Number: issueNum,
		Status: "Research",
	}
	currentStage := stgs[0] // Research

	if err := eng.advanceToNextStage(board, item, currentStage); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}

	// The cache should immediately reflect the new status without Layer 2 sweep.
	gotID, ok := cache.GetItemID(boardcache.ItemKey(repo, issueNum))
	if !ok {
		t.Fatal("GetItemID returned !ok after advanceToNextStage")
	}
	if gotID != itemID {
		t.Errorf("item ID mismatch: want %q, got %q", itemID, gotID)
	}

	// Read status via the cache internals using GetItemID to confirm the key.
	key := boardcache.ItemKey(repo, issueNum)
	_ = key // key confirmed via GetItemID above

	// Use ApplyStatusBatch with a no-op to flush nothing; read via GetItemID side-channel.
	// Directly verify by checking that the cache returns the updated status through the
	// UpdateItemStatus path: the cache item should now have status "Plan".
	gotItems, err := cache.FetchProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	var found *gh.ProjectItem
	for i := range gotItems.Items {
		if gotItems.Items[i].Number == issueNum {
			found = &gotItems.Items[i]
			break
		}
	}
	if found == nil {
		t.Fatal("item not found in cache after advanceToNextStage")
	}
	if found.Status != "Plan" {
		t.Errorf("cache Status = %q after advanceToNextStage, want %q", found.Status, "Plan")
	}
}

// TestAttemptMergeOnValidate_EnqueueOnYoloWithQueueEnabled verifies that when
// IsMergeQueueEnabled is true and MergeQueue != "off", EnqueuePullRequest is called,
// MergePR is not called, fabrik:auto-merge-enabled is applied, and (true, nil) is returned.
func TestAttemptMergeOnValidate_EnqueueOnYoloWithQueueEnabled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "abc123", IsMergeQueueEnabled: true}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected autoMergeEnabled=true, got false")
	}
	if len(client.enqueuePullRequestCalls) != 1 {
		t.Fatalf("expected EnqueuePullRequest called once, got %d", len(client.enqueuePullRequestCalls))
	}
	if client.enqueuePullRequestCalls[0].prNumber != 10 {
		t.Errorf("EnqueuePullRequest called with PR %d, want 10", client.enqueuePullRequestCalls[0].prNumber)
	}
	if client.enqueuePullRequestCalls[0].expectedHeadOID != "abc123" {
		t.Errorf("EnqueuePullRequest called with head OID %q, want %q", client.enqueuePullRequestCalls[0].expectedHeadOID, "abc123")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called in enqueue path, got %d call(s)", len(client.mergePRCalls))
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called in enqueue path, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
	}
	foundLabel := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Error("expected fabrik:auto-merge-enabled label to be applied")
	}
}

// TestAttemptMergeOnValidate_NoEnqueueWhenQueueNotEnabled verifies that when
// IsMergeQueueEnabled is false, the existing auto-merge path is taken (no enqueue call).
func TestAttemptMergeOnValidate_NoEnqueueWhenQueueNotEnabled(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1", IsMergeQueueEnabled: false}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected autoMergeEnabled=true via existing path")
	}
	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("EnqueuePullRequest must not be called when IsMergeQueueEnabled=false, got %d call(s)", len(client.enqueuePullRequestCalls))
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once (existing path), got %d", len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_CruiseDoesNotEnqueue verifies that a cruise-labeled item
// does not call EnqueuePullRequest or MergePR even when IsMergeQueueEnabled is true.
func TestAttemptMergeOnValidate_CruiseDoesNotEnqueue(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1", IsMergeQueueEnabled: true}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error for cruise item: %v", err)
	}
	if enabled {
		t.Error("expected autoMergeEnabled=false for cruise item")
	}
	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("EnqueuePullRequest must not be called for cruise items, got %d call(s)", len(client.enqueuePullRequestCalls))
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called for cruise items, got %d call(s)", len(client.mergePRCalls))
	}
}

// TestAttemptMergeOnValidate_MergeQueueOffDoesNotEnqueue verifies that when
// MergeQueue == "off", the existing auto-merge path is taken even if IsMergeQueueEnabled is true.
func TestAttemptMergeOnValidate_MergeQueueOffDoesNotEnqueue(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1", IsMergeQueueEnabled: true}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeQueue = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected autoMergeEnabled=true via existing path when MergeQueue=off")
	}
	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("EnqueuePullRequest must not be called when MergeQueue=off, got %d call(s)", len(client.enqueuePullRequestCalls))
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once (existing path), got %d", len(client.enablePullRequestAutoMergeCalls))
	}
}

// testStagesWithValidateAndHolding returns stages including Validate and a holding
// stage named "BatchHold" (deliberately not "Queued") to verify behavior is driven
// by the HoldingStage field, not the column name.
func testStagesWithValidateAndHolding() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Research", Order: 1, Prompt: "research"},
		{Name: "Plan", Order: 2, Prompt: "plan"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
		{Name: "Validate", Order: 4, Prompt: "validate"},
		{Name: "BatchHold", Order: 6, HoldingStage: true},
		{Name: "Done", Order: 99, CleanupWorktree: true},
	}
}

// TestAttemptMergeOnValidate_MergeTrainOn_AdvancesToQueued verifies that when
// merge_train: on, a yolo Validate completion advances the item to the holding stage,
// adds stage:Validate:complete, and does NOT call auto-merge or enqueue.
func TestAttemptMergeOnValidate_MergeTrainOn_AdvancesToQueued(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), board, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Fatal("expected enabled=false when advancing to holding stage (not auto-merge path)")
	}

	// Must have called UpdateProjectItemStatus with the holding stage option ID.
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected 1 UpdateProjectItemStatus call, got %d", len(client.updateStatusCalls))
	}
	// The holding stage is named "BatchHold" (not "Queued") — option ID must match the
	// HoldingStage field, not a hardcoded name.
	if client.updateStatusCalls[0].optionID != "OPT_BatchHold" {
		t.Errorf("UpdateProjectItemStatus called with option %q, want %q",
			client.updateStatusCalls[0].optionID, "OPT_BatchHold")
	}

	// Must have added stage:Validate:complete.
	var foundCompleteLabel bool
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			foundCompleteLabel = true
		}
	}
	if !foundCompleteLabel {
		t.Error("expected stage:Validate:complete label to be added")
	}

	// Must NOT have enabled auto-merge or enqueued.
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called when merge_train: on, got %d call(s)",
			len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("EnqueuePullRequest must not be called when merge_train: on, got %d call(s)",
			len(client.enqueuePullRequestCalls))
	}
}

// TestAttemptMergeOnValidate_MergeTrainOff_UsesExistingPath verifies that when
// merge_train: off (default), the existing auto-merge path runs unchanged.
func TestAttemptMergeOnValidate_MergeTrainOff_UsesExistingPath(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
	}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "off"
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), board, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true via existing auto-merge path when merge_train: off")
	}

	// Must NOT have moved the item to the holding stage.
	for _, c := range client.updateStatusCalls {
		if c.optionID == "OPT_BatchHold" {
			t.Errorf("UpdateProjectItemStatus must not target holding stage when merge_train: off")
		}
	}

	// Must have used the existing auto-merge path.
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once (existing path), got %d",
			len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_MergeTrainOn_CruiseBypasses verifies that cruise
// items are unaffected when merge_train: on — cruise early-return fires first.
func TestAttemptMergeOnValidate_MergeTrainOn_CruiseBypasses(t *testing.T) {
	client := &mockGitHubClient{}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"fabrik:cruise"}}

	enabled, err := eng.attemptMergeOnValidate(context.Background(), board, item, &stages.Stage{Name: "Validate"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Fatal("expected enabled=false for cruise item")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("cruise item must not be advanced to holding stage, got %d status update(s)", len(client.updateStatusCalls))
	}
}
