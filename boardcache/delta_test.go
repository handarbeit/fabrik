package boardcache

// Delegation-correctness tests for Phase 3-B.
//
// These tests verify that CacheImpl correctly delegates to itemstate.Store
// internally while preserving all public-API semantics. They complement the
// behavioral tests in boardcache_test.go with focused Store-delegation checks.

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// ---------------------------------------------------------------------------
// 1. Bootstrap store-delegation
// ---------------------------------------------------------------------------

func TestStoreDelegationBootstrap(t *testing.T) {
	c := NewCacheImpl(&mockClient{}, nopLog)
	board := &gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Research", Labels: []string{"a"}},
			{ID: "I_2", ItemID: "PVTI_2", Number: 2, Repo: "owner/repo", Status: "Plan"},
		},
	}
	c.Bootstrap(board)

	// All items accessible via store.Get.
	snap1, err := c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get(1): %v", err)
	}
	if snap1.State().Status != "Research" {
		t.Errorf("item1 Status: want Research, got %q", snap1.State().Status)
	}
	if !containsStr(snap1.Labels(), "a") {
		t.Errorf("item1 Labels: want [a], got %v", snap1.Labels())
	}

	snap2, err := c.store.Get("owner/repo", 2)
	if err != nil {
		t.Fatalf("store.Get(2): %v", err)
	}
	if snap2.State().Status != "Plan" {
		t.Errorf("item2 Status: want Plan, got %q", snap2.State().Status)
	}

	// store.All returns both items.
	all := c.store.All()
	if len(all) != 2 {
		t.Errorf("store.All: want 2 items, got %d", len(all))
	}

	// Store preserves the content node ID (used by github.Client.FetchItemDetails GraphQL query).
	if snap1.State().ID != "I_1" {
		t.Errorf("store.Get(1) ID: want I_1, got %q", snap1.State().ID)
	}

	// FetchProjectBoard reconstructs identical board with ID preserved.
	fetched, err := c.FetchProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(fetched.Items) != 2 {
		t.Errorf("FetchProjectBoard: want 2 items, got %d", len(fetched.Items))
	}
	if fetched.ProjectID != "PID" {
		t.Errorf("FetchProjectBoard.ProjectID: want PID, got %q", fetched.ProjectID)
	}
	// Verify ID is present in reconstructed board items so FetchItemDetails GraphQL queries work.
	found := false
	for _, item := range fetched.Items {
		if item.Number == 1 && item.ID == "I_1" {
			found = true
		}
	}
	if !found {
		t.Errorf("FetchProjectBoard: item #1 ID not preserved; want I_1 in reconstructed items")
	}
}

// ---------------------------------------------------------------------------
// 2. Reconcile drift log
// ---------------------------------------------------------------------------

func TestStoreDelegationReconcile(t *testing.T) {
	var logBuf []string
	logFn := func(format string, args ...any) { logBuf = append(logBuf, format) }
	c := NewCacheImpl(&mockClient{}, logFn)
	c.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Research"},
			{ID: "I_2", ItemID: "PVTI_2", Number: 2, Repo: "owner/repo", Status: "Plan"},
		},
	})

	// Item 1 status changed.
	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Implement"},
			{ID: "I_2", ItemID: "PVTI_2", Number: 2, Repo: "owner/repo", Status: "Plan"},
		},
	})

	// Store reflects the new status.
	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Implement" {
		t.Errorf("after Reconcile: want Status Implement, got %q", s.Status)
	}

	// "[reconciliation] N items differed" log was emitted.
	foundReconciliationLog := false
	for _, line := range logBuf {
		if len(line) >= 16 && line[:16] == "[reconciliation]" {
			foundReconciliationLog = true
			break
		}
	}
	if !foundReconciliationLog {
		t.Errorf("expected [reconciliation] log, got: %v", logBuf)
	}
}

// ---------------------------------------------------------------------------
// 3. Per-action delta delegation — labeled
// ---------------------------------------------------------------------------

func TestStoreDelegationDeltaLabeled(t *testing.T) {
	c := seedCache(t)
	payload := issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "priority")
	c.ApplyDelta("issues", payload)

	snap, err := c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !containsStr(snap.Labels(), "priority") {
		t.Errorf("Store.Labels after labeled delta: want priority, got %v", snap.Labels())
	}
}

// ---------------------------------------------------------------------------
// 4. Per-action delta delegation — unlabeled
// ---------------------------------------------------------------------------

func TestStoreDelegationDeltaUnlabeled(t *testing.T) {
	c := seedCache(t)
	// Item #1 starts with "enhancement".
	payload := issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "enhancement")
	c.ApplyDelta("issues", payload)

	snap, err := c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if containsStr(snap.Labels(), "enhancement") {
		t.Errorf("Store.Labels after unlabeled: enhancement should be gone, got %v", snap.Labels())
	}
}

// ---------------------------------------------------------------------------
// 5. Per-action delta delegation — issue_comment.created
// ---------------------------------------------------------------------------

func TestStoreDelegationDeltaComment(t *testing.T) {
	c := seedCache(t)
	payload := issueCommentPayloadJSON("created", "owner/repo", 1, "NC_1", 42, "hello", "alice")
	c.ApplyDelta("issue_comment", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if len(s.Comments) != 1 || s.Comments[0].DatabaseID != 42 {
		t.Errorf("Store.Comments after comment delta: want [{DatabaseID:42}], got %+v", s.Comments)
	}
}

// ---------------------------------------------------------------------------
// 6. Per-action delta delegation — pull_request SHA update
// ---------------------------------------------------------------------------

func TestStoreDelegationDeltaPRSync(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 10)

	payload := pullRequestPayloadJSON("synchronize", "owner/repo", 10, "sha_sync", "open", false, false)
	c.ApplyDelta("pull_request", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || s.LinkedPR.HeadSHA != "sha_sync" {
		t.Errorf("Store.LinkedPR.HeadSHA after PR sync: want sha_sync, got %v", s.LinkedPR)
	}
}

// ---------------------------------------------------------------------------
// 7. Per-action delta delegation — check_run routes via shaToKey
// ---------------------------------------------------------------------------

func TestStoreDelegationDeltaCheckRunViaSHA(t *testing.T) {
	c := seedCache(t)
	// Establish SHA → issue mapping via PR delta.
	testSetLinkedPR(c, "owner/repo", 1, 10)
	c.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, SHA: "sha_ci"})

	payload := checkRunPayloadJSON("completed", "owner/repo", 7777, "ci", "completed", "success", "sha_ci")
	c.ApplyDelta("check_run", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || len(s.LinkedPR.CheckRuns) != 1 || s.LinkedPR.CheckRuns[0].ID != 7777 {
		t.Errorf("Store.LinkedPR.CheckRuns after check_run: want [{ID:7777}], got %v", s.LinkedPR)
	}
}

// ---------------------------------------------------------------------------
// 8. Per-action delta delegation — projects_v2_item.edited via itemIDToKey
// ---------------------------------------------------------------------------

func TestStoreDelegationDeltaStatusEditViaItemID(t *testing.T) {
	c := seedCache(t)
	// PVTI_001 → item #1 (itemIDToKey index set by Bootstrap).
	payload := projectsV2ItemPayloadJSON("edited", "PVTI_001", "Review")
	c.ApplyDelta("projects_v2_item", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Review" {
		t.Errorf("Store.Status after projects_v2_item edited: want Review, got %q", s.Status)
	}
}

// ---------------------------------------------------------------------------
// 9. Concurrent access — race detector clean
// ---------------------------------------------------------------------------

func TestStoreDelegationConcurrentAccess(t *testing.T) {
	c := seedCache(t)

	var wg sync.WaitGroup
	const goroutines = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			switch idx % 5 {
			case 0:
				// Read via FetchProjectBoard.
				c.FetchProjectBoard("owner", "repo", 1, "organization")
			case 1:
				// Write via ApplyDelta labeled.
				label := "label-concurrent"
				c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 1, label))
			case 2:
				// Write via ApplyDelta unlabeled.
				c.ApplyDelta("issues", issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "label-concurrent"))
			case 3:
				// Read via store.Get.
				c.store.Get("owner/repo", 1)
			case 4:
				// Write via UpdateItemStatus.
				c.UpdateItemStatus(ItemKey("owner/repo", 1), "Research")
			}
		}(i)
	}
	wg.Wait()

	// Verify cache is still consistent.
	_, err := c.store.Get("owner/repo", 1)
	if err != nil {
		t.Errorf("store.Get after concurrent access: %v", err)
	}
	if len(c.store.All()) != 2 {
		t.Errorf("store.All after concurrent access: want 2 items, got %d", len(c.store.All()))
	}
}

// ---------------------------------------------------------------------------
// 10. Negative cache TTL — expired entry allows fresh REST call
// ---------------------------------------------------------------------------

func TestStoreDelegationNegativeCacheTTLExpiry(t *testing.T) {
	callCount := 0
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			callCount++
			return nil, nil // no closing reference
		},
	}
	c := seedCacheWithStalePRLink(t, mc)

	// First delta — triggers REST call and populates negative cache.
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 33, 9001, "approved", "bot"))
	if callCount != 1 {
		t.Fatalf("want 1 REST call after first delta, got %d", callCount)
	}

	// Immediately replay — negative cache blocks second REST call.
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 33, 9002, "approved", "bot2"))
	if callCount != 1 {
		t.Errorf("want negative cache to block second REST call, got %d total calls", callCount)
	}

	// Manually expire the negative cache entry by back-dating it.
	mk := missKey("owner/repo", 33)
	c.mu.Lock()
	c.recentMissCache[mk] = time.Now().Add(-(recentMissTTL + time.Second))
	c.mu.Unlock()

	// After TTL expiry, the REST call fires again.
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 33, 9003, "approved", "bot3"))
	if callCount != 2 {
		t.Errorf("want 2 REST calls after TTL expiry, got %d", callCount)
	}
}

// ---------------------------------------------------------------------------
// 11. FetchItemDetails backfills prNumToKey so PR review/comment deltas route
//     without auto-heal after the first deep fetch.
// ---------------------------------------------------------------------------

func TestFetchItemDetailsBackfillsPrNumToKey(t *testing.T) {
	mc := &mockClient{
		itemDetailsResult: &gh.ProjectItem{
			Body:           "body",
			Author:         "alice",
			LinkedPRNumber: 42,
		},
	}
	c := NewCacheImpl(mc, nopLog)
	c.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Research"},
		},
	})

	item := gh.ProjectItem{ID: "I_1", Number: 1, Repo: "owner/repo"}
	if err := c.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if item.LinkedPRNumber != 42 {
		t.Fatalf("want LinkedPRNumber 42, got %d", item.LinkedPRNumber)
	}

	// prNumToKey must be backfilled so PR deltas can route by PR number.
	pk := prKey("owner/repo", 42)
	c.mu.RLock()
	issKey, found := c.prNumToKey[pk]
	c.mu.RUnlock()
	if !found {
		t.Errorf("prNumToKey not backfilled after FetchItemDetails with LinkedPRNumber=42")
	}
	wantKey := itemKey("owner/repo", 1)
	if issKey != wantKey {
		t.Errorf("prNumToKey: want %q, got %q", wantKey, issKey)
	}
}

// ---------------------------------------------------------------------------
// 12. Reconcile item removal cleans up stale prNumToKey entries.
// ---------------------------------------------------------------------------

func TestReconcileRemoveCleansUpPrNumToKey(t *testing.T) {
	c := seedCache(t)
	// Establish PR linkage for item #1 → PR #10.
	testSetLinkedPR(c, "owner/repo", 1, 10)

	pk := prKey("owner/repo", 10)
	c.mu.RLock()
	_, beforeReconcile := c.prNumToKey[pk]
	c.mu.RUnlock()
	if !beforeReconcile {
		t.Fatalf("prNumToKey for PR #10 not set before Reconcile")
	}

	// Reconcile with a board that no longer contains item #1.
	c.Reconcile(&gh.ProjectBoard{
		ProjectID: "PID", Title: "T", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_002", ItemID: "PVTI_002", Number: 2, Repo: "owner/repo", Status: "Plan"},
		},
	})

	c.mu.RLock()
	_, afterReconcile := c.prNumToKey[pk]
	c.mu.RUnlock()
	if afterReconcile {
		t.Errorf("prNumToKey entry for PR #10 survived after item #1 removed by Reconcile")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
