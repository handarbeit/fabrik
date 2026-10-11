package sim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// TestSlotSaturation_PollNeverBlocksAndDeferredItemDispatchesWhenSlotFrees is
// the #2114 scenario. With MaxConcurrent=2 and both slots held by Specify
// workers that block inside the (scripted) Claude invocation, a third
// dispatchable issue must be deferred without stalling the poll:
//
//   - PollOnce returns while both workers are still held (the blocking acquire
//     never returned), and the post-dispatch settle scans ran in that same poll
//     (observed through fabrik:clear-claude-limit being consumed on a paused,
//     never-dispatched bystander);
//   - the deferred issue carries no in_progress label and no stage call;
//   - once one worker exits, a later poll dispatches the deferred issue without
//     any unrelated change to it.
//
// PollOnce runs in a goroutine with a deadline so a regression to the blocking
// acquire fails this test instead of hanging the suite. Not RunPoll: RunPoll
// waits for worker quiescence, which cannot happen while workers are held.
func TestSlotSaturation_PollNeverBlocksAndDeferredItemDispatchesWhenSlotFrees(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages(), MaxConcurrent: 2})

	var (
		mu      sync.Mutex
		gates   = map[int]chan struct{}{}
		entered = make(chan int, 4)
	)
	gateFor := func(n int) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		return gates[n]
	}
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if g := gateFor(issue.Number); g != nil {
			entered <- issue.Number
			select {
			case <-g:
			case <-ctx.Done():
			}
		}
		return simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
	})

	// pollWithin drives one poll the way RunPoll does, but bounded.
	pollWithin := func(d time.Duration) {
		t.Helper()
		if env.PollInterval > 0 {
			env.Clock.Advance(env.PollInterval)
		}
		done := make(chan error, 1)
		go func() { done <- env.Engine.PollOnce(context.Background()) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
		case <-time.After(d):
			t.Fatalf("poll did not return within %s — blocked on a worker slot?\n\n%s", d, diagnostics(env))
		}
	}

	a := FileIssue(t, env, "holder A", "body", "Specify")
	b := FileIssue(t, env, "holder B", "body", "Specify")
	mu.Lock()
	gates[a] = make(chan struct{})
	gates[b] = make(chan struct{})
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, g := range gates {
			select {
			case <-g:
			default:
				close(g)
			}
		}
	})

	pollWithin(60 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(60 * time.Second):
			t.Fatalf("holder workers did not both reach the scripted invocation\n\n%s", diagnostics(env))
		}
	}

	// A bystander that never dispatches (paused) but carries the one-shot
	// fabrik:clear-claude-limit command, consumed by a settle scan that runs
	// AFTER dispatchCandidates in poll().
	bystander := FileIssue(t, env, "bystander", "body", "Specify", "fabrik:paused", "fabrik:awaiting-input", "fabrik:clear-claude-limit")
	c := FileIssue(t, env, "deferred C", "body", "Specify")
	callsBefore := env.Claude.StageCallCount("Specify")

	pollWithin(30 * time.Second) // both slots held, C dispatchable

	if hasLabel(IssueLabels(t, env, bystander), "fabrik:clear-claude-limit") {
		t.Errorf("post-dispatch settle scan did not run in the saturated poll: bystander still carries fabrik:clear-claude-limit")
	}
	labels := IssueLabels(t, env, c)
	for _, l := range labels {
		if l == "stage:Specify:in_progress" || l == "stage:Specify:complete" || l == "stage:Specify:failed" {
			t.Errorf("deferred issue #%d must carry no stage label, has %v", c, labels)
		}
	}
	if got := env.Claude.StageCallCount("Specify"); got != callsBefore {
		t.Errorf("StageCallCount(Specify) = %d, want %d — the deferred issue must not be invoked", got, callsBefore)
	}

	// Free one slot; a later poll must dispatch the deferred issue.
	close(gateFor(a))
	deadline := time.Now().Add(120 * time.Second)
	for !hasLabel(IssueLabels(t, env, c), "stage:Specify:complete") {
		if time.Now().After(deadline) {
			t.Fatalf("deferred issue #%d was never dispatched after a slot freed\n\n%s", c, diagnostics(env))
		}
		time.Sleep(50 * time.Millisecond)
		pollWithin(60 * time.Second)
	}

	close(gateFor(b))
	waitForWorkerQuiescence(t, env)
}
