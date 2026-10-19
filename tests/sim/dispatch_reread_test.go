package sim

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Sim twin for #2350 R2 / audit §3.1. dispatchCandidates used to decide on the
// copy of an item captured before Phase 1/2 and the settle scans of the same
// poll. A Phase 1 cycle-limit pause therefore did not stop a pending comment pass
// being dispatched on the same item in that very poll. The dispatch decision now
// re-reads the item from the Store, so the pause is respected.

// runPausedMidPollScenario drives a review gate to its cycle limit, then seeds
// the over-limit review together with a human comment (a comment pass would be
// admitted for it) and runs exactly one poll. It returns the number of comment
// passes Claude was asked for, and the worker lifecycle changes the Store saw for
// the item in that poll.
func runPausedMidPollScenario(t *testing.T, rereadDisabled bool) (commentPasses int, paused bool, workerChanges int) {
	t.Helper()
	const maxCycles = 1
	env := newReviewAuthorityEnv(t, 15*time.Minute, maxCycles)
	env.Engine.SetDispatchRereadDisabledForTest(rereadDisabled)

	num, prNum := seedReviewGateItem(t, env, "Review", "reread-paused", "review-authority:authoritative")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)

	before := env.Claude.CommentCallCount("Review")
	seedActionableReview(t, env, prNum, "reviewer-human", "CHANGES_REQUESTED", "round 1: not right")
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Review") > before }, 80)
	// RunPoll waits for the reinvoke's worker, so the next poll is the one in which
	// Phase 1 finds the review cycle limit reached and pauses the item. A human
	// comment arrives first, so the same poll also admits a comment pass for it.
	passesBefore := env.Claude.CommentCallCount("Review")
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "one more thing, please")

	// The observer runs on the engine's goroutine, the read below on the test's.
	var changes atomic.Int32
	env.Engine.SubscribeStoreForTest(itemstate.ObserverFunc(func(c itemstate.Change, _ itemstate.Snapshot) {
		if c.Number == num && c.Fields&itemstate.WorkerLifecycleChanged != 0 {
			changes.Add(1)
		}
	}))
	RunPoll(t, env)

	paused = hasLabel(IssueLabels(t, env, num), "fabrik:paused")
	return env.Claude.CommentCallCount("Review") - passesBefore, paused, int(changes.Load())
}

func TestDispatchReread_PausedByPhase1IsNotDispatchedInTheSamePoll(t *testing.T) {
	t.Parallel()
	passes, paused, _ := runPausedMidPollScenario(t, false)
	if !paused {
		t.Fatal("scenario did not reach the Phase 1 cycle-limit pause; the test proves nothing")
	}
	if passes != 0 {
		t.Fatalf("a comment pass was dispatched %d time(s) on an item Phase 1 paused in the same poll", passes)
	}
}

// Non-vacuity: with the re-read disabled the stale copy is dispatched.
func TestDispatchReread_NonVacuity_WithoutRereadThePausedItemIsDispatched(t *testing.T) {
	t.Parallel()
	passes, paused, workers := runPausedMidPollScenario(t, true)
	if !paused {
		t.Fatal("scenario did not reach the Phase 1 cycle-limit pause; the test proves nothing")
	}
	if passes == 0 && workers == 0 {
		t.Fatalf("without the re-read nothing was dispatched either (passes=%d, worker changes=%d): the scenario does not reproduce §3.1", passes, workers)
	}
	t.Log(fmt.Sprintf("without the re-read: passes=%d worker changes=%d", passes, workers))
}
