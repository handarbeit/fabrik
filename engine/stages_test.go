package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// testEngineForMerge returns a minimal engine wired for attemptMergeOnValidate tests.
func testEngineForMerge(client *mockGitHubClient) *Engine {
	stgs := testStagesWithValidate()
	return testEngineWithStages(client, stgs)
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
	eng := testEngineForMerge(client)
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
	eng := testEngineForMerge(client)
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
	eng := testEngineForMerge(client)
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
	eng := testEngineForMerge(client)
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
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	_, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when FetchLinkedPR fails, got nil (would incorrectly allow advancement)")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called on FetchLinkedPR error, got %d call(s)", len(client.enablePullRequestAutoMergeCalls))
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
	eng := testEngineWithStages(client, stgs)

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
	eng := testEngineWithStages(client, stgs)

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
