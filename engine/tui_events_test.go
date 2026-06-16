package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// collectEvents drains ch until no new event arrives within idle.
func collectEvents(ch chan tui.Event, idle time.Duration) []tui.Event {
	var events []tui.Event
	deadline := time.NewTimer(idle)
	defer deadline.Stop()
	for {
		select {
		case ev := <-ch:
			events = append(events, ev)
			if !deadline.Stop() {
				select {
				case <-deadline.C:
				default:
				}
			}
			deadline.Reset(idle)
		case <-deadline.C:
			return events
		}
	}
}

// TestJobStartedEvent_EarlyReturn verifies that processItem does NOT emit
// JobStartedEvent when it early-returns before the lock-acquired boundary.
// An item with stage:Research:complete causes an early return before lock
// acquisition; the events channel must stay empty.
func TestJobStartedEvent_EarlyReturn(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(t, client, claude)

	ch := make(chan tui.Event, 64)
	eng.events = ch

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test Issue",
		Status: "Research",
		Labels: []string{"stage:Research:complete"},
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	events := collectEvents(ch, 20*time.Millisecond)
	for _, ev := range events {
		if _, ok := ev.(tui.JobStartedEvent); ok {
			t.Errorf("got unexpected JobStartedEvent on early-return path (events: %v)", events)
		}
	}
}

// TestJobStartedEvent_SuccessPath verifies that processItem emits exactly one
// JobStartedEvent and at least one JobCompletedEvent{Skipped:false} when Claude
// runs successfully (FABRIK_STAGE_COMPLETE returned).
func TestJobStartedEvent_SuccessPath(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	wm := NewWorktreeManager(repoDir)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			return "FABRIK_STAGE_COMPLETE\n", true, TokenUsage{}, nil
		},
	}
	eng := newTestEngineWithWM(client, claude, wm)

	ch := make(chan tui.Event, 64)
	eng.events = ch

	// Wire the InvocationObserver so JobCompletedEvent{Skipped:false} fires.
	invObs := &InvocationObserver{Stages: eng.cfg.Stages, Emit: eng.emitStructural}
	unsub := eng.store.Subscribe(invObs)
	defer unsub()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 1,
		Title:  "Test Issue",
		Status: "Research",
		ItemID: "PVTI_1",
	}

	if err := eng.processItem(context.Background(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}

	events := collectEvents(ch, 50*time.Millisecond)

	var startedCount int
	var completedSkippedFalseCount int
	for _, ev := range events {
		switch e := ev.(type) {
		case tui.JobStartedEvent:
			startedCount++
		case tui.JobCompletedEvent:
			if !e.Skipped {
				completedSkippedFalseCount++
			}
		}
	}

	if startedCount != 1 {
		t.Errorf("JobStartedEvent count = %d, want 1 (events: %v)", startedCount, events)
	}
	if completedSkippedFalseCount < 1 {
		t.Errorf("JobCompletedEvent{Skipped:false} count = %d, want ≥1 (events: %v)", completedSkippedFalseCount, events)
	}
}

// TestJobCompletedEvent_CancelledPath verifies that processItem emits
// JobCompletedEvent{Skipped:true} when context is cancelled after JobStartedEvent
// fires. The deferred emit at the emission site must cover this path; no
// JobCompletedEvent{Skipped:false} should appear.
func TestJobCompletedEvent_CancelledPath(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	wm := NewWorktreeManager(repoDir)
	client := &mockGitHubClient{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inv := &blockingClaudeInvoker{ready: make(chan struct{})}
	eng := newTestEngineWithWM(client, inv, wm)

	ch := make(chan tui.Event, 64)
	eng.events = ch

	// Wire InvocationObserver to detect any (unwanted) Skipped:false events.
	invObs := &InvocationObserver{Stages: eng.cfg.Stages, Emit: eng.emitStructural}
	unsub := eng.store.Subscribe(invObs)
	defer unsub()

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 2,
		Title:  "Cancelled Issue",
		Status: "Research",
		ItemID: "PVTI_2",
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = eng.processItem(ctx, board, item)
	}()

	// Wait until JobStartedEvent arrives, then cancel the context.
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if _, ok := ev.(tui.JobStartedEvent); ok {
				cancel()
				goto waitDone
			}
		case <-timeout:
			t.Fatal("timed out waiting for JobStartedEvent")
		}
	}
waitDone:
	wg.Wait()

	// Collect remaining events (deferred emit fires synchronously on return).
	remaining := collectEvents(ch, 50*time.Millisecond)

	var skippedTrue, skippedFalse int
	for _, ev := range remaining {
		if e, ok := ev.(tui.JobCompletedEvent); ok {
			if e.Skipped {
				skippedTrue++
			} else {
				skippedFalse++
			}
		}
	}

	if skippedTrue < 1 {
		t.Errorf("JobCompletedEvent{Skipped:true} count = %d, want ≥1", skippedTrue)
	}
	if skippedFalse > 0 {
		t.Errorf("JobCompletedEvent{Skipped:false} count = %d, want 0 (no InvocationRecorded on cancel path)", skippedFalse)
	}
}

// newTestEngineWithWM creates a test engine using the provided WorktreeManager.
func newTestEngineWithWM(client *mockGitHubClient, claude ClaudeInvoker, wm *WorktreeManager) *Engine {
	return NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		claude,
		wm,
	)
}

// blockingClaudeInvoker blocks in Invoke until the context is cancelled.
// The ready channel is closed once when Invoke is first entered.
type blockingClaudeInvoker struct {
	once  sync.Once
	ready chan struct{}
}

func (b *blockingClaudeInvoker) Invoke(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	b.once.Do(func() { close(b.ready) })
	<-ctx.Done()
	return "", false, TokenUsage{}, ctx.Err()
}

func (b *blockingClaudeInvoker) InvokeForComments(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
	return "", false, TokenUsage{}, nil
}
