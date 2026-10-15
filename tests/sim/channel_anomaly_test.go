package sim

import (
	"context"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"strconv"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Anomaly events (#2170, ADR 2170): an overseer subscribed to the channel learns,
// from a push, about the per-item conditions that used to be visible only in the
// engine log. The sim clock is shared with the engine, so a threshold is crossed
// with Clock.Advance and nothing sleeps for it; the deriver's tick is shortened
// (startPipelineChannel). Real time is spent only on the engine's own retry
// backoff, which runs on the wall clock (see retryCooldownPolls).

func newAnomalyEnv(t *testing.T) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	// Set before the hub starts: the consumer reads the thresholds unlocked.
	env.Engine.SetAnomalyThresholdsForTest(5*time.Minute, 10*time.Minute, 5*time.Minute)
	return env
}

// A3/A8: #1805, #2129 and #2135 were killed by max_wall_time and then waited for
// a worker slot. The retry is due, every slot is held, and the overseer is told
// once - with the cause and the reason - and told again when it finally runs.
func TestRetryStarved_WallTimeKillWaitingForASlot(t *testing.T) {
	t.Parallel()
	env := newAnomalyEnv(t)
	_, sink := startPipelineChannel(t, env)
	env.Claude.ForStage("Specify", simclaude.WallTimeKilled(), simclaude.DefaultScript)

	num := FileIssue(t, env, "wall-time kill", "body", "Specify")
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 80)

	// Saturate the pool once the killed worker has released its slot. The send
	// blocks until it has, which is also when the retry was recorded.
	release := env.Engine.HoldGeneralWorkerSlotsForTest()
	defer release()

	waitRealTime(t, 150*time.Millisecond, "negative window over the 25 ms deriver ticker: retry-starved is tick-driven, nothing to drain")
	if n := len(sink.ofType(channelevents.RetryStarved, 0)); n != 0 {
		t.Fatalf("retry-starved fired before the retry was %d minutes overdue", 5)
	}

	env.Clock.Advance(10 * time.Minute)
	got := waitForEvents(t, sink, channelevents.RetryStarved, 1)[0]
	m := got.Meta
	if got.Issue != num || m["cause"] != "wall-time-kill" || m["reason"] != "no worker slot" || m["slots_in_use"] == "" ||
		m["slots_in_use"] != m["slots_max"] || m["due_at"] == "" || m["stage"] != "Specify" {
		t.Fatalf("retry-starved = %+v", got)
	}
	if n := len(sink.ofType(channelevents.RetryResumed, 0)); n != 0 {
		t.Fatalf("retry-resumed before the item was dispatched: %d", n)
	}

	// A slot frees: the retry is dispatched and the stage completes - exactly one
	// recovery, and still exactly one starved.
	release()
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", retryCooldownPolls)
	waitForEvents(t, sink, channelevents.RetryResumed, 1)
	waitForEvents(t, sink, channelevents.RetryStarved, 1)
}

// Non-vacuity (FR-014): with the detector neutralised the same run publishes nothing.
func TestRetryStarved_NonVacuous(t *testing.T) {
	t.Parallel()
	env := newAnomalyEnv(t)
	_, sink := startPipelineChannel(t, env)
	env.Claude.ForStage("Specify", simclaude.WallTimeKilled(), simclaude.DefaultScript)
	// Per-engine switch (#2210): kept for the whole test, and visible to no other.
	restore := env.Engine.SetAnomalyDetectorsDisabledForTest(true, false, false)
	defer restore()

	FileIssue(t, env, "wall-time kill", "body", "Specify")
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Specify") >= 1 }, 80)
	release := env.Engine.HoldGeneralWorkerSlotsForTest()
	defer release()
	env.Clock.Advance(10 * time.Minute)
	waitRealTime(t, 300*time.Millisecond, "negative window over the 25 ms deriver ticker: the neutralised detector must stay silent across several ticks")
	if n := len(sink.ofType(channelevents.RetryStarved, 0)); n != 0 {
		t.Fatalf("a disabled detector published %d retry-starved events", n)
	}
}

// A11: the feedback gate held #2125's advance for over an hour with no dispatch
// (engine defect #2160). The Review stage completes while a body-only bot review
// is outstanding on its PR, so the gate holds the advance on every poll.
func wedgeEnv(t *testing.T) *Env {
	t.Helper()
	env := newFeedbackGateEnv(t, nil)
	// Before the hub starts: the consumer reads the thresholds unlocked.
	env.Engine.SetAnomalyThresholdsForTest(5*time.Minute, 10*time.Minute, 5*time.Minute)
	env.Claude.ForStage("Review", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if pr, _ := env.Sim.FetchLinkedPR(env.Owner, env.Repo, issue.Number); pr != nil {
			env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{
				Author: "handarbeit-pruefer", State: "COMMENTED", Body: feedbackReviewBody, DatabaseID: feedbackReviewID,
			})
		}
		return simclaude.DefaultScript(ctx, stage, issue, nc, resume, workDir, opts)
	})
	return env
}

// pollMinutes runs n polls a minute of sim time apart, pausing after each so the
// deriver (whose tick is real time) observes the engine between polls.
func pollMinutes(t *testing.T, env *Env, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		env.Clock.Advance(time.Minute)
		RunPoll(t, env)
		waitRealTime(t, 60*time.Millisecond, "the deriver observes the engine on its own 25 ms ticker between polls; the wedge detector reads the hold from that tick")
	}
}

func TestItemWedged_FeedbackHoldWithNoDispatch(t *testing.T) {
	t.Parallel()
	// The reinvoke that would clear the hold is suppressed, which reproduces #2160's
	// shape without depending on that defect staying unfixed.
	env := wedgeEnv(t)
	defer env.Engine.SetReviewReinvokeDisabledForTest()()
	_, sink := startPipelineChannel(t, env)
	num := FileIssue(t, env, "wedged gate", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)

	pollMinutes(t, env, 14)

	// The scenario must really be the one under test: the gate is holding the
	// advance, nothing was dispatched for the review body.
	if st := projectItem(t, env, num).Status; st != "Review" {
		t.Skipf("the feedback gate is not holding the advance (item is in %q): this scenario no longer reproduces a hold, so a pass would be vacuous", st)
	}
	if n := env.Claude.CommentCallCount("Review"); n != 0 {
		t.Skipf("a reinvoke was dispatched (%d): this scenario no longer reproduces a hold with no dispatch", n)
	}

	got := waitForEvents(t, sink, channelevents.ItemWedged, 1)[0]
	m := got.Meta
	if got.Issue != num || m["gate"] != "feedback" || m["feedback_ids"] != strconv.Itoa(feedbackReviewID) || m["held_seconds"] == "" {
		t.Fatalf("item-wedged = %+v", got)
	}
	pollMinutes(t, env, 4)
	waitForEvents(t, sink, channelevents.ItemWedged, 1) // a persisting hold does not repeat
	if n := len(sink.ofType(channelevents.ItemUnwedged, 0)); n != 0 {
		t.Fatalf("item-unwedged while the hold persists: %d", n)
	}
}

// The same hold, but the reinvoke the engine normally dispatches goes out inside
// the threshold: nothing is wedged.
func TestItemWedged_NotRaisedWhenAReinvokeIsDispatched(t *testing.T) {
	t.Parallel()
	env := wedgeEnv(t)
	_, sink := startPipelineChannel(t, env)
	num := FileIssue(t, env, "dispatched gate", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)

	pollMinutes(t, env, 14)

	if n := env.Claude.CommentCallCount("Review"); n == 0 {
		t.Fatal("the review body was never dispatched as a reinvoke")
	}
	if n := len(sink.ofType(channelevents.ItemWedged, 0)); n != 0 {
		t.Fatalf("item-wedged = %d for an item that was dispatched in time", n)
	}
}

// Non-vacuity (FR-014): with the detector neutralised the wedge publishes nothing.
func TestItemWedged_NonVacuous(t *testing.T) {
	t.Parallel()
	env := wedgeEnv(t)
	defer env.Engine.SetReviewReinvokeDisabledForTest()()
	defer env.Engine.SetAnomalyDetectorsDisabledForTest(false, true, false)()
	_, sink := startPipelineChannel(t, env)
	num := FileIssue(t, env, "disabled detector", "body", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)
	pollMinutes(t, env, 14)
	if n := len(sink.ofType(channelevents.ItemWedged, 0)); n != 0 {
		t.Fatalf("a disabled detector published %d item-wedged events", n)
	}
}
