package engine

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Acceptance tests for the per-layer cache freshness rule (#2180, ADR 2180),
// driven through runProbeAndDeepFetch like the #1090 tests in
// terminal_selfwrite_test.go. Each masked-layer test lands an external change
// whose updatedAt sits just BEFORE an engine self-write and asserts the next
// probe cycle sees it; each fails on the pre-#2180 single-baseline rule.

// layerFixture warms owner/repo#1 with a deep fetch at baseTime that captured
// the layered board evidence: no issue comments, a linked PR last touched at
// baseTime. fetchCalls counts every FetchItemDetails that reaches GitHub; the
// client reports `commentsOnGitHub` issue comments on each fetch.
type layerFixture struct {
	eng              *Engine
	cache            *boardcache.CacheImpl
	client           *mockGitHubClient
	item             gh.ProjectItem
	baseTime         time.Time
	fetchCalls       *int
	commentsOnGitHub *int
}

func newLayerFixture(t *testing.T) *layerFixture {
	t.Helper()
	baseTime := time.Now().Add(-time.Hour)
	calls, comments := 0, 0
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			calls++
			if item.UpdatedAt.IsZero() {
				item.UpdatedAt = baseTime
			}
			item.Comments = nil
			for i := 1; i <= comments; i++ {
				item.Comments = append(item.Comments, gh.Comment{DatabaseID: i * 1000})
			}
			return nil
		},
		addCommentFn: func(owner, repo string, issueNumber int, body string) (int, error) { return 99, nil },
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Number: 1, Repo: "owner/repo", ItemID: "PVTI_001", LinkedPRNumber: 7,
		UpdatedAt: baseTime, IssueLayerUpdatedAt: baseTime, LinkedPRUpdatedAt: baseTime,
		IssueCommentTotal: 0, IssueCommentTotalKnown: true,
	}
	if err := cache.FetchItemDetails(&item); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if calls != 1 {
		t.Fatalf("warm-up made %d fetches, want 1", calls)
	}
	return &layerFixture{eng: eng, cache: cache, client: client, item: item, baseTime: baseTime,
		fetchCalls: &calls, commentsOnGitHub: &comments}
}

// probe runs one probe cycle that reports the given board evidence.
func (f *layerFixture) probe(ev gh.BoardProbeItem) {
	ev.ItemID, ev.ContentID, ev.Number, ev.Repo, ev.Status = "PVTI_001", "I_001", 1, "owner/repo", "Research"
	f.client.probeProjectBoardFn = func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
		return []gh.BoardProbeItem{ev}, "PVT_1", nil
	}
	f.eng.runProbeAndDeepFetch(f.cache)
}

// A human comment on a non-paused item with an updatedAt just before the
// engine's own comment is seen by the next probe cycle, once.
func TestCacheFreshness_HumanCommentBeforeSelfWrite_SeenWithinOnePoll(t *testing.T) {
	f := newLayerFixture(t)

	f.eng.postItemComment(f.item, "🏭 **Fabrik** status", false) // the self-write
	humanAt := time.Now().Add(-2 * time.Second)                 // just before the write's baseline
	*f.commentsOnGitHub = 2                                     // the engine's comment and the human's

	f.probe(gh.BoardProbeItem{
		EffectiveUpdatedAt: humanAt, IssueLayerUpdatedAt: humanAt, LinkedPRUpdatedAt: f.baseTime, LinkedPRNumber: 7,
		IssueCommentTotal: 2, IssueCommentTotalKnown: true,
	})
	if *f.fetchCalls != 2 {
		t.Fatalf("the human comment was masked by the self-write baseline: %d fetches, want 2", *f.fetchCalls)
	}
	snap, _ := f.eng.store.Get("owner/repo", 1)
	if got := len(snap.State().Comments); got != 2 {
		t.Errorf("dispatcher view holds %d issue comments, want 2", got)
	}

	// Settled: the same board reading costs nothing more.
	f.probe(gh.BoardProbeItem{
		EffectiveUpdatedAt: humanAt, IssueLayerUpdatedAt: humanAt, LinkedPRUpdatedAt: f.baseTime, LinkedPRNumber: 7,
		IssueCommentTotal: 2, IssueCommentTotalKnown: true,
	})
	if *f.fetchCalls != 2 {
		t.Errorf("a settled item was refetched: %d fetches, want 2", *f.fetchCalls)
	}
}

// A review-thread comment (PR layer) with an updatedAt just before an
// issue-side self-write is seen: the PR layer is never self-write anchored.
func TestCacheFreshness_PRThreadCommentBeforeSelfWrite_SeenWithinOnePoll(t *testing.T) {
	f := newLayerFixture(t)

	f.eng.addLabel(f.item, "fabrik:awaiting-input") // issue-side self-write
	threadAt := time.Now().Add(-2 * time.Second)

	f.probe(gh.BoardProbeItem{
		EffectiveUpdatedAt: threadAt, IssueLayerUpdatedAt: f.baseTime, LinkedPRUpdatedAt: threadAt,
		LinkedPRNumber: 7, IssueCommentTotal: 0, IssueCommentTotalKnown: true,
	})
	if *f.fetchCalls != 2 {
		t.Fatalf("the PR-layer change was masked by an issue-side self-write: %d fetches, want 2", *f.fetchCalls)
	}
	snap, _ := f.eng.store.Get("owner/repo", 1)
	if !snap.State().LastSeenPRUpdatedAt.Equal(threadAt) {
		t.Errorf("PR baseline = %v, want %v", snap.State().LastSeenPRUpdatedAt, threadAt)
	}
}

// CI: a cached PENDING check-run set that completed on GitHub just before an
// engine self-write is not served from the cache. Check completion moves no
// updatedAt, so this is the cache-trust path, not the baseline.
func TestCacheFreshness_CompletedChecksBeforeSelfWrite_SeenByReadClient(t *testing.T) {
	f := newLayerFixture(t)
	f.eng.store.Apply(itemstate.CheckRunCompleted{
		Repo: "owner/repo", SHA: "sha1",
		Run: gh.CheckRun{ID: 1, Name: "build", Status: "in_progress"},
	})
	live := 0
	f.client.fetchCheckRunsFn = func(owner, repo, sha string) ([]gh.CheckRun, error) {
		live++
		return []gh.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "success"}}, nil
	}
	f.eng.addLabel(f.item, "fabrik:awaiting-ci") // the self-write

	runs, err := f.eng.readClient.FetchCheckRuns("owner", "repo", "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := gh.ClassifyCheckRuns(runs); status != gh.CheckRunsReady {
		t.Fatalf("a completed CI result was masked by the cached PENDING set: status=%v", status)
	}
	if live != 1 {
		t.Errorf("want one live confirmation, got %d", live)
	}
}

// #1090 guarantee: engine self-writes alone — labels, comments, repeated over
// many polls — cause no refetch beyond the warm-up, whatever the poll count.
func TestCacheFreshness_SelfWritesAlone_NoRefetchLoop(t *testing.T) {
	f := newLayerFixture(t)

	for poll := 0; poll < 20; poll++ {
		switch poll % 3 {
		case 0:
			f.eng.addLabel(f.item, "fabrik:awaiting-input")
		case 1:
			f.eng.postItemComment(f.item, "hello", false)
			*f.commentsOnGitHub++
		default:
			f.eng.removeLabel(f.item, "fabrik:awaiting-input")
		}
		// GitHub's updatedAt follows the write; the comment total follows the
		// engine's own comments, which it wrote through the cache.
		snap, _ := f.eng.store.Get("owner/repo", 1)
		at := snap.State().LastSeenSourceUpdatedAt
		f.probe(gh.BoardProbeItem{
			EffectiveUpdatedAt: at, IssueLayerUpdatedAt: at, LinkedPRUpdatedAt: f.baseTime, LinkedPRNumber: 7,
			IssueCommentTotal: len(snap.State().Comments), IssueCommentTotalKnown: true,
		})
	}
	if *f.fetchCalls != 1 {
		t.Errorf("self-writes alone caused %d refetches over 20 polls, want 0", *f.fetchCalls-1)
	}
}
