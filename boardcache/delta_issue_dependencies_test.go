package boardcache

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

const depRepo = "example-org/fabrik-test-alpha"

func readDepFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

type depChangeLog struct {
	mu      sync.Mutex
	changes []itemstate.Change
}

func (l *depChangeLog) take() []itemstate.Change {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.changes
	l.changes = nil
	return out
}

// depTestCache returns a cache holding #8766 (blocked), #8767 (blocking), an
// unrelated #1, and a same-number issue in another repository, all deep-fetched,
// plus a log of the Store changes made after setup.
func depTestCache(t *testing.T) (*CacheImpl, *depChangeLog, *mockClient) {
	t.Helper()
	mc := &mockClient{}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	testBootstrapFromBoard(c, &gh.ProjectBoard{
		ProjectID: "PID",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 8766, Repo: depRepo, Status: "Implement"},
			{ID: "I_2", ItemID: "PVTI_2", Number: 8767, Repo: depRepo, Status: "Implement"},
			{ID: "I_3", ItemID: "PVTI_3", Number: 1, Repo: depRepo, Status: "Implement"},
			{ID: "I_4", ItemID: "PVTI_4", Number: 8766, Repo: "other-org/other", Status: "Implement"},
		},
	})
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, k := range []struct {
		repo string
		n    int
	}{{depRepo, 8766}, {depRepo, 8767}, {depRepo, 1}, {"other-org/other", 8766}} {
		if _, _, err := c.store.Apply(itemstate.ItemDeepFetched{Repo: k.repo, Number: k.n, FreshState: gh.ProjectItem{
			Repo: k.repo, Number: k.n, UpdatedAt: at,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	log := &depChangeLog{}
	c.store.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		log.mu.Lock()
		log.changes = append(log.changes, ch)
		log.mu.Unlock()
	}))
	return c, log, mc
}

func deepValid(t *testing.T, c *CacheImpl, repo string, n int) bool {
	t.Helper()
	snap, ok := c.store.Peek(repo, n)
	if !ok {
		t.Fatalf("%s#%d not cached", repo, n)
	}
	return !snap.State().LastDeepFetchAt.IsZero()
}

func TestIssueDependencies_AllFixturesInvalidateBothSides(t *testing.T) {
	for _, fx := range []string{
		"issue_dependencies_blocked_by_added",
		"issue_dependencies_blocked_by_removed",
		"issue_dependencies_blocking_added",
		"issue_dependencies_blocking_removed",
	} {
		t.Run(fx, func(t *testing.T) {
			c, log, mc := depTestCache(t)
			c.ApplyDelta("issue_dependencies", readDepFixture(t, fx))

			if deepValid(t, c, depRepo, 8766) || deepValid(t, c, depRepo, 8767) {
				t.Error("both named issues must have their deep cache invalidated")
			}
			if !deepValid(t, c, depRepo, 1) {
				t.Error("an unrelated issue was invalidated")
			}
			if !deepValid(t, c, "other-org/other", 8766) {
				t.Error("a same-number issue in another repository was invalidated")
			}
			woke := map[string]bool{}
			for _, ch := range log.take() {
				if ch.Fields&itemstate.DependenciesInvalidatedChanged != 0 {
					woke[ch.Repo+"#"+strconv.Itoa(ch.Number)] = true
				}
			}
			if len(woke) != 2 || !woke[depRepo+"#8766"] || !woke[depRepo+"#8767"] {
				t.Errorf("wake flag set for %v; want exactly #8766 and #8767", woke)
			}
			if mc.fetchItemDetailsCount != 0 || mc.fetchLabelsCount != 0 {
				t.Error("applying the delivery must not call GitHub")
			}
		})
	}
}

func TestIssueDependencies_SecondDeliveryOfOneChangeIsFree(t *testing.T) {
	c, log, _ := depTestCache(t)
	c.ApplyDelta("issue_dependencies", readDepFixture(t, "issue_dependencies_blocked_by_added"))
	if len(log.take()) == 0 {
		t.Fatal("first delivery changed nothing")
	}
	c.ApplyDelta("issue_dependencies", readDepFixture(t, "issue_dependencies_blocking_added"))
	if got := log.take(); len(got) != 0 {
		t.Errorf("second delivery of the same change produced %d changes; want 0", len(got))
	}
}

func TestIssueDependencies_UncachedIssuesAreIgnoredWithoutFetch(t *testing.T) {
	mc := &mockClient{}
	c := NewCacheImpl(mc, itemstate.NewStore(nil), nopLog)
	testBootstrapFromBoard(c, &gh.ProjectBoard{ProjectID: "PID"})
	c.ApplyDelta("issue_dependencies", readDepFixture(t, "issue_dependencies_blocked_by_removed"))
	if len(c.store.All()) != 0 {
		t.Error("a delivery for uncached issues must not create items")
	}
	if mc.fetchItemDetailsCount != 0 {
		t.Error("no GitHub call is allowed")
	}
}

func TestIssueDependencies_PausedCacheIsNoOp(t *testing.T) {
	c, log, _ := depTestCache(t)
	c.Pause()
	c.ApplyDelta("issue_dependencies", readDepFixture(t, "issue_dependencies_blocked_by_added"))
	if len(log.take()) != 0 || !deepValid(t, c, depRepo, 8766) {
		t.Error("a paused cache must ignore the delivery")
	}
}

func TestIssueDependencies_UnknownActionIgnored(t *testing.T) {
	c, log, _ := depTestCache(t)
	c.ApplyDelta("issue_dependencies", []byte(`{"action":"bogus","blocked_issue":{"number":8766,"html_url":"https://github.com/`+depRepo+`/issues/8766"}}`))
	if len(log.take()) != 0 {
		t.Error("an unknown action must change nothing")
	}
}

func TestIssueDependencies_UnreadableSideDoesNotBlockTheOther(t *testing.T) {
	c, log, _ := depTestCache(t)
	// The blocking side has no number; the blocked side is intact.
	c.ApplyDelta("issue_dependencies", []byte(`{"action":"blocked_by_added",
		"blocked_issue":{"number":8766,"html_url":"https://github.com/`+depRepo+`/issues/8766"},
		"blocking_issue":{"html_url":"https://github.com/`+depRepo+`/issues/8767"}}`))
	log.take()
	if deepValid(t, c, depRepo, 8766) {
		t.Error("readable side must still be invalidated")
	}
	if !deepValid(t, c, depRepo, 8767) {
		t.Error("an unreadable side must not be guessed")
	}
}

func TestIssueDependencies_RepoResolution(t *testing.T) {
	url := func(r string, n string) string { return "https://github.com/" + r + "/issues/" + n }
	cases := []struct {
		name     string
		is       issueDependenciesIssue
		side     string
		wantRepo string
		wantOK   bool
	}{
		{"html_url only", issueDependenciesIssue{Number: 5, HTMLURL: lenientString(url("a/b", "5"))}, "", "a/b", true},
		{"side repo only", issueDependenciesIssue{Number: 5}, "c/d", "c/d", true},
		{"agreeing, case-insensitive", issueDependenciesIssue{Number: 5, HTMLURL: lenientString(url("a/b", "5"))}, "A/B", "a/b", true},
		{"disagreeing", issueDependenciesIssue{Number: 5, HTMLURL: lenientString(url("a/b", "5"))}, "c/d", "", false},
		{"cross-repo side from its own url", issueDependenciesIssue{Number: 9, HTMLURL: lenientString(url("x/y", "9"))}, "x/y", "x/y", true},
		{"no number", issueDependenciesIssue{HTMLURL: lenientString(url("a/b", "5"))}, "", "", false},
		{"no repo", issueDependenciesIssue{Number: 5}, "", "", false},
		{"malformed side repo", issueDependenciesIssue{Number: 5}, "nonsense", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, n, ok := resolveDependencyIssue(tc.is, issueDependenciesRepo{FullName: lenientString(tc.side)})
			if ok != tc.wantOK || repo != tc.wantRepo || (ok && n != int(tc.is.Number)) {
				t.Errorf("got (%q, %d, %v); want (%q, %d, %v)", repo, n, ok, tc.wantRepo, tc.is.Number, tc.wantOK)
			}
		})
	}
}

// Embedded summaries are unordered with respect to the action and must have no
// effect: a delivery whose summaries are wildly wrong behaves exactly like the
// capture.
func TestIssueDependencies_EmbeddedSummariesAreIgnored(t *testing.T) {
	c, _, _ := depTestCache(t)
	c.ApplyDelta("issue_dependencies", []byte(`{"action":"blocked_by_removed",
		"blocked_issue":{"number":8766,"html_url":"https://github.com/`+depRepo+`/issues/8766","state":"closed",
			"issue_dependencies_summary":{"blocked_by":99,"total_blocked_by":99}},
		"blocking_issue":{"number":8767,"html_url":"https://github.com/`+depRepo+`/issues/8767"}}`))
	snap, _ := c.store.Peek(depRepo, 8766)
	if st := snap.State(); st.IsClosed || len(st.BlockedBy) != 0 {
		t.Errorf("embedded data leaked into the cache: closed=%v blockedBy=%v", st.IsClosed, st.BlockedBy)
	}
}
