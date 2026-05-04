package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
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
