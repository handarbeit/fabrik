package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// seedOtherThenQueue deep-fetches the numbers into a non-Queued column and then
// applies one shallow listing update per entry, in the given (listing) order,
// moving each to the holding column with the given GitHub Status-value time.
// This is the shape the v0.0.84 gate hit: moves seen together in one pass are
// stamped in listing order, not move order (#2246).
func seedOtherThenQueue(eng *Engine, listing []int, at map[int]time.Time) {
	for _, n := range listing {
		eng.store.Apply(itemstate.ItemDeepFetched{
			Repo: churnTestRepo, Number: n,
			FreshState: gh.ProjectItem{Number: n, Status: "Plan", Repo: churnTestRepo},
		})
	}
	for _, n := range listing {
		eng.store.Apply(itemstate.ShallowBoardItemUpdated{
			Repo: churnTestRepo, Number: n,
			Item: gh.ProjectItem{Number: n, Status: churnTestStatus, Repo: churnTestRepo, StatusAtGitHub: at[n]},
		})
	}
}

func queuedOrder(t *testing.T, eng *Engine) []int {
	t.Helper()
	board, err := eng.readClient.FetchProjectBoard("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	groups := groupQueuedByRepo(board.Items, churnTestStatus, churnTestRepo)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	var out []int
	for _, it := range groups[0].items {
		out = append(out, it.Number)
	}
	return out
}

func newQueueOrderEngine() *Engine {
	eng := NewWithDeps(Config{Owner: "owner", Repo: "repo", MaxConcurrent: 1, Stages: testStages()}, &mockGitHubClient{}, &mockClaudeInvoker{}, nil)
	cache := boardcache.NewCacheImpl(&mockGitHubClient{}, eng.store, func(string, ...any) {})
	// A cached board is only served with a known project ID (#2396).
	cache.NoteProjectID("PVT_test")
	eng.readClient = cache
	return eng
}

// TestQueuedOrder_FollowsGitHubMoveTimeNotListingOrder: moves made as A, B, C, P
// but seen in the listing order A, P, B, C form the batch [A, B, C, P]. The issue
// numbers are chosen so neither listing order nor Number order equals the answer.
func TestQueuedOrder_FollowsGitHubMoveTimeNotListingOrder(t *testing.T) {
	const a, b, c, p = 4, 3, 2, 1
	t0 := time.Date(2026, 1, 14, 21, 43, 0, 0, time.UTC)
	eng := newQueueOrderEngine()
	seedOtherThenQueue(eng, []int{a, p, b, c}, map[int]time.Time{
		a: t0, b: t0.Add(1 * time.Second), c: t0.Add(2 * time.Second), p: t0.Add(3 * time.Second),
	})
	if got, want := queuedOrder(t, eng), []int{a, b, c, p}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestQueuedOrder_SameSecondTiesFallToNumber: GitHub's resolution is one second,
// so equal times carry no order information and fall to Number, independent of
// listing order.
func TestQueuedOrder_SameSecondTiesFallToNumber(t *testing.T) {
	t0 := time.Date(2026, 1, 14, 21, 43, 0, 0, time.UTC)
	eng := newQueueOrderEngine()
	seedOtherThenQueue(eng, []int{4, 1, 3, 2}, map[int]time.Time{1: t0, 2: t0, 3: t0, 4: t0})
	if got, want := queuedOrder(t, eng), []int{1, 2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestQueuedOrder_UnknownGitHubTimeFallsBackToObservation: members with no
// GitHub time are ordered by observation (listing) order; a zero GitHub time is
// never treated as the epoch.
func TestQueuedOrder_UnknownGitHubTimeFallsBackToObservation(t *testing.T) {
	eng := newQueueOrderEngine()
	seedOtherThenQueue(eng, []int{4, 1, 3, 2}, nil)
	got := queuedOrder(t, eng)
	if len(got) != 4 {
		t.Fatalf("order = %v, want 4 members", got)
	}
	// Observation times are time.Now() stamps in listing order; ties (same
	// clock reading) fall to Number, so the result is a total, stable order.
	if again := queuedOrder(t, eng); !reflect.DeepEqual(got, again) {
		t.Fatalf("order unstable: %v then %v", got, again)
	}
}

func TestQueueOrderTime(t *testing.T) {
	gh1 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	obs := gh1.Add(time.Hour)
	if got := queueOrderTime(gh1, obs); !got.Equal(gh1) {
		t.Errorf("known GitHub time = %v, want %v", got, gh1)
	}
	if got := queueOrderTime(time.Time{}, obs); !got.Equal(obs) {
		t.Errorf("unknown GitHub time = %v, want observation %v", got, obs)
	}
	if got := queueOrderTime(time.Time{}, time.Time{}); !got.IsZero() {
		t.Errorf("both unknown = %v, want zero", got)
	}
}

// TestQueueOrderTime_ImplausibleFutureTimeFallsBack: a GitHub time later than the
// observation by more than the skew tolerance is ignored (FR-007); within the
// tolerance it is used.
func TestQueueOrderTime_ImplausibleFutureTimeFallsBack(t *testing.T) {
	obs := time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC)
	near := obs.Add(queueOrderSkewTolerance)
	if got := queueOrderTime(near, obs); !got.Equal(near) {
		t.Errorf("time within tolerance = %v, want %v", got, near)
	}
	far := obs.Add(queueOrderSkewTolerance + time.Second)
	if got := queueOrderTime(far, obs); !got.Equal(obs) {
		t.Errorf("future time = %v, want observation %v", got, obs)
	}
}

// TestSortQueuedGroupsByEntry_MixedKnownAndUnknown: a member with a GitHub time
// and one with only an observed time compare on the effective times.
func TestSortQueuedGroupsByEntry_MixedKnownAndUnknown(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	groups := []queuedRepoGroup{{items: []gh.ProjectItem{
		{Number: 1, StatusEnteredAt: t0.Add(10 * time.Second)},
		{Number: 2, StatusAtGitHub: t0.Add(5 * time.Second), StatusEnteredAt: t0.Add(20 * time.Second)},
		{Number: 3, StatusEnteredAt: t0.Add(1 * time.Second)},
	}}}
	sortQueuedGroupsByEntry(groups)
	var got []int
	for _, it := range groups[0].items {
		got = append(got, it.Number)
	}
	if want := []int{3, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
