package engine

import (
	"encoding/json"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// issueEventPayload builds a minimal issues / issue_comment webhook payload
// for use in Layer 1 status-refresh tests.
func issueEventPayload(repo string, issueNum int) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"issue":      map[string]interface{}{"number": issueNum},
		"repository": map[string]interface{}{"full_name": repo},
	})
	return b
}

// layer1Cache creates a CacheImpl backed by the engine's shared Store.
// If projectID is non-empty, the cache is bootstrapped with a single item that
// has no itemID (simulating the issues.opened-without-project-info path).
func layer1Cache(eng *Engine, client *mockGitHubClient, projectID string, withItem bool) *boardcache.CacheImpl {
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	if !withItem {
		// Bootstrap with projectID only — no items.
		cache.Bootstrap(&gh.ProjectBoard{
			ProjectID: projectID, OwnerType: "organization",
		})
		return cache
	}
	// Bootstrap with one item that has no itemID — simulates issues.opened path
	// where the payload carries no project board info.
	cache.Bootstrap(&gh.ProjectBoard{
		ProjectID: projectID, OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "", Number: 1, Repo: "owner/repo", Status: ""},
		},
	})
	return cache
}

// TestLayer1StatusRefreshRegression is the mandatory regression test.
// On current main this test FAILS because the silent bail at GetItemID !ok
// prevents LookupIssueProjectItem from ever being called.
func TestLayer1StatusRefreshRegression(t *testing.T) {
	const newItemID = "PVTI_new"
	const newStatus = "Research"

	client := &mockGitHubClient{
		lookupIssueProjectItemFn: func(projectID, repo string, issueNumber int) (string, string, error) {
			return newItemID, newStatus, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Wire mayNeedWork observer so we can assert StatusChanged fires.
	mwnObs := newMayNeedWorkObserver(&eng.mayNeedWorkMu, &eng.mayNeedWork)
	unsub := eng.store.Subscribe(mwnObs)
	defer unsub()

	// Bootstrap issue #1 WITHOUT an itemID.
	cache := layer1Cache(eng, client, "PVT_test", true)

	key := boardcache.ItemKey("owner/repo", 1)
	if _, ok := cache.GetItemID(key); ok {
		t.Fatal("precondition: issue should not have itemID before the call")
	}

	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)

	// Assert LookupIssueProjectItem was called with correct args.
	client.mu.Lock()
	calls := client.lookupIssueProjectItemCalls
	fpCalls := client.fetchProjectItemStatusCalls
	client.mu.Unlock()

	if len(calls) != 1 {
		t.Fatalf("LookupIssueProjectItem call count = %d, want 1", len(calls))
	}
	if calls[0].projectID != "PVT_test" {
		t.Errorf("call.projectID = %q, want %q", calls[0].projectID, "PVT_test")
	}
	if calls[0].repo != "owner/repo" {
		t.Errorf("call.repo = %q, want %q", calls[0].repo, "owner/repo")
	}
	if calls[0].issueNumber != 1 {
		t.Errorf("call.issueNumber = %d, want 1", calls[0].issueNumber)
	}
	if len(fpCalls) != 0 {
		t.Errorf("FetchProjectItemStatus called %d times, want 0 (fast path not taken)", len(fpCalls))
	}

	// Assert cache now has the itemID.
	id, ok := cache.GetItemID(key)
	if !ok || id != newItemID {
		t.Errorf("GetItemID = %q ok=%v, want %q true", id, ok, newItemID)
	}

	// Assert cache has the correct status (via shared Store).
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.State().Status != newStatus {
		t.Errorf("item Status = %q, want %q", snap.State().Status, newStatus)
	}

	// Assert StatusChanged observer fired (item appears in mayNeedWork).
	eng.mayNeedWorkMu.Lock()
	inSet := eng.mayNeedWork[key]
	eng.mayNeedWorkMu.Unlock()
	if !inSet {
		t.Error("StatusChanged observer did not fire: item not in mayNeedWork")
	}
}

// TestLayer1StatusRefreshFastPath is the no-regression test.
// When the cache already has an itemID, Layer 1 must use FetchProjectItemStatus
// and must NOT call LookupIssueProjectItem.
func TestLayer1StatusRefreshFastPath(t *testing.T) {
	const existingItemID = "PVTI_existing"
	const newStatus = "Plan"

	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			return newStatus, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Bootstrap with an item that already has an itemID (normal post-Bootstrap state).
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PVT_test", OwnerType: "organization",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: existingItemID, Number: 1, Repo: "owner/repo", Status: "Research"},
		},
	})

	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)

	client.mu.Lock()
	fpCalls := client.fetchProjectItemStatusCalls
	lookupCalls := client.lookupIssueProjectItemCalls
	client.mu.Unlock()

	if len(fpCalls) != 1 {
		t.Fatalf("FetchProjectItemStatus call count = %d, want 1", len(fpCalls))
	}
	if fpCalls[0] != existingItemID {
		t.Errorf("FetchProjectItemStatus called with %q, want %q", fpCalls[0], existingItemID)
	}
	if len(lookupCalls) != 0 {
		t.Errorf("LookupIssueProjectItem called %d times, want 0 (should take fast path)", len(lookupCalls))
	}
}

// TestLayer1StatusRefreshSkipWhenNotOnBoard verifies that when
// LookupIssueProjectItem returns ("", "", nil) — the issue is not on fabrik's
// project — Layer 1 silently returns without updating the cache.
func TestLayer1StatusRefreshSkipWhenNotOnBoard(t *testing.T) {
	client := &mockGitHubClient{
		lookupIssueProjectItemFn: func(projectID, repo string, issueNumber int) (string, string, error) {
			return "", "", nil // issue not on the project
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	cache := layer1Cache(eng, client, "PVT_test", true)

	key := boardcache.ItemKey("owner/repo", 1)

	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)

	// Cache should still have no itemID.
	if id, ok := cache.GetItemID(key); ok || id != "" {
		t.Errorf("GetItemID = %q ok=%v, want empty and false", id, ok)
	}

	// Status should remain empty.
	snap, err := eng.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if snap.State().Status != "" {
		t.Errorf("Status = %q, want empty", snap.State().Status)
	}
}

// TestLayer1StatusRefreshEmptyProjectID verifies that when cache.ProjectID()
// returns "" (Bootstrap not yet complete), Layer 1 skips the fallback call
// entirely. This guards against useless API calls during the startup window.
func TestLayer1StatusRefreshEmptyProjectID(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	// Bootstrap with empty projectID — simulates pre-Bootstrap or mid-startup state.
	cache := layer1Cache(eng, client, "" /* empty projectID */, true)

	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)

	client.mu.Lock()
	lookupCalls := client.lookupIssueProjectItemCalls
	client.mu.Unlock()

	if len(lookupCalls) != 0 {
		t.Errorf("LookupIssueProjectItem called %d times with empty projectID, want 0", len(lookupCalls))
	}
}
