package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	_, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

// TestAttemptMergeOnValidate_DirectMergeFailsCINotGreen verifies that when the
// direct-merge fallback's MergePR call returns gh.ErrNotMergeableCI (issue
// #1094: MergePR now self-gates on mergeable_state), attemptMergeOnValidate
// returns (false, err) wrapping ErrNotMergeableCI — distinguishable from
// ErrNotMergeable — and, critically, does NOT apply fabrik:rebase-needed or
// fabrik:paused. A CI refusal must retry on the next poll (via full Validate
// re-dispatch, since handleStageComplete's caller returns without adding
// stage:Validate:complete on any mergeErr), not route into the rebase cycle.
func TestAttemptMergeOnValidate_DirectMergeFailsCINotGreen(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 42, HeadSHA: "sha42"}, nil
		},
		enablePullRequestAutoMergeFn: func(owner, repo string, prNumber int, strategy string) error {
			return errors.New("GraphQL error: Pull request is in unstable status")
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return fmt.Errorf("%w: mergeable_state=%q", gh.ErrNotMergeableCI, "blocked")
		},
	}
	eng := testEngineForMerge(t, client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when MergePR refuses on CI, got nil")
	}
	if !errors.Is(err, gh.ErrNotMergeableCI) {
		t.Errorf("expected errors.Is(err, gh.ErrNotMergeableCI), got: %v", err)
	}
	if errors.Is(err, gh.ErrNotMergeable) {
		t.Error("ErrNotMergeableCI must not also satisfy errors.Is(err, gh.ErrNotMergeable) — the two sentinels must stay distinguishable")
	}
	if enabled {
		t.Fatal("expected autoMergeEnabled=false when direct merge is refused on CI")
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:auto-merge-enabled" {
			t.Error("fabrik:auto-merge-enabled label must NOT be applied when direct merge is refused on CI")
		}
		if c.labelName == "fabrik:rebase-needed" {
			t.Error("fabrik:rebase-needed must NOT be applied for a CI refusal — that would incorrectly consume a rebase cycle")
		}
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be applied for a CI refusal — it should retry on the next poll")
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

// TestAdvanceToNextStage_EmptyItemRepo_CacheWriteThroughSucceeds is a regression
// test for issue #957's cache-key sweep: the Store is populated (via Reconcile/
// BootstrapFromProbe) under the resolved "owner/repo" key, but a caller's local
// gh.ProjectItem can have an empty Repo field (single-repo default setups never
// populate it on some code paths). Before the fix, advanceToNextStage's write-
// through used boardcache.ItemKey(item.Repo, item.Number) — with item.Repo=="",
// that resolves to a different, non-existent key ("#N") than the one the Store
// actually holds ("owner/repo#N"), so UpdateItemStatus silently no-ops via the
// phantom-key guard (no error, no log) until the next Reconcile repairs it.
func TestAdvanceToNextStage_EmptyItemRepo_CacheWriteThroughSucceeds(t *testing.T) {
	const (
		repo      = "owner/repo"
		issueNum  = 43
		itemID    = "PVTI_43"
		projectID = "PID_1"
	)

	client := &mockGitHubClient{
		updateProjectItemStatusFn: func(projectID, itemID, fieldID, optionID string) error {
			return nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs) // cfg.Owner="owner", cfg.Repo="repo"

	// The Store is bootstrapped with the item keyed under the resolved "owner/repo"
	// form, mirroring how a real deep-fetch/Reconcile populates it.
	cache := boardcache.NewCacheImpl(boardcache.NewGitHubAdapter(client), eng.store, func(format string, args ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: projectID,
		Items: []gh.ProjectItem{
			{
				ID:     "I_43",
				ItemID: itemID,
				Repo:   repo,
				Number: issueNum,
				Status: "Research",
			},
		},
	})
	eng.readClient = cache

	board := &gh.ProjectBoard{ProjectID: projectID}
	// The item handed to advanceToNextStage has an empty Repo field — the
	// mismatch this test exists to catch.
	item := gh.ProjectItem{
		ID:     "I_43",
		ItemID: itemID,
		Repo:   "",
		Number: issueNum,
		Status: "Research",
	}
	currentStage := stgs[0] // Research

	if err := eng.advanceToNextStage(board, item, currentStage); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}

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
		t.Errorf("cache Status = %q after advanceToNextStage with item.Repo=\"\", want %q "+
			"(write-through must not silently drop the update when item.Repo is empty)", found.Status, "Plan")
	}
}

// TestHandleStageComplete_EmptyItemRepo_CacheWriteThroughSucceeds is a regression
// test for issue #957's cache-key sweep, covering the ApplyLabelAdded write-through
// for the stage completion label rather than advanceToNextStage's UpdateItemStatus.
func TestHandleStageComplete_EmptyItemRepo_CacheWriteThroughSucceeds(t *testing.T) {
	const (
		repo     = "owner/repo"
		issueNum = 44
		itemID   = "PVTI_44"
	)

	client := &mockGitHubClient{}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs) // cfg.Owner="owner", cfg.Repo="repo"

	cache := boardcache.NewCacheImpl(boardcache.NewGitHubAdapter(client), eng.store, func(format string, args ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		Items: []gh.ProjectItem{
			{
				ID:     "I_44",
				ItemID: itemID,
				Repo:   repo,
				Number: issueNum,
				Status: "Research",
			},
		},
	})
	eng.readClient = cache

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		ID:     "I_44",
		ItemID: itemID,
		Repo:   "",
		Number: issueNum,
		Status: "Research",
	}
	stage := &stages.Stage{Name: "Research"}

	eng.handleStageComplete(context.Background(), board, item, stage)

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
		t.Fatal("item not found in cache after handleStageComplete")
	}
	wantLabel := "stage:Research:complete"
	if !hasLabel(found.Labels, wantLabel) {
		t.Errorf("cache Labels = %v after handleStageComplete with item.Repo=\"\", want to contain %q "+
			"(write-through must not silently drop the label when item.Repo is empty)", found.Labels, wantLabel)
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), board, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), board, item, &stages.Stage{Name: "Validate"})
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

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), board, item, &stages.Stage{Name: "Validate"})
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

// ── #1216: wait_for_reviews enforced at the landing decision ─────────────────
//
// The review gate used to be armed only by the catch-up loop's handleReviewGate,
// which no-ops while !hasComplete (#617) and is ordered ahead of the handler that
// clears CI — so it could never arm before attemptMergeOnValidate ran. These tests
// pin the gate at the landing decision itself, for both merge_train modes.

// TestAttemptMergeOnValidate_ReviewGate_BlocksAutoMerge verifies that with
// merge_train: off and an outstanding reviewer, no landing action is taken and
// fabrik:awaiting-review is applied (FR-1).
func TestAttemptMergeOnValidate_ReviewGate_BlocksAutoMerge(t *testing.T) {
	waitTrue := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return []gh.ReviewRequest{{Login: "verveguy"}}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", LinkedPRNumber: 10}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue})
	if err != nil {
		t.Fatalf("expected (false, false, nil) when the review gate blocks, got err %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when the review gate blocks")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called while reviewers are outstanding, got %d call(s)",
			len(client.enablePullRequestAutoMergeCalls))
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("MergePR must not be called while reviewers are outstanding, got %d call(s)", len(client.mergePRCalls))
	}
	if len(client.enqueuePullRequestCalls) != 0 {
		t.Errorf("EnqueuePullRequest must not be called while reviewers are outstanding, got %d call(s)",
			len(client.enqueuePullRequestCalls))
	}
	if !hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("expected fabrik:awaiting-review to be applied when the landing gate blocks")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_BlocksAdvanceToQueued is the FR-3 twin of
// the test above: under merge_train: on, an outstanding reviewer must also prevent
// the advance to the holding column. Turning the train on must not weaken the gate.
func TestAttemptMergeOnValidate_ReviewGate_BlocksAdvanceToQueued(t *testing.T) {
	waitTrue := true
	client := &mockGitHubClient{
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return []gh.ReviewRequest{{Login: "verveguy"}}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, nil
		},
	}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", LinkedPRNumber: 10}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), board, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue})
	if err != nil {
		t.Fatalf("expected (false, false, nil) when the review gate blocks, got err %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when the review gate blocks")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("item must not advance to the holding column while reviewers are outstanding, got %d status update(s)",
			len(client.updateStatusCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "stage:Validate:complete" {
			t.Error("stage:Validate:complete must not be added while the review gate blocks")
		}
	}
	if !hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("expected fabrik:awaiting-review to be applied when the landing gate blocks")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_ClearedProceeds verifies the gate opens
// once no reviewers are outstanding and at least one review has been submitted —
// the landing decision then runs exactly as before.
func TestAttemptMergeOnValidate_ReviewGate_ClearedProceeds(t *testing.T) {
	waitTrue := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return nil, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return []gh.PRReview{{State: "APPROVED"}}, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", LinkedPRNumber: 10}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true once the review gate has cleared")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 1 {
		t.Fatalf("expected EnablePullRequestAutoMerge called once, got %d", len(client.enablePullRequestAutoMergeCalls))
	}
	if hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("fabrik:awaiting-review must not be applied when the gate is clear")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_ResolvesPRViaFallback covers the
// base:<branch> case, where closedByPullRequestsReferences is structurally empty
// so item.LinkedPRNumber is always 0: the gate must resolve the PR number via
// FetchLinkedPR rather than silently skipping itself.
// TestAttemptMergeOnValidate_ReviewGate_IgnoresStaleClosedPR pins the State/Merged
// filter on the FetchLinkedPR fallback. FetchLinkedPR queries state=all, so a stale
// PR from a previous cycle on the same fabrik/issue-N branch can be returned. Gating
// on it would read the wrong PR's review state — here the stale PR carries an
// outstanding reviewer, which would block a landing that has nothing to do with it.
// handleBrokenReviewLinkage applies the same filter to its own FetchLinkedPR result.
func TestAttemptMergeOnValidate_ReviewGate_IgnoresStaleClosedPR(t *testing.T) {
	waitTrue := true
	reviewFetched := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			// Stale PR from a previous cycle: closed, never merged.
			return &gh.PRDetails{Number: 77, HeadSHA: "sha1", State: "closed"}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			reviewFetched = true
			return []gh.ReviewRequest{{Login: "verveguy"}}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			reviewFetched = true
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"base:dev"}}

	if _, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewFetched {
		t.Error("the gate must not read review state from a closed PR that merely shares the branch name")
	}
	if hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("a stale closed PR must not cause the landing to be held for review")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_IgnoresAlreadyMergedPR is the Merged half of
// the same filter: a PR that already merged (e.g. via a race with a retried landing)
// is not a PR whose review state should decide this landing.
func TestAttemptMergeOnValidate_ReviewGate_IgnoresAlreadyMergedPR(t *testing.T) {
	waitTrue := true
	reviewFetched := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, HeadSHA: "sha1", State: "closed", Merged: true}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			reviewFetched = true
			return []gh.ReviewRequest{{Login: "verveguy"}}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			reviewFetched = true
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"base:dev"}}

	if _, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewFetched {
		t.Error("the gate must not read review state from an already-merged PR")
	}
}

func TestAttemptMergeOnValidate_ReviewGate_ResolvesPRViaFallback(t *testing.T) {
	waitTrue := true
	var gatedPR int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 77, HeadSHA: "sha1", State: "open"}, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			gatedPR = prNumber
			return []gh.ReviewRequest{{Login: "verveguy"}}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Labels: []string{"base:dev"}}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue})
	if err != nil {
		t.Fatalf("expected (false, false, nil) when the review gate blocks, got err %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when the review gate blocks")
	}
	if gatedPR != 77 {
		t.Errorf("review gate checked PR #%d, want #77 resolved via FetchLinkedPR fallback", gatedPR)
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called while reviewers are outstanding, got %d call(s)",
			len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_ReviewGate_FetchErrorBlocks verifies the conservative
// failure mode: an unreadable review state blocks the landing rather than clearing
// the gate on unknown data.
func TestAttemptMergeOnValidate_ReviewGate_FetchErrorBlocks(t *testing.T) {
	waitTrue := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, errors.New("boom")
		},
		// Requests succeed and report nobody outstanding — trusting this side alone
		// would falsely clear the gate. Both sides must be discarded together.
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", LinkedPRNumber: 10}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue})
	if err != nil {
		t.Fatalf("expected (false, false, nil) on a review-fetch error, got err %v", err)
	}
	if enabled {
		t.Error("expected enabled=false on a review-fetch error")
	}
	if len(client.enablePullRequestAutoMergeCalls) != 0 {
		t.Errorf("EnablePullRequestAutoMerge must not be called when review state is unknown, got %d call(s)",
			len(client.enablePullRequestAutoMergeCalls))
	}
}

// TestAttemptMergeOnValidate_ReviewGate_FetchErrorLogsDistinctly pins the
// operator-facing signal. A review-fetch failure and a PR nobody has reviewed yet
// both reach the same blocking exit with outstanding empty and hasReviews false,
// so without a distinct message an operator watching a GitHub API outage would see
// "waiting for initial review submission" and have no indication the gate is
// actually blocking on unknown state.
func TestAttemptMergeOnValidate_ReviewGate_FetchErrorLogsDistinctly(t *testing.T) {
	waitTrue := true
	client := &mockGitHubClient{
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			return nil, errors.New("boom")
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	eventsCh := make(chan tui.Event, 32)
	eng.events = eventsCh
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", LinkedPRNumber: 10}

	if _, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue}); err != nil {
		t.Fatalf("expected (false, false, nil) on a review-fetch error, got err %v", err)
	}

	close(eventsCh)
	var sawUnreadable, sawInitialSubmission bool
	for ev := range eventsCh {
		le, ok := ev.(tui.LogEvent)
		if !ok || le.Tag != "awaiting-review" {
			continue
		}
		if strings.Contains(le.Message, "review state unreadable") {
			sawUnreadable = true
		}
		if strings.Contains(le.Message, "waiting for initial review submission") {
			sawInitialSubmission = true
		}
	}
	if !sawUnreadable {
		t.Error("expected the hold to report the review state as unreadable on a fetch error")
	}
	if sawInitialSubmission {
		t.Error("a fetch error must not be reported as 'waiting for initial review submission' — " +
			"that message means nobody has reviewed yet, not that review state is unknown")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_LinkedPRFetchErrorBlocks pins the
// distinction between "no PR" and "could not read the PR". On a base:<branch>
// repo LinkedPRNumber is always 0, so FetchLinkedPR is the gate's only PR-resolution
// route — treating a transient failure there as "nothing to gate on" would land the
// item with the gate never evaluated at all, which is the exact FR-1 failure this
// issue is about.
func TestAttemptMergeOnValidate_ReviewGate_LinkedPRFetchErrorBlocks(t *testing.T) {
	waitTrue := true
	var reviewFetches int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errors.New("boom")
		},
		// Would report a clear gate if it were ever consulted — it must not be,
		// since there is no PR number to key it on.
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			reviewFetches++
			return []gh.PRReview{{State: "APPROVED"}}, nil
		},
	}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo", Labels: []string{"base:dev"}}

	enabled, _, err := eng.attemptMergeOnValidate(context.Background(), board, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue})
	if err != nil {
		t.Fatalf("expected (false, false, nil) on a linked-PR fetch error, got err %v", err)
	}
	if enabled {
		t.Error("expected enabled=false when the linked PR could not be read")
	}
	if len(client.updateStatusCalls) != 0 {
		t.Errorf("item must not advance while review state is unknown, got %d status update(s)",
			len(client.updateStatusCalls))
	}
	if reviewFetches != 0 {
		t.Errorf("review state must not be fetched without a resolved PR number, got %d call(s)", reviewFetches)
	}
	if !hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("expected fabrik:awaiting-review to be applied so the review timeout can anchor")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_NoPRDoesNotBlock verifies that a
// definitively absent PR does not strand the item: no PR means no reviewer
// requests, so the landing proceeds (handleBrokenReviewLinkage owns the
// broken-linkage pause). Contrast with the fetch-error case above.
func TestAttemptMergeOnValidate_ReviewGate_NoPRDoesNotBlock(t *testing.T) {
	waitTrue := true
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	stgs := testStagesWithValidateAndHolding()
	eng := testEngineWithStages(t, client, stgs)
	eng.cfg.MergeTrain = "on"
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1", Repo: "owner/repo"}

	if _, _, err := eng.attemptMergeOnValidate(context.Background(), board, item,
		&stages.Stage{Name: "Validate", WaitForReviews: &waitTrue}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.updateStatusCalls) != 1 {
		t.Fatalf("expected the advance to proceed when no PR exists, got %d status update(s)", len(client.updateStatusCalls))
	}
	if hasAddLabelCall(client, "fabrik:awaiting-review") {
		t.Error("fabrik:awaiting-review must not be applied when there is no PR to review")
	}
}

// TestAttemptMergeOnValidate_ReviewGate_OptOutCostsNothing pins the opt-in guard:
// a stage without wait_for_reviews performs no review fetches at all, and no extra
// FetchLinkedPR call. This is the no-regression guard for every other
// TestAttemptMergeOnValidate_* case in this file, all of which leave WaitForReviews nil.
func TestAttemptMergeOnValidate_ReviewGate_OptOutCostsNothing(t *testing.T) {
	var linkedPRCalls, reviewCalls, requestCalls int
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			linkedPRCalls++
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchPRReviewsFn: func(owner, repo string, prNumber int) ([]gh.PRReview, error) {
			reviewCalls++
			return nil, nil
		},
		fetchPRReviewRequestsFn: func(owner, repo string, prNumber int) ([]gh.ReviewRequest, error) {
			requestCalls++
			return nil, nil
		},
	}
	eng := testEngineForMerge(t, client)
	eng.cfg.MergeTrain = "off"
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	if _, _, err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item,
		&stages.Stage{Name: "Validate"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewCalls != 0 || requestCalls != 0 {
		t.Errorf("review state must not be fetched when wait_for_reviews is unset: %d review, %d request call(s)",
			reviewCalls, requestCalls)
	}
	if linkedPRCalls != 1 {
		t.Errorf("expected exactly 1 FetchLinkedPR call (the auto-merge path's own), got %d", linkedPRCalls)
	}
}

// hasAddLabelCall reports whether the mock recorded an AddLabelToIssue call for label.
func hasAddLabelCall(client *mockGitHubClient, label string) bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addLabelCalls {
		if c.labelName == label {
			return true
		}
	}
	return false
}
