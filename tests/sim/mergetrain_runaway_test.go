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
}

// TestMergeTrainRunaway_DoesNotTripWithGenerousWindow is the non-vacuity
// (AC10) negative control for TestMergeTrainRunaway_TripsAndPausesQueuedMembers
// above: with MaxTrainTrialsPerWindow effectively disabled (a very large
// value), the identical poison declaration instead cascades all the way down
// to red singletons via the ordinary bisection ejection/reroute paths,
// landing nothing but never pausing via this guard specifically — proving
// the positive control's pause is genuinely the guard firing, not some other
// disposition that happens to look similar.
//
// This is a standalone top-level test, not a t.Run subtest of the positive
// control, deliberately: a t.Parallel() subtest pauses until its parent test
// function returns and then runs in the package's shared parallel wave, but
// the parent's own mergeTrainSlots slot (acquired by its own mergeTrainEnv
// call) is not released until every one of its subtests — including this
// one — completes, since t.Cleanup only fires once the whole subtree is
// done. Nesting this scenario there meant one logical test held two of the
// package's three mergeTrainSlots concurrency-limiting slots for the
// subtest's entire real-git-work duration, cutting the effective concurrency
// available to every other merge-train scenario from 3 to 2 and worsening
// exactly the contention mergeTrainSlots exists to bound (review finding on
// this PR). Two independent top-level tests each acquire exactly one slot
// for their own natural lifetime, with no such overlap.
func TestMergeTrainRunaway_DoesNotTripWithGenerousWindow(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.MaxTrainTrialsPerWindow = 1000
			cfg.TrainTrialWindowDuration = 5 * time.Minute
		},
	})

	files := []map[string]string{
		{"a.txt": "a\n"}, {"b.txt": "b\n"}, {"c.txt": "c\n"}, {"d.txt": "d\n"}, {"e.txt": "e\n"},
	}
	nums := make([]int, len(files))
	for i := range files {
		nums[i], _ = QueueMember(t, env, "noguard", files[i])
	}
	startTrialVerdictSeeder(t, env, poisonVerdict(nums...))

	RunPoll(t, env)

	for _, n := range nums {
		if hasLabel(IssueLabels(t, env, n), "fabrik:paused") && hasCommentContaining(t, env, n, "runaway guard tripped") {
			t.Errorf("#%d was paused by the runaway guard even though the window (1000 trials) should never be reached — the guard must not fire without cause", n)
		}
	}
}
