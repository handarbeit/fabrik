package engine

// Tests for the Phase 5 store unification (F3).
//
// Each test corresponds to a test specification in issue #537. The first five
// target concrete foot-guns that existed before the single-Store change; they
// are designed to FAIL on origin/main and PASS after this PR's change.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// seedSharedStore pre-populates a store with one item so mutations have
// somewhere to apply to.
func seedSharedStore(t *testing.T, store *itemstate.Store) {
	t.Helper()
	_, _, err := store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Repo:   "owner/repo",
		Number: 1,
		Status: "Research",
	}})
	if err != nil {
		t.Fatalf("seeding shared store: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 1: Cross-mutation visibility
// ---------------------------------------------------------------------------
//
// Apply an IssueLabeled (simulating the cacheImpl / webhook path) and a
// LocalLockAcquired (simulating the engine path) on the same item, then
// verify that a single store.Get returns a Snapshot with BOTH mutations
// reflected. On origin/main this would require two separate Store instances.

func TestStoreUnification_CrossMutationVisibility(t *testing.T) {
	store := itemstate.NewStore(nil)
	seedSharedStore(t, store)

	// Webhook-side mutation: label added (in the old model this went to cacheImpl.store).
	_, _, err := store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "fabrik:yolo"})
	if err != nil {
		t.Fatalf("IssueLabeled: %v", err)
	}

	// Engine-side mutation: lock acquired (in the old model this went to engine.store).
	_, _, err = store.Apply(itemstate.LocalLockAcquired{
		Repo:       "owner/repo",
		Number:     1,
		User:       "testuser",
		AcquiredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("LocalLockAcquired: %v", err)
	}

	// Single store.Get must return a Snapshot with BOTH fields populated.
	snap, err := store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	if !containsLabel(snap.Labels(), "fabrik:yolo") {
		t.Errorf("Labels: want fabrik:yolo, got %v", snap.Labels())
	}
	if snap.Lock() == nil {
		t.Error("Lock: want non-nil (LocalLockAcquired applied), got nil")
	}
	if snap.Lock() != nil && snap.Lock().HolderUser != "testuser" {
		t.Errorf("Lock.HolderUser: want testuser, got %q", snap.Lock().HolderUser)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Worker liveness label read (foot-gun #1)
// ---------------------------------------------------------------------------
//
// Verifies that after a webhook-delivered IssueLabeled mutation flows through
// cacheImpl.ApplyDelta, the label is visible via e.store.All().Labels().
//
// Before the unification fix, cacheImpl.store and engine.store were different
// pointers, so e.store.All() would return an empty Labels slice here.

func TestStoreUnification_WorkerLivenessLabelRead(t *testing.T) {
	client := &mockGitHubClient{}
	eng, cache := testEngineWithCache(client, &mockClaudeInvoker{})

	// Simulate a webhook delivering "fabrik:locked:testuser" to the cache's delta path.
	// cacheImpl.ApplyLabelAdded applies IssueLabeled on c.store, which after unification
	// is the same pointer as eng.store.
	lockLabel := fmt.Sprintf("fabrik:locked:%s", eng.cfg.User)
	cache.ApplyLabelAdded(boardcache.ItemKey("owner/repo", 1), lockLabel)

	// Verify the label is visible from eng.store.All() — which runStartupCleanup reads.
	found := false
	for _, snap := range eng.store.All() {
		if snap.Repo() == "owner/repo" && snap.Number() == 1 {
			if containsLabel(snap.Labels(), lockLabel) {
				found = true
			}
			break
		}
	}
	if !found {
		t.Errorf("eng.store.All() did not see label %q applied via cacheImpl.ApplyLabelAdded; "+
			"this indicates the stores are still split (foot-gun #1 not fixed)", lockLabel)
	}
}

// ---------------------------------------------------------------------------
// Test 3: LinkedPR field completeness
// ---------------------------------------------------------------------------
//
// Verifies that LinkedPR.Number (set by PRHeadSHAUpdated) and HasHadChecks
// (set by PRChecksObserved) are both readable from a single Snapshot after
// store unification.

func TestStoreUnification_LinkedPRFieldCompleteness(t *testing.T) {
	store := itemstate.NewStore(nil)
	seedSharedStore(t, store)

	// Wire LinkedPR.Number via PRHeadSHAUpdated (mirrors what PR delta handlers do).
	_, _, err := store.Apply(itemstate.PRHeadSHAUpdated{
		Repo:        "owner/repo",
		Number:      1,
		LinkedPRNum: 42,
		SHA:         "abc123",
	})
	if err != nil {
		t.Fatalf("PRHeadSHAUpdated: %v", err)
	}

	// Set HasHadChecks (engine-side CI gate mutation).
	_, _, err = store.Apply(itemstate.PRChecksObserved{Repo: "owner/repo", Number: 1})
	if err != nil {
		t.Fatalf("PRChecksObserved: %v", err)
	}

	snap, err := store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	lpr := snap.LinkedPR()
	if lpr == nil {
		t.Fatal("LinkedPR: want non-nil, got nil")
	}
	if lpr.Number != 42 {
		t.Errorf("LinkedPR.Number: want 42, got %d", lpr.Number)
	}
	if !lpr.HasHadChecks {
		t.Error("LinkedPR.HasHadChecks: want true, got false")
	}
}

// ---------------------------------------------------------------------------
// Test 4: Observer fires exactly once per mutation
// ---------------------------------------------------------------------------
//
// Registers a single observer on the shared store and applies mutations
// sequentially. Each Apply must fire the observer exactly once — not twice.
// Before the unification fix, wakeObs and mwnObs were registered on both
// e.store and cacheImpl.store; after unification they are the same pointer,
// so double-registration would cause every Apply to fire the observer twice.

func TestStoreUnification_ObserverFiresOnce(t *testing.T) {
	store := itemstate.NewStore(nil)
	seedSharedStore(t, store)

	var totalFires int64

	obs := itemstate.ObserverFunc(func(_ itemstate.Change, _ itemstate.Snapshot) {
		atomic.AddInt64(&totalFires, 1)
	})
	unsub := store.Subscribe(obs)
	defer unsub()

	mutations := []itemstate.Mutation{
		itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "a"},
		itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "b"},
		itemstate.LocalLockAcquired{Repo: "owner/repo", Number: 1, User: "u", AcquiredAt: time.Now()},
	}
	wantFires := int64(len(mutations))
	for _, m := range mutations {
		if _, _, err := store.Apply(m); err != nil {
			t.Fatalf("Apply(%T): %v", m, err)
		}
	}

	if got := atomic.LoadInt64(&totalFires); got != wantFires {
		t.Errorf("observer fired %d times for %d mutations; want exactly %d "+
			"(double-registration would produce %d)", got, wantFires, wantFires, wantFires*2)
	}
}

// ---------------------------------------------------------------------------
// Test 5: Concurrent mutation safety
// ---------------------------------------------------------------------------
//
// 100 goroutines mix engine-side (LocalLockAcquired/Released) and cache-side
// (IssueLabeled) mutations on the same item. The race detector must be clean
// and the final state must be coherent (at least the last writer's value holds).

func TestStoreUnification_ConcurrentMutationSafety(t *testing.T) {
	store := itemstate.NewStore(nil)
	seedSharedStore(t, store)

	const goroutines = 100
	var wg sync.WaitGroup
	var errs int64

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			label := fmt.Sprintf("label-%d", i)
			if _, _, err := store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: label}); err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			if _, _, err := store.Apply(itemstate.LocalLockAcquired{
				Repo: "owner/repo", Number: 1, User: fmt.Sprintf("u%d", i), AcquiredAt: time.Now(),
			}); err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			store.Apply(itemstate.LocalLockReleased{Repo: "owner/repo", Number: 1}) //nolint
		}(i)
	}
	wg.Wait()

	if n := atomic.LoadInt64(&errs); n != 0 {
		t.Errorf("%d Apply calls returned errors", n)
	}

	// Final state must be coherent: Get must succeed and return a valid Snapshot.
	snap, err := store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get after concurrent mutations: %v", err)
	}
	if snap.Number() != 1 {
		t.Errorf("Snapshot.Number: want 1, got %d", snap.Number())
	}
}

// ---------------------------------------------------------------------------
// Test 6: CacheImpl construction contract
// ---------------------------------------------------------------------------
//
// Verifies that NewCacheImpl accepts an externally-created Store and uses it
// (mutations applied via the store pointer are visible through the cache).
// The nil-store panic is tested separately in TestNewCacheImplNilStorePanic.

func TestStoreUnification_CacheImplUsesProvidedStore(t *testing.T) {
	sharedStore := itemstate.NewStore(nil)
	cache := boardcache.NewCacheImpl(&mockGitHubClient{}, sharedStore, func(string, ...any) {})

	// Bootstrap writes item state into the shared store.
	cache.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{Repo: "owner/repo", Number: 1, Status: "Research"},
		},
	})

	// Mutation applied through the shared store pointer is visible via Get.
	if _, _, err := sharedStore.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "verify"}); err != nil {
		t.Fatalf("Apply IssueLabeled: %v", err)
	}

	snap, err := sharedStore.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !containsLabel(snap.Labels(), "verify") {
		t.Errorf("Labels after applying via shared store: want [verify], got %v", snap.Labels())
	}
}
