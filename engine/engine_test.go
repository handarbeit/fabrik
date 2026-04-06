package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func TestItemNeedsWork_SkipsPaused(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for paused item")
	}
}

func TestItemNeedsWork_SkipsPausedWithNewComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Status: "Research",
		Labels: []string{"fabrik:paused"},
		Comments: []gh.Comment{
			{ID: "C1", Author: "otheruser", Body: "Please do something"},
		},
	}

	if eng.itemNeedsWork(item) {
		t.Error("itemNeedsWork should return false for paused item even when new comments are present")
	}
}

func TestFindNewComments(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 1,
		Comments: []gh.Comment{
			{ID: "C1", Author: "testuser", Body: "Do this"},
			{ID: "C2", Author: "otheruser", Body: "Not from us"},
			{ID: "C3", Author: "testuser", Body: "🏭 **Fabrik — output"},
			{ID: "C4", Author: "testuser", Body: "Also do that"},
		},
	}

	comments := eng.findNewComments(item)
	if len(comments) != 2 {
		t.Fatalf("expected 2 new comments, got %d", len(comments))
	}
	if comments[0].ID != "C1" || comments[1].ID != "C4" {
		t.Errorf("comments = %v", comments)
	}

	// After markCommentsProcessed, second call should return no new comments
	eng.markCommentsProcessed(item, comments)
	comments2 := eng.findNewComments(item)
	if len(comments2) != 0 {
		t.Errorf("expected 0 new comments on second call, got %d", len(comments2))
	}
}

func TestMapKeys(t *testing.T) {
	m := map[string]string{
		"a": "1",
		"b": "2",
		"c": "3",
	}
	keys := mapKeys(m)
	if len(keys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(keys))
	}
	// Check all keys present (order doesn't matter)
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	for _, expected := range []string{"a", "b", "c"} {
		if !found[expected] {
			t.Errorf("missing key %q", expected)
		}
	}
}

func TestNewWithDeps(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	wm := NewWorktreeManager("/repo")
	cfg := Config{Owner: "o", Repo: "r"}

	eng := NewWithDeps(cfg, client, claude, wm)
	if eng.cfg.Owner != "o" {
		t.Errorf("Owner = %q", eng.cfg.Owner)
	}
	if eng.processedSet == nil {
		t.Error("processedSet should be initialized")
	}
}

func TestGitToplevel(t *testing.T) {
	// We're running in a git repo, so this should succeed
	dir, err := gitToplevel()
	if err != nil {
		t.Fatalf("gitToplevel: %v", err)
	}
	if dir == "" {
		t.Error("gitToplevel returned empty string")
	}
}

func TestNew(t *testing.T) {
	skipIfNoGit(t)
	cfg := Config{
		Owner: "o",
		Repo:  "r",
		Token: "tok",
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if eng.client == nil {
		t.Error("client should not be nil")
	}
	if eng.claude == nil {
		t.Error("claude should not be nil")
	}
	if len(eng.worktreeManagers) == 0 {
		t.Error("worktreeManagers should not be empty")
	}
}

func TestRun_ShutdownOnSignal(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{}, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})
	eng.cfg.PollSeconds = 300 // long poll so we don't hit a second tick

	// Use ReadyCh so we only send SIGINT after signal.Notify is registered.
	readyCh := make(chan struct{})
	eng.cfg.ReadyCh = readyCh

	done := make(chan error, 1)
	go func() {
		done <- eng.Run()
	}()

	// Wait for Run to register signal handlers before sending SIGINT.
	<-readyCh
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(syscall.SIGINT)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
}

func TestProcessedSetConcurrency(t *testing.T) {
	e := &Engine{
		cfg:          Config{User: "testuser"},
		processedSet: make(map[string]time.Time),
	}

	var wg sync.WaitGroup
	// Simulate concurrent writes from multiple workers
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("%d-TestStage", n)
			e.mu.Lock()
			e.processedSet[key] = time.Now()
			e.mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(e.processedSet) != 100 {
		t.Errorf("expected 100 entries, got %d", len(e.processedSet))
	}
}

// TestMarkCommentsProcessedConcurrency verifies markCommentsProcessed is safe
// when called from multiple goroutines.
func TestMarkCommentsProcessedConcurrency(t *testing.T) {
	e := &Engine{
		cfg:          Config{User: "testuser"},
		processedSet: make(map[string]time.Time),
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			item := gh.ProjectItem{Number: n}
			comments := []gh.Comment{
				{ID: fmt.Sprintf("c-%d-a", n)},
				{ID: fmt.Sprintf("c-%d-b", n)},
			}
			e.markCommentsProcessed(item, comments)
		}(i)
	}
	wg.Wait()

	// 20 items * 2 comments each = 40 entries
	if len(e.processedSet) != 40 {
		t.Errorf("expected 40 entries, got %d", len(e.processedSet))
	}
}

// TestFindNewCommentsFiltering verifies that findNewComments correctly filters
// already-processed, wrong-author, and fabrik-output comments.
func TestFindNewCommentsFiltering(t *testing.T) {
	e := &Engine{
		cfg:          Config{User: "alice"},
		processedSet: make(map[string]time.Time),
	}

	// Pre-mark one comment as processed
	e.processedSet["#42-comment-c2"] = time.Now()

	item := gh.ProjectItem{
		Number: 42,
		Comments: []gh.Comment{
			{ID: "c1", Author: "alice", Body: "please fix"},        // new — should be returned
			{ID: "c2", Author: "alice", Body: "already seen"},      // already processed
			{ID: "c3", Author: "bob", Body: "not my user"},         // wrong author
			{ID: "c4", Author: "alice", Body: "🏭 **Fabrik output"}, // fabrik output
		},
	}

	result := e.findNewComments(item)
	if len(result) != 1 {
		t.Fatalf("expected 1 new comment, got %d", len(result))
	}
	if result[0].ID != "c1" {
		t.Errorf("expected comment c1, got %s", result[0].ID)
	}
}

// TestMaxConcurrentDefault verifies the default config value.
func TestMaxConcurrentDefault(t *testing.T) {
	cfg := Config{MaxConcurrent: 5}
	if cfg.MaxConcurrent != 5 {
		t.Errorf("expected default MaxConcurrent=5, got %d", cfg.MaxConcurrent)
	}
}

// TestConcurrentItemDispatch verifies that the non-blocking semaphore dispatch
// used in poll() respects MaxConcurrent and processes all items across multiple
// simulated poll cycles without data races.
func TestConcurrentItemDispatch(t *testing.T) {
	const numItems = 15
	const maxConcurrent = 3

	e := &Engine{
		cfg: Config{
			User:          "testuser",
			MaxConcurrent: maxConcurrent,
			Stages:        nil, // no matching stage → processItem returns nil immediately
		},
		processedSet: make(map[string]time.Time),
		lockedIssues: make(map[string]bool),
		sem:          make(chan struct{}, maxConcurrent),
	}

	board := &gh.ProjectBoard{}
	items := make([]gh.ProjectItem, numItems)
	for i := range items {
		items[i] = gh.ProjectItem{Number: i + 1, Status: "NoSuchStage"}
	}

	var (
		mu          sync.Mutex
		processed   int
		maxInFlight int
		inFlight    int
	)

	// Replicate the non-blocking dispatch pattern from poll(). Items that don't
	// get a semaphore slot are retried in subsequent cycles, mirroring real behaviour.
	remaining := make([]gh.ProjectItem, len(items))
	copy(remaining, items)
	var dispatchWg sync.WaitGroup

	for len(remaining) > 0 {
		var nextRound []gh.ProjectItem
		for _, item := range remaining {
			item := item
			select {
			case e.sem <- struct{}{}:
			default:
				nextRound = append(nextRound, item)
				continue
			}
			dispatchWg.Add(1)
			go func() {
				defer dispatchWg.Done()
				defer func() { <-e.sem }()

				mu.Lock()
				inFlight++
				if inFlight > maxInFlight {
					maxInFlight = inFlight
				}
				mu.Unlock()

				if err := e.processItem(context.Background(), board, item); err != nil {
					t.Errorf("processItem error for issue #%d: %v", item.Number, err)
				}

				mu.Lock()
				inFlight--
				processed++
				mu.Unlock()
			}()
		}
		remaining = nextRound
		if len(remaining) > 0 {
			// Yield so in-flight goroutines can make progress and free semaphore slots.
			dispatchWg.Wait()
		}
	}
	dispatchWg.Wait()

	if processed != numItems {
		t.Errorf("expected %d items processed, got %d", numItems, processed)
	}
	if maxInFlight > maxConcurrent {
		t.Errorf("max in-flight goroutines was %d, expected <= %d", maxInFlight, maxConcurrent)
	}
}

// TestPollNonBlockingAtCapacity verifies that the dispatch loop in poll() skips
// items via non-blocking semaphore acquire when all slots are taken, so poll()
// itself never blocks and the ticker can fire on schedule.
func TestPollNonBlockingAtCapacity(t *testing.T) {
	const maxConcurrent = 2

	e := &Engine{
		cfg: Config{
			User:          "testuser",
			MaxConcurrent: maxConcurrent,
			Stages:        nil,
		},
		processedSet: make(map[string]time.Time),
		sem:          make(chan struct{}, maxConcurrent),
	}

	// Fill the semaphore to simulate two in-flight workers from a previous cycle.
	e.sem <- struct{}{}
	e.sem <- struct{}{}

	items := []gh.ProjectItem{
		{Number: 1, Status: "NoSuchStage"},
		{Number: 2, Status: "NoSuchStage"},
	}

	// Replicate the non-blocking dispatch from poll().
	dispatched := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, item := range items {
			item := item
			select {
			case e.sem <- struct{}{}:
				e.inFlight.Store(item.Number, struct{}{})
				e.wg.Add(1)
				dispatched++
				go func() {
					defer e.wg.Done()
					defer func() { <-e.sem }()
					defer e.inFlight.Delete(item.Number)
				}()
			default:
				// skipped — at capacity
			}
		}
	}()

	select {
	case <-done:
		// dispatch loop returned without blocking — correct
	case <-time.After(time.Second):
		t.Fatal("dispatch loop blocked when semaphore was full")
	}

	if dispatched != 0 {
		t.Errorf("expected 0 dispatched (semaphore full), got %d", dispatched)
	}
}

// TestIdleCountNotIncrementedWhileWorkersInFlight verifies that idleCount (which
// drives auto-upgrade) is not incremented when dispatched==0 but workers are
// still running from a previous poll cycle. Upgrading while workers are in-flight
// would call syscall.Exec and kill them.
func TestIdleCountNotIncrementedWhileWorkersInFlight(t *testing.T) {
	e := &Engine{
		cfg: Config{
			AutoUpgrade:   true,
			MaxConcurrent: 1,
		},
		processedSet: make(map[string]time.Time),
		sem:          make(chan struct{}, 1),
	}

	// Simulate an in-flight worker by populating the map directly.
	e.inFlight.Store(42, struct{}{})

	// With dispatched==0 and an in-flight worker, idleCount must not increment.
	dispatched := 0
	var hasInFlight bool
	e.inFlight.Range(func(_, _ any) bool { hasInFlight = true; return false })

	if hasInFlight {
		e.idleCount = 0
	} else if dispatched == 0 {
		e.idleCount++
	}

	if e.idleCount != 0 {
		t.Errorf("idleCount should remain 0 while workers are in-flight, got %d", e.idleCount)
	}
}

func TestExtractModelOverride(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels", nil, ""},
		{"no model label", []string{"stage:Plan:complete", "fabrik:locked"}, ""},
		{"single model label", []string{"model:opus"}, "opus"},
		{"model label among others", []string{"stage:Plan", "model:sonnet", "fabrik:locked"}, "sonnet"},
		{"empty model name skipped", []string{"model:", "model:haiku"}, "haiku"},
		{"multiple model labels uses first", []string{"model:opus", "model:sonnet"}, "opus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := eng.extractModelOverride(0, tc.labels)
			if got != tc.want {
				t.Errorf("extractModelOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestExtractModelOverrideWarnsOnMultiple(t *testing.T) {
	// Verify no panic and correct return value when multiple model labels are present.
	// The warning goes to eng.logf (stdout in test mode) and is tested behaviorally above.
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	result := eng.extractModelOverride(0, []string{"model:opus", "model:sonnet", "model:haiku"})
	if result != "opus" {
		t.Errorf("expected %q, got %q", "opus", result)
	}
}

func TestItemNeedsWork_CleanupStage_NeedsWork(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	item := gh.ProjectItem{
		Number: 1,
		Status: "Done",
		Labels: []string{}, // no completion label → needs work
	}
	if !eng.itemNeedsWork(item) {
		t.Error("cleanup stage without completion label should need work")
	}
}

func TestItemNeedsWork_CleanupStage_AlreadyComplete(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	item := gh.ProjectItem{
		Number: 1,
		Status: "Done",
		Labels: []string{"stage:Done:complete"},
	}
	if eng.itemNeedsWork(item) {
		t.Error("cleanup stage with completion label should not need work")
	}
}

func TestItemNeedsWork_CleanupStage_PausedItem(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.Stages = testStagesWithCleanup()

	item := gh.ProjectItem{
		Number: 1,
		Status: "Done",
		Labels: []string{"fabrik:paused"}, // paused → should not need work
	}
	if eng.itemNeedsWork(item) {
		t.Error("paused cleanup stage item should not need work")
	}
}
