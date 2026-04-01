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
