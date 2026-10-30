package sim

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Sim twins for #2528 (ADR 2528): a stage completes and the worker advances the
// item, but a poll drains the column-move admission while the worker is still
// registered. The poll drops the item (a worker is in flight) and stamps
// periodic-re-eval; WorkerExited is excluded from cycleSetFlags (ADR 039), so
// without the fix the next stage waits for the 10 × PollSeconds re-evaluation.

const advanceInFlightPollSeconds = 180

func advanceInFlightStages() []*stages.Stage {
	yes := true
	return []*stages.Stage{
		{Name: "Research", Order: 1, AutoAdvance: &yes},
		{Name: "Plan", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// advanceInFlightEnv wires a hook that, right after the worker's Status write,
// applies the write-through and runs a poll with the worker still registered.
func advanceInFlightEnv(t *testing.T, fixOff bool) (*Env, int) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages: advanceInFlightStages(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.PollSeconds = advanceInFlightPollSeconds
		},
	})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Engine.SetPostRunAdvanceAdmissionDisabledForTest(fixOff)
	num := FileIssue(t, env, "advance while the worker runs", "body", "Research")
	var fired atomic.Bool
	env.Engine.SetAfterAdvanceHookForTest(func(item gh.ProjectItem, newStatus string) {
		if item.Number != num || newStatus != "Plan" || !fired.CompareAndSwap(false, true) {
			return
		}
		env.Engine.SimulateCacheStatusWriteThroughForTest(env.OwnerRepo, num, newStatus)
		// The wake of the move runs a poll while this worker is still in flight.
		if err := env.Engine.PollOnce(context.Background()); err != nil {
			t.Errorf("mid-run poll: %v", err)
		}
	})
	t.Cleanup(func() { env.Engine.SetAfterAdvanceHookForTest(nil) })
	return env, num
}

// SC-001: the next stage is dispatched within a poll or two of the worker's exit.
func TestAdvanceWhileWorkerInFlightStartsNextStagePromptly(t *testing.T) {
	t.Parallel()
	env, num := advanceInFlightEnv(t, false)
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Research") >= 1 }, 10)
	for poll := 1; poll <= 4; poll++ {
		RunPoll(t, env)
		if env.Claude.StageCallCount("Plan") >= 1 {
			return
		}
	}
	t.Fatalf("Plan on #%d not dispatched within 4 polls of the advance (10 × poll is the unfixed wait)\n\n%s", num, diagnostics(env))
}

// SC-003: with the fix neutralised the same scenario stalls until the periodic
// re-evaluation, which proves the scenario above exercises the failure.
func TestAdvanceWhileWorkerInFlightStallsWithoutTheFix(t *testing.T) {
	t.Parallel()
	env, num := advanceInFlightEnv(t, true)
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Research") >= 1 }, 10)
	RunPolls(t, env, 6) // well short of the 10-poll periodic re-evaluation
	if got := env.Claude.StageCallCount("Plan"); got != 0 {
		t.Fatalf("Plan on #%d dispatched %d time(s) without the fix; the scenario no longer reproduces the stall\n\n%s", num, got, diagnostics(env))
	}
	// The periodic re-evaluation is still the backstop.
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.StageCallCount("Plan") >= 1 }, 12)
}
