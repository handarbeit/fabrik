package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// testEngineForMerge returns a minimal engine wired for attemptMergeOnValidate tests.
func testEngineForMerge(client *mockGitHubClient) *Engine {
	stgs := testStagesWithValidate()
	return testEngineWithStages(client, stgs)
}

// TestAttemptMergeOnValidate_NoCheckRuns_MergeProceeds verifies R5: when there are
// no CI check runs at all the gate clears and MergePR is called.
func TestAttemptMergeOnValidate_NoCheckRuns_MergeProceeds(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, nil // R5: no CI
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	if err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once, got %d", len(client.mergePRCalls))
	}
	if client.mergePRCalls[0].prNumber != 10 {
		t.Errorf("MergePR called with pr %d, want 10", client.mergePRCalls[0].prNumber)
	}
}

// TestAttemptMergeOnValidate_AllGreen_MergeProceeds verifies R4: all checks green clears
// the pending timer, removes fabrik:awaiting-ci, and proceeds to merge.
func TestAttemptMergeOnValidate_AllGreen_MergeProceeds(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 11, HeadSHA: "sha2"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	// Seed a stale pending timer to confirm it gets cleared on green.
	eng.store.Apply(itemstate.CIMergePendingStarted{Repo: "owner/repo", Number: 1, At: time.Now().Add(-1 * time.Minute)})

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	if err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(client.mergePRCalls) != 1 {
		t.Fatalf("expected MergePR called once, got %d", len(client.mergePRCalls))
	}
	snap, _ := eng.store.Get("owner/repo", 1)
	if lpr := snap.LinkedPR(); lpr != nil && !lpr.CIMergePendingSince.IsZero() {
		t.Error("ciMergePendingSince should be cleared on all-green")
	}
}

// TestAttemptMergeOnValidate_CIFailed_BlocksMerge verifies R3: when any check fails,
// merge is blocked, fabrik:awaiting-ci is added, and error is returned.
func TestAttemptMergeOnValidate_CIFailed_BlocksMerge(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 12, HeadSHA: "sha3"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when CI failed, got nil")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on CI failure, got %d", len(client.mergePRCalls))
	}
	found := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			found = true
		}
	}
	if !found {
		t.Error("expected fabrik:awaiting-ci to be added on CI failure")
	}
}

// TestAttemptMergeOnValidate_CIPending_BlocksMergeNoLabel verifies R2 + R10c: when
// checks are still running, merge is blocked but fabrik:awaiting-ci is NOT applied.
func TestAttemptMergeOnValidate_CIPending_BlocksMergeNoLabel(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 13, HeadSHA: "sha4"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "build", Status: "in_progress", Conclusion: ""},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when CI pending, got nil")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on pending CI, got %d", len(client.mergePRCalls))
	}
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:awaiting-ci" {
			t.Error("fabrik:awaiting-ci must NOT be added when CI is only pending (R10c)")
		}
	}
}

// TestAttemptMergeOnValidate_CIPending_TracksTimer verifies R2: on first pending observation
// ciMergePendingSince is populated so the timeout clock starts.
func TestAttemptMergeOnValidate_CIPending_TracksTimer(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 14, HeadSHA: "sha5"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "ci", Status: "queued", Conclusion: ""},
			}, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	snapBefore, _ := eng.store.Get("owner/repo", 1)
	if lpr := snapBefore.LinkedPR(); lpr != nil && !lpr.CIMergePendingSince.IsZero() {
		t.Fatal("expected no pending entry before first call")
	}

	_ = eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})

	snapAfter, _ := eng.store.Get("owner/repo", 1)
	var afterSince time.Time
	if lpr := snapAfter.LinkedPR(); lpr != nil {
		afterSince = lpr.CIMergePendingSince
	}
	if afterSince.IsZero() {
		t.Error("expected ciMergePendingSince to be set after first pending observation")
	}
}

// TestAttemptMergeOnValidate_CIPendingTimeout_PausesIssue verifies R6: when CI has
// been pending longer than CIWaitTimeout the issue is paused with a comment.
func TestAttemptMergeOnValidate_CIPendingTimeout_PausesIssue(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 15, HeadSHA: "sha6"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return []gh.CheckRun{
				{Name: "slow-ci", Status: "in_progress", Conclusion: ""},
			}, nil
		},
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(client, stgs)
	eng.cfg.CIWaitTimeout = 1 * time.Millisecond // tiny timeout for test

	// Pre-seed as if first observed 1 second ago (well past 1ms timeout).
	eng.store.Apply(itemstate.CIMergePendingStarted{Repo: "owner/repo", Number: 1, At: time.Now().Add(-1 * time.Second)})

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error on CI timeout, got nil")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on timeout, got %d", len(client.mergePRCalls))
	}
	if len(client.addCommentCalls) == 0 {
		t.Error("expected timeout comment to be posted")
	}
	foundPaused := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			foundPaused = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused label on CI timeout")
	}
	// CIMergePendingSince should be cleared after timeout.
	snapTimeout, _ := eng.store.Get("owner/repo", 1)
	if lpr := snapTimeout.LinkedPR(); lpr != nil && !lpr.CIMergePendingSince.IsZero() {
		t.Error("ciMergePendingSince should be deleted after timeout fires")
	}
}

// TestAttemptMergeOnValidate_NoPR_ReturnsNil verifies that when FetchLinkedPR finds
// no PR, attemptMergeOnValidate returns nil (no error, no merge).
func TestAttemptMergeOnValidate_NoPR_ReturnsNil(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, nil
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	if err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"}); err != nil {
		t.Fatalf("expected nil when no PR, got %v", err)
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR when no PR, got %d", len(client.mergePRCalls))
	}
}

// TestAttemptMergeOnValidate_ErrNotMergeable_DispatchesRebase verifies that
// ErrNotMergeable dispatches a rebase reinvoke (not immediate pause) and returns
// errRebaseDispatched, adding fabrik:rebase-needed but NOT fabrik:paused.
func TestAttemptMergeOnValidate_ErrNotMergeable_DispatchesRebase(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 20, HeadSHA: "sha7"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return gh.ErrNotMergeable
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.MaxRebaseCycles = 3
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	stage := &stages.Stage{Name: "Validate"}

	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, stage)
	// Wait for the dispatched goroutine to exit (exits early via ErrSkipItem in tests).
	eng.wg.Wait()

	if !errors.Is(err, errRebaseDispatched) {
		t.Fatalf("expected errRebaseDispatched, got %v", err)
	}
	foundRebaseNeeded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:rebase-needed" {
			foundRebaseNeeded = true
		}
		if c.labelName == "fabrik:paused" {
			t.Error("fabrik:paused must NOT be added when dispatching rebase")
		}
	}
	if !foundRebaseNeeded {
		t.Error("expected fabrik:rebase-needed to be added on ErrNotMergeable")
	}
	snap1, _ := eng.store.Get("owner/repo", 1)
	if snap1.RebaseCycles("Validate") != 1 {
		t.Errorf("expected RebaseCycles(Validate) == 1, got %d", snap1.RebaseCycles("Validate"))
	}
}

// TestAttemptMergeOnValidate_ErrNotMergeable_CycleLimitPause verifies that
// when the rebase cycle limit is already reached, ErrNotMergeable falls through
// to the existing pause path: fabrik:paused + fabrik:awaiting-input.
func TestAttemptMergeOnValidate_ErrNotMergeable_CycleLimitPause(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 21, HeadSHA: "sha8"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			return gh.ErrNotMergeable
		},
	}
	eng := testEngineForMerge(client)
	eng.cfg.MaxRebaseCycles = 3
	// Pre-seed 3 rebase cycles to simulate hitting the limit
	for i := 0; i < 3; i++ {
		eng.store.Apply(itemstate.RebaseCycleIncremented{Repo: "owner/repo", Number: 1, StageName: "Validate"})
	}

	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}
	stage := &stages.Stage{Name: "Validate"}

	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, stage)
	if err == nil {
		t.Fatal("expected error at cycle limit")
	}
	if errors.Is(err, errRebaseDispatched) {
		t.Fatal("expected plain error at cycle limit, not errRebaseDispatched")
	}
	foundPaused := false
	foundRebaseNeeded := false
	for _, c := range client.addLabelCalls {
		if c.labelName == "fabrik:paused" {
			foundPaused = true
		}
		if c.labelName == "fabrik:rebase-needed" {
			foundRebaseNeeded = true
		}
	}
	if !foundPaused {
		t.Error("expected fabrik:paused when rebase cycle limit reached")
	}
	if !foundRebaseNeeded {
		t.Error("expected fabrik:rebase-needed to be added on ErrNotMergeable")
	}
}

// TestHandleStageComplete_WaitForCI_SkipsMergeAndReturns verifies Approach A': when
// wait_for_ci is true, handleStageComplete adds fabrik:awaiting-ci, does NOT add
// stage:Validate:complete, and does NOT call attemptMergeOnValidate.
// The completion label is deferred to checkCIGate in the catch-up loop (ADR 032).
func TestHandleStageComplete_WaitForCI_SkipsMergeAndReturns(t *testing.T) {
	merged := false
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 99, HeadSHA: "sha8"}, nil
		},
		mergePRFn: func(owner, repo string, prNumber int) error {
			merged = true
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

	if merged {
		t.Error("MergePR must not be called when wait_for_ci is true")
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

// TestAttemptMergeOnValidate_FetchLinkedPRError_ReturnsError verifies that a
// transient FetchLinkedPR API error returns an error (retriable) rather than nil
// (which would allow the caller to advance past Validate without merging the PR).
func TestAttemptMergeOnValidate_FetchLinkedPRError_ReturnsError(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return nil, errors.New("network error")
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when FetchLinkedPR fails, got nil (would incorrectly allow advancement)")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on FetchLinkedPR error, got %d", len(client.mergePRCalls))
	}
}

// TestAttemptMergeOnValidate_FetchCheckRunsError_ReturnsError verifies that a
// transient FetchCheckRuns API error returns an error (retriable) rather than
// proceeding to merge with unknown CI status.
func TestAttemptMergeOnValidate_FetchCheckRunsError_ReturnsError(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: 10, HeadSHA: "sha1"}, nil
		},
		fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) {
			return nil, errors.New("GitHub API 503")
		},
	}
	eng := testEngineForMerge(client)
	item := gh.ProjectItem{Number: 1, ItemID: "PVTI_1"}

	err := eng.attemptMergeOnValidate(context.Background(), &gh.ProjectBoard{}, item, &stages.Stage{Name: "Validate"})
	if err == nil {
		t.Fatal("expected error when FetchCheckRuns fails, got nil (would proceed to merge with unknown CI status)")
	}
	if len(client.mergePRCalls) != 0 {
		t.Errorf("expected no MergePR on FetchCheckRuns error, got %d", len(client.mergePRCalls))
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
	cache.Bootstrap(&gh.ProjectBoard{
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
