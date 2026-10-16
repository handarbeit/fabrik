package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// #2346: an engine issue-body write must reach the Store in the same step that
// advances the freshness baseline, or a poll-only daemon (no issues.edited echo)
// serves the pre-edit body to the next invocation's .fabrik-context/issue.md.

const (
	bodyWTRepo   = "owner/repo"
	bodyWTNumber = 60
	bodyWTOld    = "old spec"
	bodyWTNew    = "updated spec body" // the body inside bodyOnlyOutput
)

// bodyWTEngine returns a cache-backed engine whose Store already holds a deep
// fetch of the item at the returned GitHub updatedAt, with an old body.
func bodyWTEngine(t *testing.T, client *mockGitHubClient) (*Engine, *boardcache.CacheImpl, time.Time) {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache, _ := eng.UseBoardCacheForTest()
	updatedAt := time.Now().Add(-time.Hour)
	pi := gh.ProjectItem{
		ID: "I_60", ItemID: "PVTI_60", Number: bodyWTNumber, Repo: bodyWTRepo, Status: "Specify",
		Title: "Title", Body: bodyWTOld, UpdatedAt: updatedAt, IssueLayerUpdatedAt: updatedAt,
	}
	if _, _, err := eng.store.Apply(itemstate.IssueOpened{Item: pi}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := eng.store.Apply(itemstate.ItemDeepFetched{Repo: bodyWTRepo, Number: bodyWTNumber, FreshState: pi}); err != nil {
		t.Fatalf("deep fetch: %v", err)
	}
	return eng, cache, updatedAt
}

// nextInvocationIssueMD mimics the poll's cache read followed by the context
// file write the next stage invocation relies on, and returns issue.md.
func nextInvocationIssueMD(t *testing.T, eng *Engine, cache *boardcache.CacheImpl, updatedAt time.Time) string {
	t.Helper()
	item := gh.ProjectItem{Number: bodyWTNumber, Repo: bodyWTRepo, UpdatedAt: updatedAt, IssueLayerUpdatedAt: updatedAt}
	if !cache.IsItemCacheFresh(bodyWTRepo, bodyWTNumber, boardcache.EvidenceFromItem(&item)) {
		t.Fatal("precondition: cache must read as fresh (cache-hit path) so the test is not vacuous")
	}
	if err := eng.readClient.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	workDir := t.TempDir()
	eng.writeContextFiles(item, &stages.Stage{Name: "Research", Order: 1}, workDir, false)
	data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "issue.md"))
	if err != nil {
		t.Fatalf("issue.md: %v", err)
	}
	return string(data)
}

func TestPublishCommentOutput_BodyEditReachesNextContextFile(t *testing.T) {
	var wrote string
	client := &mockGitHubClient{updateIssueBodyFn: func(_, _ string, _ int, body string) error { wrote = body; return nil }}
	eng, cache, updatedAt := bodyWTEngine(t, client)
	item := gh.ProjectItem{Number: bodyWTNumber, Repo: bodyWTRepo}

	eng.publishCommentOutput("owner", "repo", item, &stages.Stage{Name: "Specify"}, nil, bodyOnlyOutput, "", "main")

	if wrote != bodyWTNew {
		t.Fatalf("UpdateIssueBody body = %q; want %q", wrote, bodyWTNew)
	}
	if got := nextInvocationIssueMD(t, eng, cache, updatedAt); got != bodyWTNew {
		t.Errorf("issue.md = %q; want the updated body %q (no webhook delivered)", got, bodyWTNew)
	}
	snap, _ := eng.store.Get(bodyWTRepo, bodyWTNumber)
	if snap.State().Title != "Title" {
		t.Errorf("Title = %q; body write-through must not touch it", snap.State().Title)
	}
}

// Non-vacuity: with the Store write-through disabled the baseline still moves
// past the edit, so the cache serves the old body — the #2346 defect.
func TestPublishCommentOutput_BodyEditWithoutWriteThroughServesStaleBody(t *testing.T) {
	eng, cache, updatedAt := bodyWTEngine(t, &mockGitHubClient{})
	eng.SetIssueBodyWriteThroughDisabledForTest(true)
	item := gh.ProjectItem{Number: bodyWTNumber, Repo: bodyWTRepo}

	eng.publishCommentOutput("owner", "repo", item, &stages.Stage{Name: "Specify"}, nil, bodyOnlyOutput, "", "main")

	if got := nextInvocationIssueMD(t, eng, cache, updatedAt); got != bodyWTOld {
		t.Errorf("issue.md = %q; with the write-through off the stale %q is expected (non-vacuity)", got, bodyWTOld)
	}
}

func TestUpdateIssueBody_FailureLeavesStoreAndBaselineUnchanged(t *testing.T) {
	client := &mockGitHubClient{updateIssueBodyFn: func(_, _ string, _ int, _ string) error { return errors.New("api error") }}
	eng, _, _ := bodyWTEngine(t, client)
	before, _ := eng.store.Get(bodyWTRepo, bodyWTNumber)

	err := eng.updateIssueBody(gh.ProjectItem{Number: bodyWTNumber, Repo: bodyWTRepo}, "owner", "repo", bodyWTNew)
	if err == nil {
		t.Fatal("want the UpdateIssueBody error returned")
	}
	after, _ := eng.store.Get(bodyWTRepo, bodyWTNumber)
	if after.State().Body != bodyWTOld {
		t.Errorf("Body = %q; want unchanged %q after a failed write", after.State().Body, bodyWTOld)
	}
	if !after.State().LastSeenSourceUpdatedAt.Equal(before.State().LastSeenSourceUpdatedAt) {
		t.Errorf("baseline moved on a failed write: %v -> %v", before.State().LastSeenSourceUpdatedAt, after.State().LastSeenSourceUpdatedAt)
	}
}

// The stage-output path (item.go) uses the same helper; the helper itself is the
// shared step, so exercise it directly with the item shape processItem passes.
func TestUpdateIssueBody_AppliesBodyAndAdvancesBaselineTogether(t *testing.T) {
	eng, _, _ := bodyWTEngine(t, &mockGitHubClient{})
	before := time.Now()

	if err := eng.updateIssueBody(gh.ProjectItem{Number: bodyWTNumber, Repo: bodyWTRepo}, "owner", "repo", bodyWTNew); err != nil {
		t.Fatalf("updateIssueBody: %v", err)
	}
	snap, _ := eng.store.Get(bodyWTRepo, bodyWTNumber)
	if snap.State().Body != bodyWTNew {
		t.Errorf("Body = %q; want %q", snap.State().Body, bodyWTNew)
	}
	if snap.State().LastSeenSourceUpdatedAt.Before(before) {
		t.Errorf("baseline = %v; want advanced to >= %v", snap.State().LastSeenSourceUpdatedAt, before)
	}
}

func TestUpdateIssueBody_NoCacheStillAdvancesBaseline(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	before := time.Now()
	if err := eng.updateIssueBody(gh.ProjectItem{Number: 61, Repo: bodyWTRepo}, "owner", "repo", bodyWTNew); err != nil {
		t.Fatalf("updateIssueBody: %v", err)
	}
	if got := lastSeenSourceUpdatedAt(t, eng, bodyWTRepo, 61); got.Before(before) {
		t.Errorf("baseline = %v; want advanced to >= %v", got, before)
	}
}
