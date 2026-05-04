package itemstate

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ---- stub FallbackFetcher ----

type stubFallback struct {
	mu    sync.Mutex
	calls int
	item  gh.ProjectItem
	err   error
}

func (f *stubFallback) FetchItem(repo string, number int) (gh.ProjectItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	item := f.item
	item.Repo = repo
	item.Number = number
	return item, f.err
}

func (f *stubFallback) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---- I4: Observers see every Change exactly once ----

// TestObserverSeesEveryChange applies N mutations and verifies the observer receives
// exactly N Changes (invariant I4).
func TestObserverSeesEveryChange(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	var received []Change
	var mu sync.Mutex
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) {
		mu.Lock()
		received = append(received, c)
		mu.Unlock()
	}))

	statuses := []string{"Research", "Plan", "Implement", "Review", "Validate", "Done"}
	for _, st := range statuses {
		if _, _, err := s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: st}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	mu.Lock()
	n := len(received)
	mu.Unlock()

	if n != len(statuses) {
		t.Errorf("observer received %d changes; want %d", n, len(statuses))
	}
}

// TestObserverReceivesCorrectFields verifies that the Change's Fields bitmask
// matches what the mutation type should produce.
func TestObserverReceivesCorrectFields(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	var lastChange Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) { lastChange = c }))

	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "urgent"})
	if lastChange.Fields&LabelsChanged == 0 {
		t.Errorf("Change.Fields %b does not include LabelsChanged", lastChange.Fields)
	}
	if lastChange.Repo != testRepo || lastChange.Number != 1 {
		t.Errorf("Change.Repo/Number = %q/%d; want %q/1", lastChange.Repo, lastChange.Number, testRepo)
	}
}

// TestMultipleObserversAllNotified verifies that all registered observers are
// called for each Change.
func TestMultipleObserversAllNotified(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	counts := make([]int64, 3)
	for i := range counts {
		idx := i
		s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
			atomic.AddInt64(&counts[idx], 1)
		}))
	}

	for i := 0; i < 5; i++ {
		s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: itoa(i) + "-label"})
	}

	for i, c := range counts {
		if c != 5 {
			t.Errorf("observer %d received %d changes; want 5", i, c)
		}
	}
}

// ---- I9: Cache-miss reads fall back to GitHub ----

// TestCacheMissFallbackCalledOnce verifies that a missing item triggers exactly
// one FallbackFetcher call and caches the result (invariant I9).
func TestCacheMissFallbackCalledOnce(t *testing.T) {
	fb := &stubFallback{item: gh.ProjectItem{
		Title:  "Fetched Issue",
		Status: "Implement",
		Labels: []string{"fresh"},
	}}
	s := NewStore(fb)

	snap, err := s.Get(testRepo, 42)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fb.callCount() != 1 {
		t.Errorf("fallback called %d times; want 1", fb.callCount())
	}
	if snap.State().Title != "Fetched Issue" {
		t.Errorf("Title = %q; want Fetched Issue", snap.State().Title)
	}

	// Second Get should hit the cache — no additional fallback call.
	if _, err := s.Get(testRepo, 42); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if fb.callCount() != 1 {
		t.Errorf("fallback called %d times after cached Get; want 1", fb.callCount())
	}
}

// TestCacheMissReturnsErrNotFoundWhenNoFallback verifies that a missing item
// with no FallbackFetcher returns ErrNotFound.
func TestCacheMissReturnsErrNotFoundWhenNoFallback(t *testing.T) {
	s := NewStore(nil)
	_, err := s.Get(testRepo, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v; want ErrNotFound", err)
	}
}

// TestCacheMissFallbackErrorReturnsErrNotFound verifies that a fallback error
// surfaces as ErrNotFound.
func TestCacheMissFallbackErrorReturnsErrNotFound(t *testing.T) {
	fb := &stubFallback{err: errors.New("not found on GitHub")}
	s := NewStore(fb)
	_, err := s.Get(testRepo, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v; want ErrNotFound", err)
	}
}

// TestFallbackPopulatesCache verifies that after a successful fallback, the item
// is observable via the change feed.
func TestFallbackPopulatesCache(t *testing.T) {
	fb := &stubFallback{item: gh.ProjectItem{Title: "Fetched"}}
	s := NewStore(fb)

	var received []Change
	var mu sync.Mutex
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
		mu.Lock()
		received = append(received, c)
		mu.Unlock()
	}))

	s.Get(testRepo, 5)

	mu.Lock()
	n := len(received)
	mu.Unlock()
	if n == 0 {
		t.Error("fallback did not trigger an observer Change")
	}
}

// ---- Subscribe / Unsubscribe ----

// TestUnsubscribeStopsNotifications verifies that after calling the unsubscribe
// function, the observer no longer receives changes.
func TestUnsubscribeStopsNotifications(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	var count int64
	unsub := s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
		atomic.AddInt64(&count, 1)
	}))

	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "before"})
	unsub()
	s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: "after"})

	if atomic.LoadInt64(&count) != 1 {
		t.Errorf("observer fired %d times; want exactly 1 (before unsubscribe)", atomic.LoadInt64(&count))
	}
}

// TestUnsubscribeIdempotent verifies that calling the unsubscribe function
// multiple times does not panic.
func TestUnsubscribeIdempotent(t *testing.T) {
	s := NewStore(nil)
	unsub := s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {}))
	unsub()
	unsub() // should not panic
}

// ---- Concurrency tests ----

// TestConcurrentApplyGet launches 100 goroutines each doing Apply/Get and
// verifies the race detector reports no data races.
func TestConcurrentApplyGet(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	var wg sync.WaitGroup
	const goroutines = 100

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Alternate between Apply and Get.
			if n%2 == 0 {
				s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: itoa(n)})
			} else {
				s.Get(testRepo, 1)
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrentSubscribeUnsubscribeApply verifies that concurrent
// Subscribe/Unsubscribe/Apply calls do not race or panic.
func TestConcurrentSubscribeUnsubscribeApply(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	var wg sync.WaitGroup
	const goroutines = 50

	for i := 0; i < goroutines; i++ {
		wg.Add(3)
		go func(n int) {
			defer wg.Done()
			unsub := s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {}))
			time.Sleep(time.Microsecond)
			unsub()
		}(i)
		go func(n int) {
			defer wg.Done()
			s.Apply(IssueLabeled{Repo: testRepo, Number: 1, Label: itoa(n)})
		}(i)
		go func(n int) {
			defer wg.Done()
			s.Get(testRepo, 1)
		}(i)
	}
	wg.Wait()
}

// TestConcurrentBoardReconcile verifies that concurrent BoardReconciled
// mutations do not race.
func TestConcurrentBoardReconcile(t *testing.T) {
	s := NewStore(nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			items := []gh.ProjectItem{
				testProjectItem(testRepo, n+1000),
			}
			s.Apply(BoardReconciled{Items: items})
		}(i)
	}
	wg.Wait()
}

// TestObserverSnapshotIsConsistentWithChange verifies that the Snapshot passed
// to an observer reflects the state at the time the Change was generated.
func TestObserverSnapshotIsConsistentWithChange(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	var observedStatus string
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) {
		if c.Fields&StatusChanged != 0 {
			observedStatus = snap.Status()
		}
	}))

	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Validate"})

	if observedStatus != "Validate" {
		t.Errorf("observer saw status %q; want Validate", observedStatus)
	}
}

// TestObserverNotCalledUnderLock verifies observers can safely call Store.Get
// without deadlocking (proves observers are called outside the write lock).
func TestObserverNotCalledUnderLock(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	done := make(chan struct{}, 1)
	s.Subscribe(ObserverFunc(func(c Change, snap Snapshot) {
		// This would deadlock if called under the write lock.
		s.Get(testRepo, 1)
		done <- struct{}{}
	}))

	s.Apply(LocalStatusUpdated{Repo: testRepo, Number: 1, NewStatus: "Review"})

	select {
	case <-done:
		// OK — observer completed without deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: observer could not call Store.Get while change was being dispatched")
	}
}

// TestItemIDIndexMaintained verifies that ProjectV2ItemEdited can resolve an item
// via the itemIDToKey index after IssueOpened sets ItemID.
func TestItemIDIndexMaintained(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.ItemID = "PVTI_test"
	s.Apply(IssueOpened{Item: pi})

	snap, changes, err := s.Apply(ProjectV2ItemEdited{ItemID: "PVTI_test", NewStatus: "Done"})
	if err != nil || len(changes) == 0 {
		t.Fatalf("ProjectV2ItemEdited: err=%v changes=%d", err, len(changes))
	}
	_ = snap
}

// TestSHAIndexMaintained verifies that CheckRunCompleted can resolve an item
// via the shaToKey index.
func TestSHAIndexMaintained(t *testing.T) {
	s := NewStore(nil)
	pi := testProjectItem(testRepo, 1)
	pi.LinkedPRNumber = 5
	s.Apply(IssueOpened{Item: pi})

	// Inject SHA via internal state (simulating a LinkedPR update).
	s.mu.Lock()
	key := itemKeyFor(testRepo, 1)
	item := s.items[key]
	if item.LinkedPR == nil {
		item.LinkedPR = &LinkedPRState{Number: 5}
	}
	item.LinkedPR.HeadSHA = "sha999"
	s.shaToKey["sha999"] = key
	s.mu.Unlock()

	_, changes, err := s.Apply(CheckRunCompleted{Repo: testRepo, SHA: "sha999", Run: gh.CheckRun{ID: 1}})
	if err != nil || len(changes) == 0 {
		t.Fatalf("CheckRunCompleted: err=%v changes=%d", err, len(changes))
	}
}
