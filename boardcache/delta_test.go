package boardcache

// Delegation-correctness tests for Phase 3-B.
//
// These tests verify that CacheImpl correctly delegates to itemstate.Store
// internally while preserving all public-API semantics. They complement the
// behavioral tests in boardcache_test.go with focused Store-delegation checks.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
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
// Phase 3-D: issues.* delta handlers — per-action coverage
// ---------------------------------------------------------------------------

func TestIssuesOpened(t *testing.T) {
	c := NewCacheImpl(&mockClient{}, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	payload := issuesOpenedPayloadJSON("owner/repo", 99, "I_99", "New Issue", "body text",
		[]string{"enhancement"}, []string{"alice"})
	c.ApplyDelta("issues", payload)

	s := testGetState(t, c, "owner/repo", 99)
	if s.Title != "New Issue" {
		t.Errorf("Title: want %q, got %q", "New Issue", s.Title)
	}
	if s.Body != "body text" {
		t.Errorf("Body: want %q, got %q", "body text", s.Body)
	}
	if !containsStr(s.Labels, "enhancement") {
		t.Errorf("Labels: want [enhancement], got %v", s.Labels)
	}
	if !containsStr(s.Assignees, "alice") {
		t.Errorf("Assignees: want [alice], got %v", s.Assignees)
	}
}

func TestIssuesOpenedIdempotent(t *testing.T) {
	c := NewCacheImpl(&mockClient{}, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	payload := issuesOpenedPayloadJSON("owner/repo", 99, "I_99", "Issue", "", []string{"bug"}, nil)
	c.ApplyDelta("issues", payload)
	c.ApplyDelta("issues", payload) // duplicate

	s := testGetState(t, c, "owner/repo", 99)
	count := 0
	for _, l := range s.Labels {
		if l == "bug" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("label 'bug' should appear exactly once after duplicate open, got %d", count)
	}
}

func TestIssuesClosed(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesActionPayloadJSON("closed", "owner/repo", 1))

	s := testGetState(t, c, "owner/repo", 1)
	if !s.IsClosed {
		t.Error("want IsClosed=true after issues.closed")
	}
}

func TestIssuesReopened(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesActionPayloadJSON("closed", "owner/repo", 1))
	c.ApplyDelta("issues", issuesActionPayloadJSON("reopened", "owner/repo", 1))

	s := testGetState(t, c, "owner/repo", 1)
	if s.IsClosed {
		t.Error("want IsClosed=false after issues.reopened")
	}
}

func TestIssuesEdited(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesEditedPayloadJSON("owner/repo", 1, "Updated Title", "Updated body"))

	s := testGetState(t, c, "owner/repo", 1)
	if s.Title != "Updated Title" {
		t.Errorf("Title: want %q, got %q", "Updated Title", s.Title)
	}
	if s.Body != "Updated body" {
		t.Errorf("Body: want %q, got %q", "Updated body", s.Body)
	}
}

func TestIssuesDeleted(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesActionPayloadJSON("deleted", "owner/repo", 1))

	if _, err := c.store.Get("owner/repo", 1); err == nil {
		t.Error("want ErrNotFound after issues.deleted, but item still in store")
	}
	// Item #2 must be unaffected.
	if _, err := c.store.Get("owner/repo", 2); err != nil {
		t.Errorf("item #2 should still be present after unrelated delete: %v", err)
	}
}

func TestIssuesTransferred(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesActionPayloadJSON("transferred", "owner/repo", 1))

	if _, err := c.store.Get("owner/repo", 1); err == nil {
		t.Error("want ErrNotFound after issues.transferred, but item still in store")
	}
}

func TestIssuesDeletedCleansPRLinkage(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	c.ApplyDelta("issues", issuesActionPayloadJSON("deleted", "owner/repo", 1))

	c.mu.RLock()
	_, found := c.prNumToKey[prKey("owner/repo", 42)]
	c.mu.RUnlock()
	if found {
		t.Error("prNumToKey entry for PR #42 should have been removed after issues.deleted")
	}
}

func TestIssuesAssigned(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("issues", issuesAssignedPayloadJSON("assigned", "owner/repo", 1, []string{"alice", "bob"}))

	s := testGetState(t, c, "owner/repo", 1)
	if !containsStr(s.Assignees, "alice") || !containsStr(s.Assignees, "bob") {
		t.Errorf("Assignees: want [alice bob], got %v", s.Assignees)
	}
}

func TestIssuesUnassigned(t *testing.T) {
	c := seedCache(t)
	// Assign alice first.
	c.ApplyDelta("issues", issuesAssignedPayloadJSON("assigned", "owner/repo", 1, []string{"alice"}))
	// Unassign: full list is now empty.
	c.ApplyDelta("issues", issuesAssignedPayloadJSON("unassigned", "owner/repo", 1, []string{}))

	s := testGetState(t, c, "owner/repo", 1)
	if containsStr(s.Assignees, "alice") {
		t.Errorf("want alice removed after unassigned; Assignees: %v", s.Assignees)
	}
}

// TestFabrikWentDeaf_IssuesLabeledBeforeOpened is the primary regression test for the
// "fabrik went deaf" bug class. Without this PR's fix, the labeled delta for an
// unknown issue is silently dropped. With the fix, ensureIssueInStore calls
// FetchProjectItem, populates the cache, and then applies the label.
//
// This test MUST fail without the ensureIssueInStore fallback.
func TestFabrikWentDeaf_IssuesLabeledBeforeOpened(t *testing.T) {
	mc := &mockClient{
		projectItemResult: &gh.ProjectItem{
			Number: 99,
			Title:  "Brand New Issue",
			Repo:   "owner/repo",
			Labels: nil, // no labels yet from GitHub
		},
	}
	// Cache starts empty — issue #99 is NOT in the cache.
	c := NewCacheImpl(mc, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	// Deliver labeled event BEFORE the item is in the cache (out-of-order delivery).
	c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 99, "fabrik:yolo"))

	// The item should now be in the cache (populated via fallback) with the label applied.
	s := testGetState(t, c, "owner/repo", 99)
	if !containsStr(s.Labels, "fabrik:yolo") {
		t.Errorf("want label 'fabrik:yolo' on issue #99 after fallback+labeled; got Labels=%v", s.Labels)
	}
	if mc.fetchProjectItemCount != 1 {
		t.Errorf("want exactly 1 FetchProjectItem call (fallback), got %d", mc.fetchProjectItemCount)
	}
}

func TestOutOfOrderIssuesLabeledBeforeOpened(t *testing.T) {
	mc := &mockClient{
		projectItemResult: &gh.ProjectItem{
			Number: 77,
			Title:  "OOO Issue",
			Repo:   "owner/repo",
		},
	}
	c := NewCacheImpl(mc, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	// labeled arrives first (out of order).
	c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 77, "stage:Research"))

	// opened arrives after — should be idempotent.
	c.ApplyDelta("issues", issuesOpenedPayloadJSON("owner/repo", 77, "I_77", "OOO Issue", "", nil, nil))

	s := testGetState(t, c, "owner/repo", 77)
	if !containsStr(s.Labels, "stage:Research") {
		t.Errorf("label 'stage:Research' should be present after out-of-order delivery; got %v", s.Labels)
	}
}

func TestIssuesClosedFallbackFetch(t *testing.T) {
	mc := &mockClient{
		projectItemResult: &gh.ProjectItem{
			Number: 55,
			Title:  "Missing Issue",
			Repo:   "owner/repo",
		},
	}
	c := NewCacheImpl(mc, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	// closed arrives but issue not in cache — should trigger fetch then close.
	c.ApplyDelta("issues", issuesActionPayloadJSON("closed", "owner/repo", 55))

	s := testGetState(t, c, "owner/repo", 55)
	if !s.IsClosed {
		t.Error("want IsClosed=true after fallback fetch + closed delta")
	}
}

// ---------------------------------------------------------------------------
// Phase 3-D: pull_request.* — draft and reviewer handlers
// ---------------------------------------------------------------------------

func TestPullRequestReadyForReview(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)
	// Establish linkedPRs entry with Draft=true.
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("opened", "owner/repo", 42, "sha1", "open", false, true))

	c.ApplyDelta("pull_request", pullRequestDraftPayloadJSON("ready_for_review", "owner/repo", 42))

	c.mu.RLock()
	pr := c.linkedPRs[prKey("owner/repo", 42)]
	c.mu.RUnlock()
	if pr == nil || pr.Draft {
		t.Errorf("want Draft=false after ready_for_review; pr=%v", pr)
	}
}

func TestPullRequestConvertedToDraft(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)
	// Establish linkedPRs entry with Draft=false.
	c.ApplyDelta("pull_request", pullRequestPayloadJSON("opened", "owner/repo", 42, "sha1", "open", false, false))

	c.ApplyDelta("pull_request", pullRequestDraftPayloadJSON("converted_to_draft", "owner/repo", 42))

	c.mu.RLock()
	pr := c.linkedPRs[prKey("owner/repo", 42)]
	c.mu.RUnlock()
	if pr == nil || !pr.Draft {
		t.Errorf("want Draft=true after converted_to_draft; pr=%v", pr)
	}
}

func TestPullRequestReviewRequested(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	payload := pullRequestReviewRequestedPayloadJSON("owner/repo", 42, []string{"alice"}, "alice")
	c.ApplyDelta("pull_request", payload)

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || len(s.LinkedPR.ReviewRequests) != 1 {
		t.Fatalf("want 1 review request; LinkedPR=%v", s.LinkedPR)
	}
	if s.LinkedPR.ReviewRequests[0].Login != "alice" {
		t.Errorf("want reviewer alice, got %q", s.LinkedPR.ReviewRequests[0].Login)
	}
}

func TestPullRequestReviewRequestRemoved(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)

	// Add reviewer first.
	c.ApplyDelta("pull_request", pullRequestReviewRequestedPayloadJSON("owner/repo", 42, []string{"alice"}, "alice"))
	// Remove reviewer.
	c.ApplyDelta("pull_request", pullRequestReviewRequestRemovedPayloadJSON("owner/repo", 42, "alice"))

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR != nil && len(s.LinkedPR.ReviewRequests) != 0 {
		t.Errorf("want 0 review requests after removal; got %v", s.LinkedPR.ReviewRequests)
	}
}

// TestPRLinkage_opened verifies that pull_request.opened for a PR with a
// "Closes #N" body triggers the auto-heal path and populates LinkedPRNumber on
// the linked issue's cached item.
func TestPRLinkage_opened(t *testing.T) {
	mc := &mockClient{
		fetchPRClosingIssuesFn: func(owner, repo string, prNumber int) ([]int, error) {
			return []int{1}, nil // PR #42 closes issue #1
		},
	}
	c := seedCacheWithStalePRLink(t, mc) // item #1 with LinkedPRNumber=0

	c.ApplyDelta("pull_request", pullRequestPayloadJSON("opened", "owner/repo", 42, "sha-link", "open", false, false))

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || s.LinkedPR.Number != 42 {
		t.Errorf("want LinkedPR.Number=42 after pull_request.opened auto-heal, got LinkedPR=%v", s.LinkedPR)
	}
}

// ---------------------------------------------------------------------------
// Phase 3-D: documented no-ops — verify no crash and no state change
// ---------------------------------------------------------------------------

func TestIssueCommentEditedNoOp(t *testing.T) {
	c := seedCache(t)
	// First add a comment.
	c.ApplyDelta("issue_comment", issueCommentPayloadJSON("created", "owner/repo", 1, "C1", 1, "original", "alice"))
	// Edit the comment — must not panic or alter stored comments.
	c.ApplyDelta("issue_comment", issueCommentPayloadJSON("edited", "owner/repo", 1, "C1", 1, "edited text", "alice"))

	s := testGetState(t, c, "owner/repo", 1)
	if len(s.Comments) != 1 || s.Comments[0].Body != "original" {
		t.Errorf("comment edit should be a no-op; got %+v", s.Comments)
	}
}

func TestPRReviewEditedNoOp(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)
	// Submit a review.
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("submitted", "owner/repo", 42, 1001, "approved", "alice"))
	// Edit the review — must be a no-op.
	c.ApplyDelta("pull_request_review", pullRequestReviewPayloadJSON("edited", "owner/repo", 42, 1001, "approved", "alice"))

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || len(s.LinkedPR.Reviews) != 1 {
		t.Errorf("want exactly 1 review after edit no-op; got %v", s.LinkedPR)
	}
}

func TestPRReviewCommentEditedNoOp(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)
	c.ApplyDelta("pull_request_review_comment",
		pullRequestReviewCommentPayloadJSON("created", "owner/repo", 42, 200, "RC_1", "original", "bob"))
	c.ApplyDelta("pull_request_review_comment",
		pullRequestReviewCommentPayloadJSON("edited", "owner/repo", 42, 200, "RC_1", "edited", "bob"))

	s := testGetState(t, c, "owner/repo", 1)
	if s.LinkedPR == nil || len(s.LinkedPR.ThreadComments) != 1 {
		t.Errorf("want exactly 1 thread comment after edit no-op; got %v", s.LinkedPR)
	}
}

func TestCheckRunCreatedNoOp(t *testing.T) {
	c := seedCache(t)
	before := testGetState(t, c, "owner/repo", 1)

	// check_run.created must not alter any item state.
	c.ApplyDelta("check_run", checkRunPayloadJSON("created", "owner/repo", 5555, "build", "in_progress", "", "sha_xyz"))

	after := testGetState(t, c, "owner/repo", 1)
	if after.Title != before.Title {
		t.Errorf("check_run.created should be a no-op; Title changed from %q to %q", before.Title, after.Title)
	}
}

func TestCheckSuiteNoOp(t *testing.T) {
	c := seedCache(t)
	before := testGetState(t, c, "owner/repo", 1)

	// All check_suite actions must be no-ops.
	for _, action := range []string{"completed", "requested", "rerequested"} {
		payload, _ := json.Marshal(map[string]string{"action": action})
		c.ApplyDelta("check_suite", payload)
	}

	after := testGetState(t, c, "owner/repo", 1)
	if after.Title != before.Title {
		t.Errorf("check_suite should be a no-op; state changed")
	}
}

// ---------------------------------------------------------------------------
// Phase 3-D: projects_v2_item handlers
// ---------------------------------------------------------------------------

func TestProjectsV2ItemCreated(t *testing.T) {
	mc := &mockClient{
		itemDetailsResult: &gh.ProjectItem{
			Number: 88,
			Repo:   "owner/repo",
			Title:  "Board Issue",
		},
	}
	c := NewCacheImpl(mc, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	payload := projectsV2ItemCreatedPayloadJSON("I_88", "Issue", "PVTI_088")
	c.ApplyDelta("projects_v2_item", payload)

	s := testGetState(t, c, "owner/repo", 88)
	if s.Number != 88 {
		t.Errorf("want Number=88, got %d", s.Number)
	}
	if mc.fetchItemDetailsCount != 1 {
		t.Errorf("want exactly 1 FetchItemDetails call, got %d", mc.fetchItemDetailsCount)
	}
	// Verify the board item ID was stored so subsequent edited/deleted/archived
	// events can be resolved through itemIDToKey.
	if itemID, ok := c.GetItemID(itemKey("owner/repo", 88)); !ok || itemID != "PVTI_088" {
		t.Errorf("want itemIDToKey[PVTI_088]=owner/repo#88; GetItemID returned (%q, %v)", itemID, ok)
	}
}

func TestProjectsV2ItemCreatedNonIssueIgnored(t *testing.T) {
	mc := &mockClient{}
	c := NewCacheImpl(mc, nopLog)
	c.Bootstrap(&gh.ProjectBoard{ProjectID: "P", Title: "T", OwnerType: "organization"})

	// PullRequest content type must be ignored.
	c.ApplyDelta("projects_v2_item", projectsV2ItemCreatedPayloadJSON("PR_123", "PullRequest", "PVTI_PR"))

	if mc.fetchItemDetailsCount != 0 {
		t.Errorf("PullRequest content_type should not trigger FetchItemDetails, got %d calls", mc.fetchItemDetailsCount)
	}
}

func TestProjectsV2ItemDeleted(t *testing.T) {
	c := seedCache(t)
	// Item #1 has ItemID "PVTI_001" (set by seedCache via Bootstrap).
	c.ApplyDelta("projects_v2_item", projectsV2ItemRemovedPayloadJSON("deleted", "PVTI_001"))

	if _, err := c.store.Get("owner/repo", 1); err == nil {
		t.Error("want item #1 removed after projects_v2_item.deleted")
	}
	// Item #2 must be unaffected.
	if _, err := c.store.Get("owner/repo", 2); err != nil {
		t.Errorf("item #2 should still be present: %v", err)
	}
}

func TestProjectsV2ItemArchived(t *testing.T) {
	c := seedCache(t)
	c.ApplyDelta("projects_v2_item", projectsV2ItemRemovedPayloadJSON("archived", "PVTI_001"))

	if _, err := c.store.Get("owner/repo", 1); err == nil {
		t.Error("want item #1 removed after projects_v2_item.archived")
	}
}

func TestProjectsV2ItemDeletedCleansPRLinkage(t *testing.T) {
	c := seedCache(t)
	testSetLinkedPR(c, "owner/repo", 1, 42)
	c.mu.Lock()
	c.linkedPRs[prKey("owner/repo", 42)] = &gh.PRDetails{Number: 42}
	c.mu.Unlock()

	c.ApplyDelta("projects_v2_item", projectsV2ItemRemovedPayloadJSON("deleted", "PVTI_001"))

	c.mu.RLock()
	_, pkFound := c.prNumToKey[prKey("owner/repo", 42)]
	_, lrFound := c.linkedPRs[prKey("owner/repo", 42)]
	c.mu.RUnlock()
	if pkFound || lrFound {
		t.Error("prNumToKey and linkedPRs entries for PR #42 should be cleaned up after item deletion")
	}
}

func TestProjectsV2ItemRestoredNoOp(t *testing.T) {
	c := seedCache(t)
	before := len(c.store.All())

	// restored must not crash or duplicate items.
	p := projectsV2ItemPayload{Action: "restored"}
	p.ProjectsV2Item.ID = "PVTI_001"
	payload, _ := json.Marshal(p)
	c.ApplyDelta("projects_v2_item", payload)

	if after := len(c.store.All()); after != before {
		t.Errorf("projects_v2_item.restored must not change item count; before=%d after=%d", before, after)
	}
}

// ---------------------------------------------------------------------------
// Phase 3-D: cache pause drops deltas
// ---------------------------------------------------------------------------

func TestCachePauseDropsDelta(t *testing.T) {
	c := seedCache(t)
	c.Pause()

	// Delta delivered while paused must be dropped.
	c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "during-pause"))

	s := testGetState(t, c, "owner/repo", 1)
	if containsStr(s.Labels, "during-pause") {
		t.Error("label applied while cache was paused; expected no-op")
	}

	c.Resume()
	// Same delta delivered after resume must be applied.
	c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "during-pause"))

	s = testGetState(t, c, "owner/repo", 1)
	if !containsStr(s.Labels, "during-pause") {
		t.Error("label should be applied after cache is resumed")
	}
}

// ---------------------------------------------------------------------------
// Phase 3-D: concurrent delta — race detector clean
// ---------------------------------------------------------------------------

func TestConcurrentDeltaSameIssue(t *testing.T) {
	c := seedCache(t)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				c.ApplyDelta("issues", issuesLabeledPayloadJSON("labeled", "owner/repo", 1, "concurrent-label"))
			} else {
				c.ApplyDelta("issues", issuesLabeledPayloadJSON("unlabeled", "owner/repo", 1, "concurrent-label"))
			}
		}(i)
	}
	wg.Wait()

	// After concurrent writes, item must still be accessible.
	if _, err := c.store.Get("owner/repo", 1); err != nil {
		t.Errorf("item #1 must be accessible after concurrent deltas: %v", err)
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
