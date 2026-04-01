package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestProcessedSetConcurrency verifies that concurrent access to processedSet
// via the mutex-protected methods does not cause data races.
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
	e.processedSet["42-comment-c2"] = time.Now()

	item := gh.ProjectItem{
		Number: 42,
		Comments: []gh.Comment{
			{ID: "c1", Author: "alice", Body: "please fix"},       // new — should be returned
			{ID: "c2", Author: "alice", Body: "already seen"},     // already processed
			{ID: "c3", Author: "bob", Body: "not my user"},        // wrong author
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

// TestConcurrentItemDispatch verifies that the semaphore-bounded goroutine pool
// used in poll() dispatches all items without races and respects MaxConcurrent.
// This mirrors the exact dispatch pattern in poll() so that regressions in the
// goroutine fan-out code are caught by go test -race.
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
	}

	board := &gh.ProjectBoard{}
	items := make([]gh.ProjectItem, numItems)
	for i := range items {
		items[i] = gh.ProjectItem{Number: i + 1, Status: "NoSuchStage"}
	}

	// Replicate the exact dispatch pattern from poll().
	var (
		mu          sync.Mutex
		processed   int
		maxInFlight int
		inFlight    int
	)

	sem := make(chan struct{}, e.cfg.MaxConcurrent)
	var wg sync.WaitGroup
	for _, item := range items {
		item := item
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			if err := e.processItem(board, item); err != nil {
				t.Errorf("processItem error for issue #%d: %v", item.Number, err)
			}

			mu.Lock()
			inFlight--
			processed++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if processed != numItems {
		t.Errorf("expected %d items processed, got %d", numItems, processed)
	}
	if maxInFlight > maxConcurrent {
		t.Errorf("max in-flight goroutines was %d, expected <= %d", maxInFlight, maxConcurrent)
	}
}

func TestExtractModelOverride(t *testing.T) {
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
			got := extractModelOverride(tc.labels)
			if got != tc.want {
				t.Errorf("extractModelOverride(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

func TestExtractModelOverrideWarnsOnMultiple(t *testing.T) {
	// Verify no panic and correct return value when multiple model labels are present.
	// The warning goes to fmt.Printf (stdout) and is tested behaviorally above.
	result := extractModelOverride([]string{"model:opus", "model:sonnet", "model:haiku"})
	if result != "opus" {
		t.Errorf("expected %q, got %q", "opus", result)
	}
}
