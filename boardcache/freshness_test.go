package boardcache

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// cachedState builds the ItemState a deep fetch at base leaves behind, then
// applies an engine self-write on top of it (the baseline moves to the local
// clock, which is after base).
func cachedState(base time.Time, prAt time.Time, issueComments int, selfWrite bool) itemstate.ItemState {
	var comments []gh.Comment
	for i := 0; i < issueComments; i++ {
		comments = append(comments, gh.Comment{DatabaseID: i + 1})
	}
	// A linked-PR comment must never count as an issue comment.
	comments = append(comments, gh.Comment{DatabaseID: 9000, FromPR: 7})
	s := itemstate.ItemState{
		LastDeepFetchAt:         base,
		LastSeenSourceUpdatedAt: base,
		LastSeenPRUpdatedAt:     prAt,
		Comments:                comments,
	}
	if selfWrite {
		s.LastSeenSourceUpdatedAt = base.Add(30 * time.Second)
	}
	return s
}

// withCachedPR marks the cached copy as knowing a linked PR.
func withCachedPR(s itemstate.ItemState) itemstate.ItemState {
	s.LinkedPR = &itemstate.LinkedPRState{Number: 7}
	return s
}

// TestCacheIsStale_Layers is the #2180 rule table: each case is a board read
// against a cached copy that an engine self-write has already anchored.
func TestCacheIsStale_Layers(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	justBeforeWrite := base.Add(20 * time.Second) // external change, before the self-write at base+30s
	afterWrite := base.Add(time.Minute)

	cases := []struct {
		name      string
		state     itemstate.ItemState
		ev        Evidence
		wantStale bool
	}{
		{
			name:  "self-write alone, nothing external: fresh",
			state: cachedState(base, base, 2, true),
			ev: Evidence{Overall: base.Add(25 * time.Second), IssueLayer: base.Add(25 * time.Second), PRLayer: base,
				CommentTotal: 2, CommentTotalKnown: true},
		},
		{
			name:  "human comment just before the self-write: stale via comment total",
			state: cachedState(base, base, 2, true),
			ev: Evidence{Overall: justBeforeWrite, IssueLayer: justBeforeWrite, PRLayer: base,
				CommentTotal: 3, CommentTotalKnown: true},
			wantStale: true,
		},
		{
			name:  "PR thread/review change just before an issue-side self-write: stale via PR layer",
			state: cachedState(base, base, 2, true),
			ev: Evidence{Overall: justBeforeWrite, IssueLayer: base, PRLayer: justBeforeWrite,
				CommentTotal: 2, CommentTotalKnown: true},
			wantStale: true,
		},
		{
			name:      "issue-layer change after the self-write: stale",
			state:     cachedState(base, base, 2, true),
			ev:        Evidence{Overall: afterWrite, IssueLayer: afterWrite, PRLayer: base, CommentTotal: 2, CommentTotalKnown: true},
			wantStale: true,
		},
		{
			name:  "comment total unknown: no comment evidence, timestamps only",
			state: cachedState(base, base, 2, true),
			ev:    Evidence{Overall: justBeforeWrite, IssueLayer: justBeforeWrite, PRLayer: base},
		},
		{
			name:  "cached issue comments at the first-page cap: comment check skipped",
			state: cachedState(base, base, issueCommentCap, true),
			ev: Evidence{Overall: base, IssueLayer: base, PRLayer: base,
				CommentTotal: issueCommentCap + 5, CommentTotalKnown: true},
		},
		{
			name:  "a deleted comment lowers the total: stale",
			state: cachedState(base, base, 3, false),
			ev: Evidence{Overall: base, IssueLayer: base, PRLayer: base,
				CommentTotal: 2, CommentTotalKnown: true},
			wantStale: true,
		},
		{
			name:  "no PR baseline captured: falls back to the overall compare (fresh)",
			state: withCachedPR(cachedState(base, time.Time{}, 2, true)),
			ev:    Evidence{Overall: base.Add(25 * time.Second), IssueLayer: base.Add(25 * time.Second), PRLayer: base.Add(25 * time.Second)},
		},
		{
			name:      "no PR baseline captured: overall after the baseline is stale",
			state:     withCachedPR(cachedState(base, time.Time{}, 2, true)),
			ev:        Evidence{Overall: afterWrite, IssueLayer: base, PRLayer: afterWrite},
			wantStale: true,
		},
		{
			// Fetched with no PR; a PR opened and was reviewed before the
			// engine's issue-side self-write, so the overall compare is masked.
			name:  "PR appeared since the fetch, change before the self-write: stale",
			state: cachedState(base, time.Time{}, 2, true),
			ev: Evidence{Overall: justBeforeWrite, IssueLayer: base, PRLayer: justBeforeWrite,
				CommentTotal: 2, CommentTotalKnown: true},
			wantStale: true,
		},
		{
			name:  "no PR cached and none on the board: fresh",
			state: cachedState(base, time.Time{}, 2, true),
			ev:    Evidence{Overall: base.Add(25 * time.Second), IssueLayer: base.Add(25 * time.Second), CommentTotal: 2, CommentTotalKnown: true},
		},
		{
			name:  "layers not captured: overall compare, masked by the self-write as before",
			state: cachedState(base, base, 2, true),
			ev:    Evidence{Overall: justBeforeWrite},
		},
		{
			name:      "never fetched baseline (zero): stale",
			state:     itemstate.ItemState{},
			ev:        Evidence{Overall: base},
			wantStale: true,
		},
		{
			name:  "no signal at all: fresh",
			state: itemstate.ItemState{},
			ev:    Evidence{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheIsStale(tc.ev, tc.state); got != tc.wantStale {
				t.Errorf("cacheIsStale = %v, want %v", got, tc.wantStale)
			}
		})
	}
}

// TestFreshness_SelfWriteAloneNeverRefetches is the #1090 guarantee (#2180
// FR-007): repeated engine self-writes, and a deep fetch that follows each
// external change, never leave the cache stale against an unchanged board.
func TestFreshness_SelfWriteAloneNeverRefetches(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	mc := &mockClient{itemDetailsResult: &gh.ProjectItem{Number: 5, Repo: "owner/repo"}}
	store := itemstate.NewStore(nil)
	c := NewCacheImpl(mc, store, nopLog)
	testBootstrapFromBoard(c, &gh.ProjectBoard{ProjectID: "PID", Items: []gh.ProjectItem{{
		ID: "I_5", Number: 5, Repo: "owner/repo", Status: "Plan", UpdatedAt: t0,
	}}})
	item := gh.ProjectItem{
		ID: "I_5", Number: 5, Repo: "owner/repo", Status: "Plan", UpdatedAt: t0,
		IssueLayerUpdatedAt: t0, IssueCommentTotal: 0, IssueCommentTotalKnown: true,
	}
	if err := c.FetchItemDetails(&item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	baseline := mc.fetchItemDetailsCount

	// The engine posts three comments (written through the cache) and labels
	// the issue; GitHub's updatedAt moves to the write time and its comment
	// total follows.
	for i := 1; i <= 3; i++ {
		store.Apply(itemstate.LocalCommentAdded{Repo: "owner/repo", Number: 5, Comment: gh.Comment{DatabaseID: i}})
		store.Apply(itemstate.SelfWriteObserved{Repo: "owner/repo", Number: 5})
	}
	board := gh.ProjectItem{
		ID: "I_5", Number: 5, Repo: "owner/repo", Status: "Plan",
		UpdatedAt: t0.Add(time.Second), IssueLayerUpdatedAt: t0.Add(time.Second),
		IssueCommentTotal: 3, IssueCommentTotalKnown: true,
	}
	for poll := 0; poll < 10; poll++ {
		if !c.IsItemCacheFresh("owner/repo", 5, EvidenceFromItem(&board)) {
			t.Fatalf("poll %d: self-writes alone made the cache stale", poll)
		}
		probe := board
		if err := c.FetchItemDetails(&probe); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
	}
	if mc.fetchItemDetailsCount != baseline {
		t.Errorf("self-writes caused %d refetches over 10 polls, want 0", mc.fetchItemDetailsCount-baseline)
	}
}

// TestFreshness_ExternalCommentBeforeSelfWrite_RefetchesOnceAndSettles pins
// the human-comment layer end to end through the cache: the external change
// predates the self-write baseline, is seen on the next read, and one refetch
// re-anchors so the following reads are cache hits.
func TestFreshness_ExternalCommentBeforeSelfWrite_RefetchesOnceAndSettles(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	human := gh.Comment{DatabaseID: 11, Author: "someone", Body: "please reconsider"}
	mc := &mockClient{itemDetailsResult: &gh.ProjectItem{Number: 5, Repo: "owner/repo", Comments: []gh.Comment{human}}}
	store := itemstate.NewStore(nil)
	c := NewCacheImpl(mc, store, nopLog)
	testBootstrapFromBoard(c, &gh.ProjectBoard{ProjectID: "PID", Items: []gh.ProjectItem{{
		ID: "I_5", Number: 5, Repo: "owner/repo", Status: "Plan", UpdatedAt: t0,
	}}})
	seed := gh.ProjectItem{
		ID: "I_5", Number: 5, Repo: "owner/repo", Status: "Plan", UpdatedAt: t0,
		IssueLayerUpdatedAt: t0, IssueCommentTotal: 0, IssueCommentTotalKnown: true,
	}
	mc.itemDetailsResult = &gh.ProjectItem{Number: 5, Repo: "owner/repo"}
	if err := c.FetchItemDetails(&seed); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}

	// A human comments at t0+1s; the engine self-writes (a label) at ~now,
	// which moves the baseline past the comment's updatedAt.
	store.Apply(itemstate.SelfWriteObserved{Repo: "owner/repo", Number: 5})
	board := gh.ProjectItem{
		ID: "I_5", Number: 5, Repo: "owner/repo", Status: "Plan",
		UpdatedAt: t0.Add(time.Second), IssueLayerUpdatedAt: t0.Add(time.Second),
		IssueCommentTotal: 1, IssueCommentTotalKnown: true,
	}
	if c.IsItemCacheFresh("owner/repo", 5, EvidenceFromItem(&board)) {
		t.Fatal("a human comment that predates the self-write baseline must make the cache stale")
	}

	mc.itemDetailsResult = &gh.ProjectItem{Number: 5, Repo: "owner/repo", Comments: []gh.Comment{human}}
	before := mc.fetchItemDetailsCount
	read := board
	if err := c.FetchItemDetails(&read); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if len(read.Comments) != 1 || read.Comments[0].DatabaseID != 11 {
		t.Fatalf("the dispatcher did not get the human comment: %+v", read.Comments)
	}
	if mc.fetchItemDetailsCount != before+1 {
		t.Errorf("want exactly one refetch, got %d", mc.fetchItemDetailsCount-before)
	}
	// Settled: further reads are cache hits.
	for i := 0; i < 5; i++ {
		again := board
		if err := c.FetchItemDetails(&again); err != nil {
			t.Fatal(err)
		}
	}
	if mc.fetchItemDetailsCount != before+1 {
		t.Errorf("reads after the refetch must be cache hits, got %d extra fetches", mc.fetchItemDetailsCount-before-1)
	}
}
