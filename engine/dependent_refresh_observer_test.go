package engine

import (
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

func seedDeepFetched(t *testing.T, s *itemstate.Store, item gh.ProjectItem) {
	t.Helper()
	s.Apply(itemstate.IssueOpened{Item: item})
	item.UpdatedAt = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.Apply(itemstate.ItemDeepFetched{Repo: item.Repo, Number: item.Number, FreshState: item})
}

func deepFetchValid(t *testing.T, s *itemstate.Store, repo string, n int) bool {
	t.Helper()
	snap, ok := s.Peek(repo, n)
	if !ok {
		t.Fatalf("%s#%d missing", repo, n)
	}
	return !snap.State().LastDeepFetchAt.IsZero()
}

// DependenciesInvalidatedChanged must be a wake and cycle-set flag, otherwise an
// invalidation reaches no one before the next poll (#1995).
func TestDependenciesInvalidatedChanged_IsWakeAndCycleFlag(t *testing.T) {
	if wakeChFlags&itemstate.DependenciesInvalidatedChanged == 0 {
		t.Error("DependenciesInvalidatedChanged must be in wakeChFlags")
	}
	if cycleSetFlags&itemstate.DependenciesInvalidatedChanged == 0 {
		t.Error("DependenciesInvalidatedChanged must be in cycleSetFlags")
	}
	if wakeChFlags&itemstate.DeepFetchChanged != 0 {
		t.Error("DeepFetchChanged must stay out of wakeChFlags: it would widen every DeepFetchInvalidated site")
	}
}

func TestDependenciesInvalidated_WakesAndEntersCycleSet(t *testing.T) {
	store := newTestStore()
	wakeCh := make(chan struct{}, 1)
	var mu sync.Mutex
	set := map[string]bool{}
	store.Subscribe(newWakeChObserver(wakeCh))
	store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	seedDeepFetched(t, store, gh.ProjectItem{Number: 1, Repo: "owner/repo"})
	drainWakeCh(wakeCh)
	mu.Lock()
	set = map[string]bool{}
	mu.Unlock()

	store.Apply(itemstate.DependenciesInvalidated{Repo: "owner/repo", Number: 1})
	select {
	case <-wakeCh:
	default:
		t.Fatal("DependenciesInvalidated must wake the poll")
	}
	mu.Lock()
	defer mu.Unlock()
	if !set["owner/repo#1"] {
		t.Error("DependenciesInvalidated must put the item in the cycle set")
	}
}

func TestDependentRefreshObserver_InvalidatesOnlyDependentsOfTheClosedBlocker(t *testing.T) {
	store := newTestStore()
	seedDeepFetched(t, store, gh.ProjectItem{Number: 1, Repo: "owner/repo"}) // blocker
	// Dependents: blocked (cached edge), unlabelled (cached edge), cross-repo edge,
	// unrelated, and one listing a different issue with the same number.
	seedDeepFetched(t, store, gh.ProjectItem{Number: 2, Repo: "owner/repo", Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{{Number: 1, Repo: "owner/repo"}}})
	seedDeepFetched(t, store, gh.ProjectItem{Number: 3, Repo: "owner/repo",
		BlockedBy: []gh.Dependency{{Number: 1}}})
	seedDeepFetched(t, store, gh.ProjectItem{Number: 1, Repo: "other/repo",
		BlockedBy: []gh.Dependency{{Number: 1, Repo: "owner/repo"}}})
	seedDeepFetched(t, store, gh.ProjectItem{Number: 4, Repo: "owner/repo"})
	seedDeepFetched(t, store, gh.ProjectItem{Number: 5, Repo: "owner/repo",
		BlockedBy: []gh.Dependency{{Number: 1, Repo: "third/repo"}}})
	store.Subscribe(&DependentRefreshObserver{Store: store})

	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 1})

	for _, k := range []struct {
		repo string
		n    int
	}{{"owner/repo", 2}, {"owner/repo", 3}, {"other/repo", 1}} {
		if deepFetchValid(t, store, k.repo, k.n) {
			t.Errorf("%s#%d lists the closed blocker and must be invalidated", k.repo, k.n)
		}
	}
	for _, k := range []int{4, 5} {
		if !deepFetchValid(t, store, "owner/repo", k) {
			t.Errorf("owner/repo#%d does not list the closed blocker and must be untouched", k)
		}
	}
}

func TestDependentRefreshObserver_IgnoresOpenAndNonCloseChanges(t *testing.T) {
	store := newTestStore()
	seedDeepFetched(t, store, gh.ProjectItem{Number: 1, Repo: "owner/repo"})
	seedDeepFetched(t, store, gh.ProjectItem{Number: 2, Repo: "owner/repo",
		BlockedBy: []gh.Dependency{{Number: 1}}})
	store.Subscribe(&DependentRefreshObserver{Store: store})

	store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "bug"})
	if !deepFetchValid(t, store, "owner/repo", 2) {
		t.Error("a non-close change on the blocker must not invalidate dependents")
	}
}

// A closed dependent is not re-evaluated.
func TestDependentRefreshObserver_SkipsClosedDependents(t *testing.T) {
	store := newTestStore()
	seedDeepFetched(t, store, gh.ProjectItem{Number: 1, Repo: "owner/repo"})
	seedDeepFetched(t, store, gh.ProjectItem{Number: 2, Repo: "owner/repo",
		BlockedBy: []gh.Dependency{{Number: 1}}})
	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 2})
	store.Subscribe(&DependentRefreshObserver{Store: store})

	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 1})
	if !deepFetchValid(t, store, "owner/repo", 2) {
		t.Error("a closed dependent must be left alone")
	}
}
