package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

var errTestAPIFailure = errors.New("api error")

// Acceptance-level tests for the SelfWriteObserved probe staleness baseline
// (#1090), exercised through the full runProbeAndDeepFetch path rather than
// against the Store directly (see mutate_test.go / comments_selfwrite_test.go
// for the lower-level unit tests). Each test warms a deep-fetch cache entry
// at baseTime, performs one self-write category, then runs a probe cycle and
// asserts whether a further deep-fetch occurs.

// warmDeepFetchAt seeds owner/repo#1 with a deep fetch that reports UpdatedAt
// = baseTime, populating both LastDeepFetchAt and LastSeenSourceUpdatedAt so
// the subsequent staleness comparison is meaningful (a never-deep-fetched
// item is always treated as stale, which would make every test trivially
// pass regardless of SelfWriteObserved).
func warmDeepFetchAt(t *testing.T, cache *boardcache.CacheImpl, baseTime time.Time) gh.ProjectItem {
	t.Helper()
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo", ItemID: "PVTI_001"}
	if err := cache.FetchItemDetails(&item); err != nil {
		t.Fatalf("warm-up FetchItemDetails: %v", err)
	}
	if item.UpdatedAt.IsZero() || !item.UpdatedAt.Equal(baseTime) {
		t.Fatalf("warm-up did not record baseTime: got %v, want %v", item.UpdatedAt, baseTime)
	}
	return item
}

// newWarmableClient returns a client whose FetchItemDetails always reports
// baseTime, and a pointer to the call counter so tests can assert whether the
// probe cycle triggered an additional deep-fetch beyond the warm-up call.
func newWarmableClient(baseTime time.Time) (*mockGitHubClient, *int) {
	calls := 0
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			calls++
			item.UpdatedAt = baseTime
			return nil
		},
	}
	return client, &calls
}

func TestRunProbeAndDeepFetch_SelfWriteLabelAdd_SuppressesRefetch(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := warmDeepFetchAt(t, cache, baseTime)
	if *calls != 1 {
		t.Fatalf("expected 1 warm-up deep-fetch, got %d", *calls)
	}

	eng.addLabel(item, "fabrik:awaiting-input")
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)
	if !afterSelfWrite.After(baseTime) {
		t.Fatalf("expected baseline to advance past baseTime after label add, got %v", afterSelfWrite)
	}

	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: afterSelfWrite},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 1 {
		t.Errorf("expected no additional deep-fetch after a self-write with unchanged external state; total calls = %d", *calls)
	}
}

func TestRunProbeAndDeepFetch_SelfWriteLabelRemove_SuppressesRefetch(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := warmDeepFetchAt(t, cache, baseTime)
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), "fabrik:blocked")

	eng.removeLabel(item, "fabrik:blocked")
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)
	if !afterSelfWrite.After(baseTime) {
		t.Fatalf("expected baseline to advance past baseTime after label removal, got %v", afterSelfWrite)
	}

	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: afterSelfWrite},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 1 {
		t.Errorf("expected no additional deep-fetch after a self-write with unchanged external state; total calls = %d", *calls)
	}
}

func TestRunProbeAndDeepFetch_SelfWriteCommentPost_SuppressesRefetch(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	client.addCommentFn = func(owner, repo string, issueNumber int, body string) (int, error) {
		return 99, nil
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := warmDeepFetchAt(t, cache, baseTime)

	eng.postItemComment(item, "hello", false)
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)
	if !afterSelfWrite.After(baseTime) {
		t.Fatalf("expected baseline to advance past baseTime after comment post, got %v", afterSelfWrite)
	}

	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: afterSelfWrite},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 1 {
		t.Errorf("expected no additional deep-fetch after a self-write with unchanged external state; total calls = %d", *calls)
	}
}

func TestRunProbeAndDeepFetch_SelfWriteBodyEdit_SuppressesRefetch(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := warmDeepFetchAt(t, cache, baseTime)
	stage := &stages.Stage{Name: "Research"}

	eng.publishCommentOutput("owner", "repo", item, stage, nil, bodyOnlyOutput, "", "main")
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)
	if !afterSelfWrite.After(baseTime) {
		t.Fatalf("expected baseline to advance past baseTime after issue body edit, got %v", afterSelfWrite)
	}

	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: afterSelfWrite},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 1 {
		t.Errorf("expected no additional deep-fetch after a self-write with unchanged external state; total calls = %d", *calls)
	}
}

func TestRunProbeAndDeepFetch_SelfWriteStatusMove_SuppressesRefetch(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	client.updateProjectItemStatusFn = func(projectID, itemID, fieldID, optionID string) error {
		return nil
	}
	stgs := testStagesWithValidate()
	eng := testEngineWithStages(t, client, stgs)

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"},
		},
	})
	eng.readClient = cache
	item := warmDeepFetchAt(t, cache, baseTime)
	item.Status = "Research"

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	if err := eng.advanceToNextStage(board, item, stgs[0]); err != nil {
		t.Fatalf("advanceToNextStage: %v", err)
	}
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)
	if !afterSelfWrite.After(baseTime) {
		t.Fatalf("expected baseline to advance past baseTime after status move, got %v", afterSelfWrite)
	}

	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Plan", EffectiveUpdatedAt: afterSelfWrite},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 1 {
		t.Errorf("expected no additional deep-fetch after a self-write with unchanged external state; total calls = %d", *calls)
	}
}

// TestRunProbeAndDeepFetch_SelfWrite_ExternalChangeAfter_StillRefetches
// verifies the "must not mask a genuine external change" requirement: a
// probe reporting an EffectiveUpdatedAt strictly later than the self-write's
// recorded baseline (e.g. a human comment landing right after our reply)
// must still trigger a deep-fetch.
func TestRunProbeAndDeepFetch_SelfWrite_ExternalChangeAfter_StillRefetches(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := warmDeepFetchAt(t, cache, baseTime)

	eng.addLabel(item, "fabrik:awaiting-input")
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)

	externalChange := afterSelfWrite.Add(time.Minute)
	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: externalChange},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 2 {
		t.Errorf("expected a deep-fetch for the genuine external change after our self-write; total calls = %d, want 2", *calls)
	}
}

// TestRunProbeAndDeepFetch_SelfWrite_FailedMutation_StillTreatedStale verifies
// that a failed self-write (the GitHub call itself errors) does not advance
// the baseline — so a subsequent probe reporting the timestamp a successful
// mutation *would* have produced is still correctly treated as stale.
func TestRunProbeAndDeepFetch_SelfWrite_FailedMutation_StillTreatedStale(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	client.addLabelToIssueFn = func(owner, repo string, issueNumber int, labelName string) error {
		return errTestAPIFailure
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := warmDeepFetchAt(t, cache, baseTime)

	eng.addLabel(item, "fabrik:awaiting-input")
	if got := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1); !got.Equal(baseTime) {
		t.Fatalf("expected baseline unchanged (still baseTime) after failed label add, got %v", got)
	}

	// Simulate what GitHub's real updatedAt would be if the label add had
	// actually succeeded (i.e. what an external observer — or a retried,
	// successful mutation — would report).
	simulatedRealUpdate := baseTime.Add(time.Minute)
	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: simulatedRealUpdate},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 2 {
		t.Errorf("expected the probe to still treat the item as stale after a failed self-write; total calls = %d, want 2", *calls)
	}
}

// TestRunProbeAndDeepFetch_SelfWrite_SuppressesRefetch_PollingOnlyMode
// verifies the "must work identically in polling-only mode" requirement: the
// baseline advance must not be gated on e.webhookMgr.
func TestRunProbeAndDeepFetch_SelfWrite_SuppressesRefetch_PollingOnlyMode(t *testing.T) {
	baseTime := time.Now().Add(-time.Hour)
	client, calls := newWarmableClient(baseTime)
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.webhookMgr = nil // polling-only mode
	item := warmDeepFetchAt(t, cache, baseTime)

	eng.addLabel(item, "fabrik:awaiting-input")
	afterSelfWrite := lastSeenSourceUpdatedAt(t, eng, "owner/repo", 1)
	if !afterSelfWrite.After(baseTime) {
		t.Fatalf("expected baseline to advance past baseTime in polling-only mode, got %v", afterSelfWrite)
	}

	client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{
			{ItemID: "PVTI_001", ContentID: "I_001", Number: 1, Repo: "owner/repo",
				Status: "Research", EffectiveUpdatedAt: afterSelfWrite},
		}, "PVT_1", nil
	}
	eng.runProbeAndDeepFetch(cache)

	if *calls != 1 {
		t.Errorf("expected no additional deep-fetch in polling-only mode after a self-write with unchanged external state; total calls = %d", *calls)
	}
}
