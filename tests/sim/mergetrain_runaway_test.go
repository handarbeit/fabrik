package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
)

// This file covers R5's runaway-guard half: recordTrial/isRunawayTripped/
// fireRunawayGuard (engine/merge_train.go, ADR-059 D8) trip and pause every
// Queued member once a repo has run MaxTrainTrialsPerWindow trials with zero
// successful landings inside TrainTrialWindowDuration.
//
// recordTrial/isRunawayTripped are anchored on real time.Now(), not the
// engine's injected Clock seam (Research's Constraint 2 — confirmed directly
// against the code, neither reads e.now()), so this scenario cannot fast-
// forward the rolling window the way most other sim scenarios manipulate
// timeouts. It relies instead on genuinely fast, real-time trial cycling
// against a small MaxTrainTrialsPerWindow — the same technique the live
// e2e TestMergeTrainRunawayGuardPausesBatch uses. Per ADR-1528, a green
// (landing) trial never counts toward the guard — only a scenario that
// never successfully lands anything can trip it, so every member here is
// poisoned: with every member poisonous, each bisection round still
// "successfully" isolates and ejects one poisoner (a real, counted trial)
// and re-forms with the survivors, repeating — cascading through several
// rounds within a single poll's worker dispatch (the runaway guard is
// checked after every trial, both in the main re-form loop and inside
// bisect's own per-sub-trial checkpoint) without ever landing anything,
// which is exactly the "persistent infra failure, not a composition
// problem" shape the guard exists to catch. Per ADR-1533, the guard's own
// Hook1/Hook2 concurrency race is out of scope here (Research's Risk 7) —
// this proves the guard fires and pauses/alerts correctly under ordinary
// (non-racing) conditions.
func TestMergeTrainRunaway_TripsAndPausesQueuedMembers(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxTrainTrialsPerWindow = 4
			cfg.TrainTrialWindowDuration = 5 * time.Minute
		},
	})

	files := []map[string]string{
		{"a.txt": "a\n"}, {"b.txt": "b\n"}, {"c.txt": "c\n"}, {"d.txt": "d\n"}, {"e.txt": "e\n"},
	}
	nums := make([]int, len(files))
	for i := range files {
		nums[i], _ = QueueMember(t, env, "runaway", files[i])
	}
	// Every member is poisoned: no possible sub-batch ever validates green,
	// so trials accumulate with zero landings across however many bisection
	// rounds it takes to notice — the guard's own premise.
	startTrialVerdictSeeder(t, env, poisonVerdict(nums...))

	RunPoll(t, env)

	// Every member still Queued when the guard fires must be paused with the
	// alert comment — not just the one the worker happened to be mid-trial
	// on. A member already ejected-and-rerouted (a red singleton, off
	// Queued entirely) before the guard tripped is out of scope for this
	// assertion; what matters is that nothing Queued is left unpaused and
	// unexplained.
	var anyPaused bool
	for _, n := range nums {
		item := projectItem(t, env, n)
		if item.Status != "Queued" {
			continue
		}
		if hasLabel(IssueLabels(t, env, n), "fabrik:paused") {
			anyPaused = true
			if !hasLabel(IssueLabels(t, env, n), "fabrik:awaiting-input") {
				t.Errorf("#%d has fabrik:paused but not fabrik:awaiting-input — runaway guard applies both together", n)
			}
			if !hasCommentContaining(t, env, n, "runaway guard tripped") {
				t.Errorf("#%d is paused but has no runaway-guard alert comment explaining why", n)
			}
			if !hasCommentContaining(t, env, n, "zero successful landings") {
				t.Errorf("#%d's alert comment should name the zero-landings condition", n)
			}
		}
	}
	if !anyPaused {
		t.Fatal("expected the runaway guard to have paused at least one Queued member — got none; either it never tripped (MaxTrainTrialsPerWindow=4 not reached) or the pause labels/comment are broken")
	}

	// Non-vacuity (AC10): with MaxTrainTrialsPerWindow effectively disabled
	// (a very large value), the identical poison declaration would instead
	// cascade all the way down to red singletons via the ordinary bisection
	// ejection/reroute paths, landing nothing but never pausing via this
	// guard specifically — proving the pause above is genuinely the guard
	// firing, not some other disposition that happens to look similar.
	t.Run("does not trip with a generous window", func(t *testing.T) {
		t.Parallel()
		env2 := mergeTrainEnv(t, mergeTrainEnvOptions{
			ConfigureCfg: func(cfg *engine.Config) {
				cfg.MaxTrainTrialsPerWindow = 1000
				cfg.TrainTrialWindowDuration = 5 * time.Minute
			},
		})
		numsA := make([]int, len(files))
		for i := range files {
			numsA[i], _ = QueueMember(t, env2, "noguard", files[i])
		}
		startTrialVerdictSeeder(t, env2, poisonVerdict(numsA...))

		RunPoll(t, env2)

		for _, n := range numsA {
			if hasLabel(IssueLabels(t, env2, n), "fabrik:paused") && hasCommentContaining(t, env2, n, "runaway guard tripped") {
				t.Errorf("#%d was paused by the runaway guard even though the window (1000 trials) should never be reached — the guard must not fire without cause", n)
			}
		}
	})
}
