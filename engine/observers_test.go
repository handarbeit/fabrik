package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

// newTestStore returns a fresh Store with no fallback fetcher.
func newTestStore() *itemstate.Store {
	return itemstate.NewStore(nil)
}

func drainWakeCh(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// --- newWakeChObserver ---

func TestWakeChObserver_SendsOnStatusChanged(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	defer unsub()

	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Plan"})
	select {
	case <-wakeCh:
	default:
		t.Fatal("expected wake signal on StatusChanged")
	}
}

func TestWakeChObserver_SendsOnLabelsChanged(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	defer unsub()

	store.Apply(itemstate.IssueLabeled{Repo: "owner/repo", Number: 1, Label: "fabrik:yolo"})
	select {
	case <-wakeCh:
	default:
		t.Fatal("expected wake signal on LabelsChanged")
	}
}

func TestWakeChObserver_SendsOnAssigneesChanged(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	defer unsub()

	store.Apply(itemstate.IssueAssigneesUpdated{Repo: "owner/repo", Number: 1, Assignees: []string{"user1"}})
	select {
	case <-wakeCh:
	default:
		t.Fatal("expected wake signal on AssigneesChanged")
	}
}

func TestWakeChObserver_SkipsInvocationChanged(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	defer unsub()

	store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 1, Completed: true})
	select {
	case <-wakeCh:
		t.Fatal("unexpected wake signal on InvocationChanged (not a wakeChFlag)")
	default:
	}
}

func TestWakeChObserver_UnsubscribeStopsSends(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	unsub()

	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Plan"})
	select {
	case <-wakeCh:
		t.Fatal("unexpected wake signal after unsubscribe")
	default:
	}
}

func TestWakeChObserver_NonBlockingWhenFull(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	// Pre-fill the channel so the second send would block without the default branch.
	wakeCh <- struct{}{}
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	defer unsub()

	// Should not deadlock.
	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Plan"})
}

// --- newMayNeedWorkObserver ---

func TestMayNeedWorkObserver_PopulatesKeyOnStatusChanged(t *testing.T) {
	var mu sync.Mutex
	set := make(map[string]bool)
	store := newTestStore()
	unsub := store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	defer unsub()

	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 42, NewStatus: "Plan"})

	mu.Lock()
	ok := set["owner/repo#42"]
	mu.Unlock()
	if !ok {
		t.Fatalf("expected key %q in mayNeedWork set", "owner/repo#42")
	}
}

func TestMayNeedWorkObserver_SkipsInvocationChanged(t *testing.T) {
	var mu sync.Mutex
	set := make(map[string]bool)
	store := newTestStore()
	unsub := store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	defer unsub()

	store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 7, Completed: true})

	mu.Lock()
	n := len(set)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("expected empty mayNeedWork set on InvocationChanged, got %d entries", n)
	}
}

func TestMayNeedWorkObserver_KeyFormat(t *testing.T) {
	var mu sync.Mutex
	set := make(map[string]bool)
	store := newTestStore()
	unsub := store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	defer unsub()

	store.Apply(itemstate.IssueLabeled{Repo: "acme/backend", Number: 99, Label: "fabrik:yolo"})

	want := fmt.Sprintf("%s#%d", "acme/backend", 99)
	mu.Lock()
	ok := set[want]
	mu.Unlock()
	if !ok {
		t.Fatalf("expected key %q in set", want)
	}
}

func TestMayNeedWorkObserver_PopulatesKeyOnAssigneesChanged(t *testing.T) {
	var mu sync.Mutex
	set := make(map[string]bool)
	store := newTestStore()
	unsub := store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	defer unsub()

	store.Apply(itemstate.IssueAssigneesUpdated{Repo: "owner/repo", Number: 1, Assignees: []string{"user1"}})

	mu.Lock()
	ok := set["owner/repo#1"]
	mu.Unlock()
	if !ok {
		t.Fatalf("expected key %q in mayNeedWork set", "owner/repo#1")
	}
}

// TestMayNeedWorkObserver_SkipsWorkerLifecycleChanged verifies that
// WorkerLifecycleChanged (WorkerExited) does NOT populate the cycleSet.
// cycleSetFlags intentionally excludes WorkerLifecycleChanged so that
// early-return goroutine exits do not bypass the cooldown gate (Fix B, issue #576).
func TestMayNeedWorkObserver_SkipsWorkerLifecycleChanged(t *testing.T) {
	var mu sync.Mutex
	set := make(map[string]bool)
	store := newTestStore()
	unsub := store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	defer unsub()

	// Seed WorkerEntered so WorkerExited produces a real change
	// (WorkerExited on a nil worker is a no-op). WorkerEntered emits
	// WorkerChanged|WorkerLifecycleChanged — neither is in cycleSetFlags,
	// so the observer does not fire and the set remains empty.
	store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 10, StageName: "Research", StartedAt: time.Now()})

	store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 10})

	mu.Lock()
	ok := set["owner/repo#10"]
	mu.Unlock()
	if ok {
		t.Fatal("mayNeedWorkObserver must NOT populate cycleSet on WorkerLifecycleChanged (Fix B)")
	}
}

// TestWakeChObserver_FiresOnWorkerLifecycleChanged verifies that the wake channel
// STILL fires on WorkerExited — wakeChFlags retains WorkerLifecycleChanged so
// non-blocked items are re-evaluated promptly after a goroutine exits (Fix B, issue #576).
func TestWakeChObserver_FiresOnWorkerLifecycleChanged(t *testing.T) {
	wakeCh := make(chan struct{}, 1)
	store := newTestStore()
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	defer unsub()

	// Seed WorkerEntered so WorkerExited produces a real WorkerLifecycleChanged.
	store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 11, StageName: "Research", StartedAt: time.Now()})
	drainWakeCh(wakeCh)

	store.Apply(itemstate.WorkerExited{Repo: "owner/repo", Number: 11})
	select {
	case <-wakeCh:
	default:
		t.Fatal("wakeChObserver must fire on WorkerLifecycleChanged (WorkerExited) — wakeChFlags still includes it")
	}
}

// --- InvocationObserver ---

func TestInvocationObserver_FiresJobCompletedEventOnInvocationChanged(t *testing.T) {
	var emitted []tui.Event
	stgs := []*stages.Stage{{Name: "Plan", Model: "sonnet"}}
	obs := &InvocationObserver{
		Stages: stgs,
		Emit: func(e tui.Event) {
			emitted = append(emitted, e)
		},
	}

	store := newTestStore()
	unsub := store.Subscribe(obs)
	defer unsub()

	// Seed status so FindStage can match.
	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 5, NewStatus: "Plan"})
	store.Apply(itemstate.InvocationRecorded{
		Repo:      "owner/repo",
		Number:    5,
		Completed: true,
		Blocked:   false,
		IsComment: true,
		Duration:  30 * time.Second,
	})

	if len(emitted) == 0 {
		t.Fatal("expected JobCompletedEvent to be emitted")
	}
	ev, ok := emitted[len(emitted)-1].(tui.JobCompletedEvent)
	if !ok {
		t.Fatalf("expected JobCompletedEvent, got %T", emitted[len(emitted)-1])
	}
	if ev.IssueNumber != 5 {
		t.Errorf("IssueNumber: got %d, want 5", ev.IssueNumber)
	}
	if ev.StageName != "Plan" {
		t.Errorf("StageName: got %q, want %q", ev.StageName, "Plan")
	}
	if ev.StageModel != "sonnet" {
		t.Errorf("StageModel: got %q, want %q", ev.StageModel, "sonnet")
	}
	if !ev.IsComment {
		t.Error("IsComment: got false, want true")
	}
	if !ev.Success {
		t.Error("Success: got false, want true")
	}
	if ev.Duration != 30*time.Second {
		t.Errorf("Duration: got %v, want 30s", ev.Duration)
	}
}

func TestInvocationObserver_SkipsStatusChanged(t *testing.T) {
	var emitted []tui.Event
	obs := &InvocationObserver{
		Stages: nil,
		Emit:   func(e tui.Event) { emitted = append(emitted, e) },
	}

	store := newTestStore()
	unsub := store.Subscribe(obs)
	defer unsub()

	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 3, NewStatus: "Plan"})

	for _, ev := range emitted {
		if _, ok := ev.(tui.JobCompletedEvent); ok {
			t.Fatal("unexpected JobCompletedEvent on StatusChanged")
		}
	}
}

func TestInvocationObserver_NilEmitIsNoOp(t *testing.T) {
	obs := &InvocationObserver{Stages: nil, Emit: nil}
	store := newTestStore()
	unsub := store.Subscribe(obs)
	defer unsub()
	// Should not panic.
	store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 1, Completed: true})
}

// --- StageChangeObserver ---

func TestStageChangeObserver_FiresStageChangedEventOnStatusChanged(t *testing.T) {
	var emitted []tui.Event
	obs := &StageChangeObserver{
		Emit: func(e tui.Event) { emitted = append(emitted, e) },
	}

	store := newTestStore()
	unsub := store.Subscribe(obs)
	defer unsub()

	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 8, NewStatus: "Implement"})

	if len(emitted) == 0 {
		t.Fatal("expected StageChangedEvent to be emitted")
	}
	ev, ok := emitted[0].(tui.StageChangedEvent)
	if !ok {
		t.Fatalf("expected StageChangedEvent, got %T", emitted[0])
	}
	if ev.Number != 8 {
		t.Errorf("Number: got %d, want 8", ev.Number)
	}
	if ev.NewStage != "Implement" {
		t.Errorf("NewStage: got %q, want %q", ev.NewStage, "Implement")
	}
}

func TestStageChangeObserver_SkipsInvocationChanged(t *testing.T) {
	var emitted []tui.Event
	obs := &StageChangeObserver{
		Emit: func(e tui.Event) { emitted = append(emitted, e) },
	}

	store := newTestStore()
	unsub := store.Subscribe(obs)
	defer unsub()

	store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 2, Completed: true})

	for _, ev := range emitted {
		if _, ok := ev.(tui.StageChangedEvent); ok {
			t.Fatal("unexpected StageChangedEvent on InvocationChanged")
		}
	}
}

func TestStageChangeObserver_NilEmitIsNoOp(t *testing.T) {
	obs := &StageChangeObserver{Emit: nil}
	store := newTestStore()
	unsub := store.Subscribe(obs)
	defer unsub()
	// Should not panic.
	store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 1, NewStatus: "Plan"})
}

// --- Race test ---

func TestObservers_ConcurrentApplyIsRaceFree(t *testing.T) {
	wakeCh := make(chan struct{}, 10)
	var mu sync.Mutex
	set := make(map[string]bool)

	store := newTestStore()
	u1 := store.Subscribe(newWakeChObserver(wakeCh))
	u2 := store.Subscribe(newMayNeedWorkObserver(&mu, &set))
	defer u1()
	defer u2()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			store.Apply(itemstate.LocalStatusUpdated{
				Repo:      fmt.Sprintf("owner/repo%d", n%3),
				Number:    n,
				NewStatus: "Plan",
			})
		}(i)
	}
	wg.Wait()
}

// --- PushUnblockObserver BlockedByChanged path ---

// TestPushUnblockObserver_BlockedByChanged_UnblocksWhenAllBlockersClosed verifies
// the deep-fetch ordering fix: B's BlockedBy is nil at bootstrap; when a deep-fetch
// populates it with a closed blocker A, PushUnblockObserver removes fabrik:blocked.
func TestPushUnblockObserver_BlockedByChanged_UnblocksWhenAllBlockersClosed(t *testing.T) {
	store := newTestStore()

	// Seed A (closed) and B (open + fabrik:blocked, BlockedBy=nil at bootstrap).
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 1, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 1})
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Number: 2,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
		// BlockedBy intentionally nil — simulates bootstrap state.
	}})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Simulate B's first deep-fetch populating BlockedBy with A (now CLOSED).
	store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 2,
		FreshState: gh.ProjectItem{
			Number: 2,
			Repo:   "owner/repo",
			Labels: []string{"fabrik:blocked"},
			BlockedBy: []gh.Dependency{
				{Number: 1, Repo: "owner/repo", State: "CLOSED"},
			},
		},
	})

	n, ok := waitForRemove(t, removeCh)
	if !ok {
		t.Fatal("timeout: Remove was not called after BlockedByChanged with all blockers closed")
	}
	if n != 2 {
		t.Errorf("expected Remove called for issue 2, got %d", n)
	}
}

// TestPushUnblockObserver_BlockedByChanged_NoOpWhenBlockerStillOpen verifies that
// the BlockedByChanged path does NOT remove fabrik:blocked when the listed blocker
// is still open in the store.
func TestPushUnblockObserver_BlockedByChanged_NoOpWhenBlockerStillOpen(t *testing.T) {
	store := newTestStore()

	// Seed A (open) and B (open + fabrik:blocked, BlockedBy=nil at bootstrap).
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 1, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Number: 2,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
	}})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Deep-fetch populates B's BlockedBy with A (still OPEN in store).
	store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 2,
		FreshState: gh.ProjectItem{
			Number: 2,
			Repo:   "owner/repo",
			Labels: []string{"fabrik:blocked"},
			BlockedBy: []gh.Dependency{
				{Number: 1, Repo: "owner/repo", State: "OPEN"},
			},
		},
	})

	select {
	case n := <-removeCh:
		t.Errorf("Remove unexpectedly called for issue %d when blocker still open", n)
	case <-time.After(100 * time.Millisecond):
		// expected: no removal
	}
}

// TestPushUnblockObserver_BlockedByChanged_StorePreemptsDepState verifies that the
// store's view of a blocker takes precedence over dep.State in the deep-fetch payload:
// even if dep.State says "CLOSED", an open blocker in the store prevents unblocking.
func TestPushUnblockObserver_BlockedByChanged_StorePreemptsDepState(t *testing.T) {
	store := newTestStore()

	// Seed A (open in store) and B (open + fabrik:blocked).
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{Number: 1, Repo: "owner/repo"}})
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Number: 2,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
	}})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Deep-fetch says A is CLOSED in dep.State, but the store still has A open.
	// The store view must win: no unblock should fire.
	store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 2,
		FreshState: gh.ProjectItem{
			Number: 2,
			Repo:   "owner/repo",
			Labels: []string{"fabrik:blocked"},
			BlockedBy: []gh.Dependency{
				{Number: 1, Repo: "owner/repo", State: "CLOSED"},
			},
		},
	})

	select {
	case n := <-removeCh:
		t.Errorf("Remove unexpectedly called for issue %d: store open blocker should preempt dep.State=CLOSED", n)
	case <-time.After(100 * time.Millisecond):
		// expected: no removal; store's open view takes precedence
	}
}

// TestPushUnblockObserver_BlockedByChanged_NoOpWhenBlockedByEmpty verifies that
// the BlockedByChanged path is a no-op when the post-mutation BlockedBy is empty.
// The item is seeded with a non-nil BlockedBy so the nil→empty deep-fetch actually
// emits BlockedByChanged; the observer must return without calling Remove.
func TestPushUnblockObserver_BlockedByChanged_NoOpWhenBlockedByEmpty(t *testing.T) {
	store := newTestStore()

	// Seed B with a non-nil BlockedBy so the subsequent deep-fetch triggers BlockedByChanged.
	store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		Number: 2,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{
			{Number: 1, Repo: "owner/repo", State: "OPEN"},
		},
	}})

	removeCh := make(chan int, 1)
	obs := &PushUnblockObserver{
		Store:  store,
		Remove: func(owner, repo string, n int) { removeCh <- n },
	}
	store.Subscribe(obs)

	// Deep-fetch clears BlockedBy to an empty slice — triggers BlockedByChanged (non-nil→empty).
	// The observer must return early because len(BlockedBy)==0.
	store.Apply(itemstate.ItemDeepFetched{
		Repo:   "owner/repo",
		Number: 2,
		FreshState: gh.ProjectItem{
			Number:    2,
			Repo:      "owner/repo",
			Labels:    []string{"fabrik:blocked"},
			BlockedBy: []gh.Dependency{},
		},
	})

	select {
	case n := <-removeCh:
		t.Errorf("Remove unexpectedly called for issue %d when BlockedBy is empty", n)
	case <-time.After(100 * time.Millisecond):
		// expected: no removal
	}
}
