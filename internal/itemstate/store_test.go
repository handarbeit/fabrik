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

// ---- Phase 3-E: new mutation handler tests ----

// TestCIMergePendingStartedAndCleared verifies CIMergePendingStarted sets
// CIMergePendingSince and CIMergePendingCleared zeroes it.
func TestCIMergePendingStartedAndCleared(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 1)})

	pendingAt := time.Now().Add(-5 * time.Minute)
	_, _, err := s.Apply(CIMergePendingStarted{Repo: testRepo, Number: 1, At: pendingAt})
	if err != nil {
		t.Fatalf("CIMergePendingStarted: %v", err)
	}

	snap, _ := s.Get(testRepo, 1)
	lpr := snap.LinkedPR()
	if lpr == nil {
		t.Fatal("expected LinkedPR non-nil after CIMergePendingStarted")
	}
	if !lpr.CIMergePendingSince.Equal(pendingAt) {
		t.Errorf("CIMergePendingSince = %v, want %v", lpr.CIMergePendingSince, pendingAt)
	}

	// Snapshot immutability: a held snapshot must not see the clear.
	snapHeld := snap

	_, _, err = s.Apply(CIMergePendingCleared{Repo: testRepo, Number: 1})
	if err != nil {
		t.Fatalf("CIMergePendingCleared: %v", err)
	}
	snap2, _ := s.Get(testRepo, 1)
	if lpr2 := snap2.LinkedPR(); lpr2 != nil && !lpr2.CIMergePendingSince.IsZero() {
		t.Error("expected CIMergePendingSince zeroed after CIMergePendingCleared")
	}

	// Original snapshot must be unchanged.
	if lprHeld := snapHeld.LinkedPR(); lprHeld == nil || lprHeld.CIMergePendingSince.IsZero() {
		t.Error("held snapshot should still show the original CIMergePendingSince")
	}
}

// TestPRChecksObservedMonotonic verifies HasHadChecks transitions false→true and stays true.
func TestPRChecksObservedMonotonic(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 2)})

	// Initially no linked PR: snap.LinkedPR() is nil.
	snap0, _ := s.Get(testRepo, 2)
	if lpr := snap0.LinkedPR(); lpr != nil && lpr.HasHadChecks {
		t.Fatal("expected HasHadChecks=false initially")
	}

	_, _, err := s.Apply(PRChecksObserved{Repo: testRepo, Number: 2})
	if err != nil {
		t.Fatalf("PRChecksObserved: %v", err)
	}

	snap1, _ := s.Get(testRepo, 2)
	lpr1 := snap1.LinkedPR()
	if lpr1 == nil || !lpr1.HasHadChecks {
		t.Fatal("expected HasHadChecks=true after PRChecksObserved")
	}

	// Second application: no-op (monotonic).
	_, changes, _ := s.Apply(PRChecksObserved{Repo: testRepo, Number: 2})
	if len(changes) != 0 {
		t.Error("second PRChecksObserved should be a no-op (no change)")
	}

	snap2, _ := s.Get(testRepo, 2)
	if lpr2 := snap2.LinkedPR(); lpr2 == nil || !lpr2.HasHadChecks {
		t.Error("HasHadChecks should still be true after redundant PRChecksObserved")
	}
}

// TestItemDeepFetchedClearsLastDeepFetchFailureAt verifies that applying ItemDeepFetched
// zeroes the LastDeepFetchFailureAt field (set by a prior DeepFetchFailed).
func TestItemDeepFetchedClearsLastDeepFetchFailureAt(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 3)})

	failedAt := time.Now().Add(-time.Minute)
	s.Apply(DeepFetchFailed{Repo: testRepo, Number: 3, At: failedAt})

	snap1, _ := s.Get(testRepo, 3)
	if snap1.State().LastDeepFetchFailureAt.IsZero() {
		t.Fatal("expected LastDeepFetchFailureAt set after DeepFetchFailed")
	}

	freshItem := testProjectItem(testRepo, 3)
	s.Apply(ItemDeepFetched{Repo: testRepo, Number: 3, FreshState: freshItem})

	snap2, _ := s.Get(testRepo, 3)
	if !snap2.State().LastDeepFetchFailureAt.IsZero() {
		t.Error("expected LastDeepFetchFailureAt zeroed after ItemDeepFetched")
	}
}

// ---- Reset observer tests ----

// TestResetNotifiesObserversOnAdd verifies that Reset emits a Change for every
// item in the new slice, with non-zero Fields.
func TestResetNotifiesObserversOnAdd(t *testing.T) {
	s := NewStore(nil)

	var mu sync.Mutex
	var received []Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
		mu.Lock()
		received = append(received, c)
		mu.Unlock()
	}))

	items := []gh.ProjectItem{
		testProjectItem(testRepo, 10),
		testProjectItem(testRepo, 11),
		testProjectItem(testRepo, 12),
	}
	s.Reset(items)

	mu.Lock()
	got := len(received)
	mu.Unlock()

	if got != 3 {
		t.Fatalf("expected 3 Changes from Reset, got %d", got)
	}
	for _, c := range received {
		if c.Fields == 0 {
			t.Errorf("Change for #%d has zero Fields; want non-zero", c.Number)
		}
		if c.Fields&ItemRemoved != 0 {
			t.Errorf("Change for #%d has ItemRemoved set unexpectedly", c.Number)
		}
	}
}

// TestResetNotifiesObserversOnRemoval verifies that Reset emits ItemRemoved Changes
// for items present in the old map but absent from the new slice.
func TestResetNotifiesObserversOnRemoval(t *testing.T) {
	s := NewStore(nil)

	// Populate three items via Apply.
	for _, n := range []int{20, 21, 22} {
		s.Apply(IssueOpened{Item: testProjectItem(testRepo, n)})
	}

	var mu sync.Mutex
	var received []Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
		mu.Lock()
		received = append(received, c)
		mu.Unlock()
	}))

	// Reset with only 2 of the 3 items — #22 is removed.
	s.Reset([]gh.ProjectItem{
		testProjectItem(testRepo, 20),
		testProjectItem(testRepo, 21),
	})

	mu.Lock()
	got := make([]Change, len(received))
	copy(got, received)
	mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("expected 3 Changes (2 retained + 1 removed), got %d", len(got))
	}

	var removals, retained int
	for _, c := range got {
		if c.Fields&ItemRemoved != 0 {
			removals++
			if c.Number != 22 {
				t.Errorf("ItemRemoved Change for unexpected item #%d", c.Number)
			}
		} else {
			retained++
		}
	}
	if removals != 1 {
		t.Errorf("expected 1 ItemRemoved Change, got %d", removals)
	}
	if retained != 2 {
		t.Errorf("expected 2 non-removal Changes, got %d", retained)
	}
}

// TestResetObserverCalledOutsideLock verifies that observers can call Store.Get
// inside the Reset callback without deadlocking.
func TestResetObserverCalledOutsideLock(t *testing.T) {
	s := NewStore(nil)

	done := make(chan struct{}, 1)
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) {
		// Would deadlock if called under the write lock.
		s.Get(c.Repo, c.Number)
		select {
		case done <- struct{}{}:
		default:
		}
	}))

	s.Reset([]gh.ProjectItem{testProjectItem(testRepo, 30)})

	select {
	case <-done:
		// OK — observer completed without deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: observer could not call Store.Get during Reset dispatch")
	}
}

// TestConcurrentReset verifies no data race when Reset, Apply, Subscribe, and
// Unsubscribe run concurrently.
func TestConcurrentReset(t *testing.T) {
	s := NewStore(nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent Resets.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.Reset([]gh.ProjectItem{testProjectItem(testRepo, n+100)})
			}
		}(i)
	}

	// Concurrent Applies.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.Apply(IssueLabeled{Repo: testRepo, Number: n + 100, Label: "x"})
			}
		}(i)
	}

	// Concurrent Subscribe/Unsubscribe.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cancel := s.Subscribe(ObserverFunc(func(Change, Snapshot) {}))
				cancel()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestLinkedPRSnapshotImmutabilityAfterCIMerge verifies that a snapshot taken
// before CIMergePendingStarted is not retroactively mutated.
func TestLinkedPRSnapshotImmutabilityAfterCIMerge(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 4)})

	snapBefore, _ := s.Get(testRepo, 4)

	s.Apply(CIMergePendingStarted{Repo: testRepo, Number: 4, At: time.Now()})

	// snapBefore must not see the mutation.
	if lpr := snapBefore.LinkedPR(); lpr != nil && !lpr.CIMergePendingSince.IsZero() {
		t.Error("snapshot taken before CIMergePendingStarted should not reflect the mutation")
	}
}

// ---- PRDetailsUpdated + prToKey index tests ----

// TestPRDetailsUpdatedMutatesAndFiresObserver verifies that PRDetailsUpdated sets the
// four new LinkedPRState fields (Title, State, Merged, Draft) and fires LinkedPRChanged.
func TestPRDetailsUpdatedMutatesAndFiresObserver(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 10)})

	var lastChange Change
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) { lastChange = c }))

	_, changes, err := s.Apply(PRDetailsUpdated{
		Repo:     testRepo,
		Number:   10,
		PRNumber: 42,
		Title:    "feat: add caching",
		State:    "open",
		Merged:   false,
		Draft:    true,
	})
	if err != nil {
		t.Fatalf("PRDetailsUpdated: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected at least one change from PRDetailsUpdated")
	}
	if lastChange.Fields&LinkedPRChanged == 0 {
		t.Errorf("Fields %b does not include LinkedPRChanged", lastChange.Fields)
	}

	snap, _ := s.Get(testRepo, 10)
	lpr := snap.LinkedPR()
	if lpr == nil {
		t.Fatal("LinkedPR is nil after PRDetailsUpdated")
	}
	if lpr.Number != 42 {
		t.Errorf("Number = %d; want 42", lpr.Number)
	}
	if lpr.Title != "feat: add caching" {
		t.Errorf("Title = %q; want %q", lpr.Title, "feat: add caching")
	}
	if lpr.State != "open" {
		t.Errorf("State = %q; want open", lpr.State)
	}
	if lpr.Merged {
		t.Error("Merged should be false")
	}
	if !lpr.Draft {
		t.Error("Draft should be true")
	}
}

// TestPRDetailsUpdatedIsNoOpWhenFieldsUnchanged verifies that a duplicate PRDetailsUpdated
// does not fire observers (invariant I6).
func TestPRDetailsUpdatedIsNoOpWhenFieldsUnchanged(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 11)})
	s.Apply(PRDetailsUpdated{Repo: testRepo, Number: 11, PRNumber: 7, Title: "T", State: "open"})

	var count int
	s.Subscribe(ObserverFunc(func(c Change, _ Snapshot) { count++ }))

	_, changes, _ := s.Apply(PRDetailsUpdated{Repo: testRepo, Number: 11, PRNumber: 7, Title: "T", State: "open"})
	if len(changes) != 0 {
		t.Error("second identical PRDetailsUpdated should be a no-op")
	}
	if count != 0 {
		t.Errorf("observer fired %d times; want 0 for no-op", count)
	}
}

// TestGetByPRKeyReturnsMappedItemKey verifies that the prToKey index is populated
// by PRDetailsUpdated and that GetByPRKey returns the correct item key.
func TestGetByPRKeyReturnsMappedItemKey(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 12)})
	s.Apply(PRDetailsUpdated{Repo: testRepo, Number: 12, PRNumber: 55, Title: "T", State: "open"})

	key, ok := s.GetByPRKey(testRepo, 55)
	if !ok {
		t.Fatal("GetByPRKey returned false; expected true after PRDetailsUpdated")
	}
	want := itemKeyFor(testRepo, 12)
	if key != want {
		t.Errorf("key = %q; want %q", key, want)
	}

	// Unknown PR returns false.
	if _, found := s.GetByPRKey(testRepo, 9999); found {
		t.Error("GetByPRKey for unknown PR should return false")
	}
}

// TestGetByPRKeyFalseAfterRemove verifies that Store.Remove clears the prToKey entry.
func TestGetByPRKeyFalseAfterRemove(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 13)})
	s.Apply(PRDetailsUpdated{Repo: testRepo, Number: 13, PRNumber: 99, Title: "T", State: "open"})

	if _, ok := s.GetByPRKey(testRepo, 99); !ok {
		t.Fatal("GetByPRKey returned false before Remove")
	}

	s.Remove(testRepo, 13)

	if _, ok := s.GetByPRKey(testRepo, 99); ok {
		t.Error("GetByPRKey should return false after Store.Remove")
	}
}

// TestPRToKeyPopulatedByPRHeadSHAUpdated verifies that the prToKey index is also
// maintained when LinkedPR.Number is set via PRHeadSHAUpdated.
func TestPRToKeyPopulatedByPRHeadSHAUpdated(t *testing.T) {
	s := NewStore(nil)
	s.Apply(IssueOpened{Item: testProjectItem(testRepo, 14)})
	s.Apply(PRHeadSHAUpdated{Repo: testRepo, Number: 14, LinkedPRNum: 77, SHA: "abc123"})

	key, ok := s.GetByPRKey(testRepo, 77)
	if !ok {
		t.Fatal("GetByPRKey returned false after PRHeadSHAUpdated with LinkedPRNum")
	}
	if want := itemKeyFor(testRepo, 14); key != want {
		t.Errorf("key = %q; want %q", key, want)
	}
}
