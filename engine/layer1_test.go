package engine

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
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
		testBootstrapFromBoard(cache, &gh.ProjectBoard{ProjectID: projectID})
		return cache
	}
	// Bootstrap with one item that has no itemID — simulates issues.opened path
	// where the payload carries no project board info.
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: projectID,
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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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
	eng := testEngine(t, client, &mockClaudeInvoker{})

	// Bootstrap with an item that already has an itemID (normal post-Bootstrap state).
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_test",
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
	eng := testEngine(t, client, &mockClaudeInvoker{})
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
	eng := testEngine(t, client, &mockClaudeInvoker{})

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

// prCommentPayload builds an issue_comment payload for a PR conversation
// comment: the issue object carries a pull_request key (#2093).
func prCommentPayload(repo string, n int, pullRequest any) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"issue": map[string]interface{}{
			"number":       n,
			"pull_request": pullRequest,
		},
		"repository": map[string]interface{}{"full_name": repo},
	})
	return b
}

func projectsV2ItemPayload(action string) []byte {
	b, _ := json.Marshal(map[string]interface{}{"action": action})
	return b
}

func layer1LookupCalls(c *mockGitHubClient) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lookupIssueProjectItemCalls)
}

type layer1TestClock struct{ t time.Time }

func (c *layer1TestClock) Now() time.Time { return c.t }

// offBoardClient returns a mock whose lookup resolves to "not on the board".
func offBoardClient() *mockGitHubClient {
	return &mockGitHubClient{
		lookupIssueProjectItemFn: func(string, string, int) (string, string, error) { return "", "", nil },
	}
}

// TestLayer1PRCommentSkipped: a PR conversation comment makes no GraphQL call
// and logs no warning (R1).
func TestLayer1PRCommentSkipped(t *testing.T) {
	client := &mockGitHubClient{
		lookupIssueProjectItemFn: func(string, string, int) (string, string, error) {
			return "", "", errors.New("Could not resolve to an Issue")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	events := make(chan tui.Event, 16)
	eng.events = events
	cache := layer1Cache(eng, client, "PVT_test", true)

	pr := map[string]interface{}{"url": "https://api.github.com/repos/owner/repo/pulls/1"}
	eng.applyLayer1StatusRefresh("issue_comment", prCommentPayload("owner/repo", 1, pr), cache)

	if n := layer1LookupCalls(client); n != 0 {
		t.Errorf("LookupIssueProjectItem calls = %d, want 0", n)
	}
	client.mu.Lock()
	fp := len(client.fetchProjectItemStatusCalls)
	client.mu.Unlock()
	if fp != 0 {
		t.Errorf("FetchProjectItemStatus calls = %d, want 0", fp)
	}
	select {
	case ev := <-events:
		t.Errorf("unexpected log event: %+v", ev)
	default:
	}
}

// TestLayer1PullRequestNullStillLooksUp pins rawPresent: a literal null (or an
// absent key) is not a PR comment.
func TestLayer1PullRequestNullStillLooksUp(t *testing.T) {
	client := offBoardClient()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := layer1Cache(eng, client, "PVT_test", true)

	eng.applyLayer1StatusRefresh("issue_comment", prCommentPayload("owner/repo", 1, nil), cache)
	if n := layer1LookupCalls(client); n != 1 {
		t.Errorf("null pull_request: lookup calls = %d, want 1", n)
	}
	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 2), cache)
	if n := layer1LookupCalls(client); n != 2 {
		t.Errorf("absent pull_request: lookup calls = %d, want 2", n)
	}
}

func TestRawPresent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{{"", false}, {"null", false}, {"  null ", false}, {"  ", false}, {"{}", true}, {`{"url":"x"}`, true}}
	for _, c := range cases {
		if got := rawPresent(json.RawMessage(c.in)); got != c.want {
			t.Errorf("rawPresent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestLayer1OffBoardNegativeCache covers R2: repeats within the TTL make no
// call, expiry re-looks up, and distinct keys are independent.
func TestLayer1OffBoardNegativeCache(t *testing.T) {
	client := offBoardClient()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	clk := &layer1TestClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	eng.SetClock(clk)
	cache := layer1Cache(eng, client, "PVT_test", true)

	p1 := issueEventPayload("owner/repo", 1)
	eng.applyLayer1StatusRefresh("issues", p1, cache)
	eng.applyLayer1StatusRefresh("issue_comment", p1, cache)
	if n := layer1LookupCalls(client); n != 1 {
		t.Fatalf("within TTL: lookup calls = %d, want 1", n)
	}

	// A different number and a different repo are looked up independently.
	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 2), cache)
	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/other", 1), cache)
	if n := layer1LookupCalls(client); n != 3 {
		t.Fatalf("distinct keys: lookup calls = %d, want 3", n)
	}

	clk.t = clk.t.Add(layer1OffBoardTTL + time.Second)
	eng.applyLayer1StatusRefresh("issues", p1, cache)
	if n := layer1LookupCalls(client); n != 4 {
		t.Errorf("after TTL: lookup calls = %d, want 4", n)
	}
	// Marking swept the expired siblings; only the fresh entry remains.
	eng.layer1OffBoardMu.Lock()
	size := len(eng.layer1OffBoard)
	eng.layer1OffBoardMu.Unlock()
	if size != 1 {
		t.Errorf("negative cache size = %d, want 1 after sweep", size)
	}
}

// TestLayer1OffBoardErrorNotCached: a failed lookup is retried on the next event.
func TestLayer1OffBoardErrorNotCached(t *testing.T) {
	client := &mockGitHubClient{
		lookupIssueProjectItemFn: func(string, string, int) (string, string, error) {
			return "", "", errors.New("502")
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := layer1Cache(eng, client, "PVT_test", true)

	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)
	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)
	if n := layer1LookupCalls(client); n != 2 {
		t.Errorf("lookup calls = %d, want 2 (errors must not be cached)", n)
	}
}

// TestLayer1PositiveResultLeavesNoNegativeEntry: a positive lookup registers
// the item and clears any stale negative entry.
func TestLayer1PositiveResultLeavesNoNegativeEntry(t *testing.T) {
	found := false
	client := &mockGitHubClient{
		lookupIssueProjectItemFn: func(string, string, int) (string, string, error) {
			if found {
				return "PVTI_1", "Research", nil
			}
			return "", "", nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := layer1Cache(eng, client, "PVT_test", true)

	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)
	eng.layer1OffBoardClearAll() // board-add, so the next event looks up again
	found = true
	eng.applyLayer1StatusRefresh("issues", issueEventPayload("owner/repo", 1), cache)

	if id, ok := cache.GetItemID(boardcache.ItemKey("owner/repo", 1)); !ok || id != "PVTI_1" {
		t.Errorf("GetItemID = %q ok=%v, want PVTI_1", id, ok)
	}
	eng.layer1OffBoardMu.Lock()
	size := len(eng.layer1OffBoard)
	eng.layer1OffBoardMu.Unlock()
	if size != 0 {
		t.Errorf("negative cache size = %d, want 0", size)
	}
}

// TestLayer1OffBoardClearedOnBoardAdd: a projects_v2_item "created" event
// clears the negative entries, so the next event looks up again. Other actions
// do not.
func TestLayer1OffBoardClearedOnBoardAdd(t *testing.T) {
	client := offBoardClient()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := layer1Cache(eng, client, "PVT_test", true)
	p := issueEventPayload("owner/repo", 1)

	eng.applyLayer1StatusRefresh("issues", p, cache)
	eng.applyLayer1StatusRefresh("projects_v2_item", projectsV2ItemPayload("edited"), cache)
	eng.applyLayer1StatusRefresh("issues", p, cache)
	if n := layer1LookupCalls(client); n != 1 {
		t.Fatalf("after non-created item event: lookup calls = %d, want 1", n)
	}

	eng.applyLayer1StatusRefresh("projects_v2_item", projectsV2ItemPayload("created"), cache)
	eng.applyLayer1StatusRefresh("issues", p, cache)
	if n := layer1LookupCalls(client); n != 2 {
		t.Errorf("after created: lookup calls = %d, want 2", n)
	}
}

// TestLayer1OffBoardBypassedOnceItemIDKnown: when the cache gains an itemID
// despite an unexpired negative entry, the fast path is taken.
func TestLayer1OffBoardBypassedOnceItemIDKnown(t *testing.T) {
	client := offBoardClient()
	client.fetchProjectItemStatusFn = func(string) (string, error) { return "Plan", nil }
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := layer1Cache(eng, client, "PVT_test", true)
	p := issueEventPayload("owner/repo", 1)

	eng.applyLayer1StatusRefresh("issues", p, cache)
	key := boardcache.ItemKey("owner/repo", 1)
	cache.RegisterItemID(key, "PVTI_9")
	eng.applyLayer1StatusRefresh("issues", p, cache)

	if n := layer1LookupCalls(client); n != 1 {
		t.Errorf("lookup calls = %d, want 1", n)
	}
	client.mu.Lock()
	fp := len(client.fetchProjectItemStatusCalls)
	client.mu.Unlock()
	if fp != 1 {
		t.Errorf("FetchProjectItemStatus calls = %d, want 1", fp)
	}
}

// TestLayer1OffBoardConcurrent exercises the mutex under -race.
func TestLayer1OffBoardConcurrent(t *testing.T) {
	eng := testEngine(t, offBoardClient(), &mockClaudeInvoker{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := layer1OffBoardKey("P", "o/r", g*1000+i)
				eng.layer1OffBoardMark(k)
				eng.layer1OffBoardHit(k)
				if i%50 == 0 {
					eng.layer1OffBoardClearAll()
				}
				eng.layer1OffBoardClear(k)
			}
		}(g)
	}
	wg.Wait()
}
