package boardcache

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// engineWriteCache returns a cache holding owner/repo#1 (Status Research,
// labels l1) and the board listing that mirrors it.
func engineWriteCache(t *testing.T) (*CacheImpl, *mockClient, *gh.ProjectBoard) {
	t.Helper()
	t1 := time.Now().Add(-time.Hour).Truncate(time.Second)
	board := &gh.ProjectBoard{
		ProjectID: "PID",
		Items: []gh.ProjectItem{
			{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research", Labels: []string{"l1"}, UpdatedAt: t1},
		},
	}
	mc := &mockClient{projectBoardResult: board}
	store := itemstate.NewStore(nil)
	store.SetEngineWriteGraceForTest(time.Minute)
	c := NewCacheImpl(mc, store, nopLog)
	testBootstrapFromBoard(c, board)
	return c, mc, board
}

func TestLightReconcile_PreWriteListingIsNotDriftAndRevertsNothing(t *testing.T) {
	c, mc, board := engineWriteCache(t)
	// The engine writes, then LightReconcile receives a listing that still
	// shows the pre-write labels (GitHub's board-listing lag, #1974).
	c.ApplyLabelAdded("owner/repo#1", "stage:Review:complete")
	c.ApplyLabelRemoved("owner/repo#1", "l1")
	mc.projectBoardResult = board // pre-write labels: l1, no stage label

	drift, keys, fresh, err := c.LightReconcile("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatal(err)
	}
	// FetchedAt is stamped at fetch start, after the writes above but within the
	// grace margin, so the listing is still treated as possibly stale.
	if drift != 0 {
		t.Fatalf("drift=%d keys=%v, want 0", drift, keys)
	}
	if fresh.FetchedAt.IsZero() {
		t.Fatal("LightReconcile did not stamp FetchedAt")
	}
	c.Reconcile(fresh)
	s := testGetState(t, c, "owner/repo", 1)
	if !containsStr(s.Labels, "stage:Review:complete") || containsStr(s.Labels, "ext") {
		t.Fatalf("labels = %v, engine writes reverted", s.Labels)
	}
}

func TestLightReconcile_WriteDuringFetchWindow(t *testing.T) {
	c, mc, board := engineWriteCache(t)
	// The engine writes while the fetch is in flight.
	mc.fetchProjectBoardHook = func() { c.ApplyLabelAdded("owner/repo#1", "fabrik:awaiting-landing-verification") }
	mc.projectBoardResult = board

	drift, keys, fresh, err := c.LightReconcile("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatal(err)
	}
	if drift != 0 {
		t.Fatalf("drift=%d keys=%v, want 0", drift, keys)
	}
	c.Reconcile(fresh)
	if s := testGetState(t, c, "owner/repo", 1); !containsStr(s.Labels, "fabrik:awaiting-landing-verification") {
		t.Fatalf("label reverted: %v", s.Labels)
	}
}

func TestLightReconcile_ExternalChangeStillDrifts(t *testing.T) {
	c, mc, board := engineWriteCache(t)
	c.ApplyLabelAdded("owner/repo#1", "fabrik:paused")
	// A label that reached the cache by webhook, never an engine write.
	c.store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "ext", Sender: "alice"})

	// A human removed it in the UI: the listing lacks it, although the engine
	// never wrote it, and carries a newer updatedAt.
	b := *board
	b.Items = []gh.ProjectItem{{ID: "I_1", Number: 1, Repo: "owner/repo", Status: "Research", Labels: nil, UpdatedAt: time.Now()}}
	mc.projectBoardResult = &b

	b.Items[0].Status = "Plan" // a gate-relevant difference the engine never wrote
	drift, _, fresh, err := c.LightReconcile("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatal(err)
	}
	if drift != 1 {
		t.Fatalf("drift=%d, want 1 for genuine external difference", drift)
	}
	c.Reconcile(fresh)
	s := testGetState(t, c, "owner/repo", 1)
	if s.Status != "Plan" || containsStr(s.Labels, "ext") {
		t.Fatalf("external change not applied: status=%q labels=%v", s.Status, s.Labels)
	}
	if !containsStr(s.Labels, "fabrik:paused") {
		t.Fatalf("shielded engine write lost alongside real drift: %v", s.Labels)
	}
}

func TestReconcile_ListingFetchedAfterGraceContradictsWrite(t *testing.T) {
	c, _, board := engineWriteCache(t)
	c.ApplyLabelAdded("owner/repo#1", "fabrik:paused")
	b := *board
	b.FetchedAt = time.Now().Add(2 * time.Minute) // after write + grace
	c.Reconcile(&b)
	if s := testGetState(t, c, "owner/repo", 1); containsStr(s.Labels, "fabrik:paused") {
		t.Fatalf("contradicting later listing not applied: %v", s.Labels)
	}
}

func TestReconcile_ZeroFetchedAtKeepsLegacyBehaviour(t *testing.T) {
	c, _, board := engineWriteCache(t)
	c.ApplyLabelAdded("owner/repo#1", "fabrik:paused")
	b := *board // FetchedAt zero
	c.Reconcile(&b)
	if s := testGetState(t, c, "owner/repo", 1); containsStr(s.Labels, "fabrik:paused") {
		t.Fatalf("zero FetchedAt must not protect: %v", s.Labels)
	}
}

func TestApplyIssueClosed_SurvivesPreWriteListing(t *testing.T) {
	c, _, board := engineWriteCache(t)
	fetched := time.Now()
	c.ApplyIssueClosed("owner/repo#1")
	b := *board
	b.FetchedAt = fetched
	c.Reconcile(&b)
	if s := testGetState(t, c, "owner/repo", 1); !s.IsClosed {
		t.Fatal("close reverted by pre-write listing")
	}
}
