package sim

import (
	"testing"
)

// This file covers R1 (drive the real assembly path; no trainValidateFn —
// see this issue's own doc comment inventory in mergetrain_helpers.go) and
// R2's poison-set matrix: every scenario here goes through
// engine.assembleTrialBranch against a real bare origin with real member
// branches, and declares which member combinations are red by scripting
// check runs keyed on the trial branch's real (post-assembly) HEAD SHA —
// never via trainValidateFn, which would bypass assembly, conflict
// resolution, trial branches and SHAs entirely (the thing this file exists
// to exercise). No scenario in this file sets trainValidateFn (AC9).
//
// Non-vacuity (AC10) for the poison matrix: each scenario asserts the
// *exact* survivor/ejection identity (which member landed, which member has
// the ejection comment) rather than merely "something landed" — a bisection
// regression that isolates the wrong member, or a poisoner that leaks
// through as a false survivor, fails the specific scenario naming that
// member, not just the suite as a whole.

// TestMergeTrainAssembly_RealConflictResolved is R1/AC1: two members write
// the same path with conflicting content, forcing assembleTrialBranch's
// real `git merge --no-ff` to hit a genuine conflict — not a scripted
// verdict. Claude's scripted resolver (registered under the holding stage's
// own name, "Queued" — resolveConflictWithClaude's actual call site, per
// Research) resolves it for real (writes merged content, commits), and both
// members land.
func TestMergeTrainAssembly_RealConflictResolved(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	startTrialVerdictSeeder(t, env, allGreenVerdict)
	env.Claude.ForStageComments("Queued", claudeResolveConflict("shared.txt", "merged-content-from-both\n"))

	numA, _ := QueueMember(t, env, "conflict-a", map[string]string{"shared.txt": "content-from-a\n"})
	numB, _ := QueueMember(t, env, "conflict-b", map[string]string{"shared.txt": "content-from-b\n"})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 20)
	WaitForIssueClosed(t, env, numA, 5)
	WaitForIssueClosed(t, env, numB, 5)

	// Proof the conflict was genuine, not fabricated: Claude's conflict
	// resolver was actually invoked. A scripted verdict (trainValidateFn)
	// would never reach resolveTrainConflict at all — assembleTrialBranch's
	// `git merge` only dispatches to Claude when the merge command itself
	// reports a real conflict.
	if got := env.Claude.CommentCallCount("Queued"); got == 0 {
		t.Error("expected the conflict resolver to have been invoked at least once — if this fires, the two members merged cleanly and this test proves nothing about conflict resolution")
	}
}

// TestMergeTrainAssembly_PoisonMatrix is R2's core: a table of poison-set
// shapes, each asserting the bisection algorithm's exact survivor and
// ejection identity. Every member writes a distinct file, so there is never
// a real git conflict in this table — CI verdict is the only thing under
// test here (real conflicts are TestMergeTrainAssembly_RealConflictResolved's
// job, and R4's — collapsing both concerns into one matrix would make a
// failure ambiguous about which mechanism broke).
func TestMergeTrainAssembly_PoisonMatrix(t *testing.T) {
	t.Parallel()

	t.Run("all-green lands the whole batch", func(t *testing.T) {
		t.Parallel()
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})
		startTrialVerdictSeeder(t, env, allGreenVerdict)

		numA, _ := QueueMember(t, env, "green-a", map[string]string{"a.txt": "a\n"})
		numB, _ := QueueMember(t, env, "green-b", map[string]string{"b.txt": "b\n"})
		numC, _ := QueueMember(t, env, "green-c", map[string]string{"c.txt": "c\n"})

		RunPoll(t, env)

		for _, n := range []int{numA, numB, numC} {
			WaitForProjectStatus(t, env, n, "Done", 20)
			WaitForIssueClosed(t, env, n, 5)
		}
	})

	t.Run("single poison at first position isolated and ejected", func(t *testing.T) {
		t.Parallel()
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})

		numA, _ := QueueMember(t, env, "poison-first-a", map[string]string{"a.txt": "a\n"})
		numB, _ := QueueMember(t, env, "poison-first-b", map[string]string{"b.txt": "b\n"})
		numC, _ := QueueMember(t, env, "poison-first-c", map[string]string{"c.txt": "c\n"})
		startTrialVerdictSeeder(t, env, poisonVerdict(numA))

		RunPoll(t, env)

		WaitForProjectStatus(t, env, numB, "Done", 20)
		WaitForProjectStatus(t, env, numC, "Done", 20)
		WaitForIssueClosed(t, env, numB, 5)
		WaitForIssueClosed(t, env, numC, 5)

		WaitForProjectStatus(t, env, numA, "Queued", 5)
		if projectItem(t, env, numA).IsClosed {
			t.Error("poisoned member A must remain open (ejected, not landed)")
		}
		if !hasCommentContaining(t, env, numA, "ejected") {
			t.Error("expected an ejection comment on poisoned member A")
		}
	})

	t.Run("single poison at middle position isolated and ejected", func(t *testing.T) {
		t.Parallel()
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})

		numA, _ := QueueMember(t, env, "poison-mid-a", map[string]string{"a.txt": "a\n"})
		numB, _ := QueueMember(t, env, "poison-mid-b", map[string]string{"b.txt": "b\n"})
		numC, _ := QueueMember(t, env, "poison-mid-c", map[string]string{"c.txt": "c\n"})
		startTrialVerdictSeeder(t, env, poisonVerdict(numB))

		RunPoll(t, env)

		WaitForProjectStatus(t, env, numA, "Done", 20)
		WaitForProjectStatus(t, env, numC, "Done", 20)
		WaitForIssueClosed(t, env, numA, 5)
		WaitForIssueClosed(t, env, numC, 5)

		WaitForProjectStatus(t, env, numB, "Queued", 5)
		if projectItem(t, env, numB).IsClosed {
			t.Error("poisoned member B must remain open (ejected, not landed)")
		}
		if !hasCommentContaining(t, env, numB, "ejected") {
			t.Error("expected an ejection comment on poisoned member B")
		}
	})

	t.Run("single poison at last position isolated and ejected", func(t *testing.T) {
		t.Parallel()
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})

		numA, _ := QueueMember(t, env, "poison-last-a", map[string]string{"a.txt": "a\n"})
		numB, _ := QueueMember(t, env, "poison-last-b", map[string]string{"b.txt": "b\n"})
		numC, _ := QueueMember(t, env, "poison-last-c", map[string]string{"c.txt": "c\n"})
		startTrialVerdictSeeder(t, env, poisonVerdict(numC))

		RunPoll(t, env)

		WaitForProjectStatus(t, env, numA, "Done", 20)
		WaitForProjectStatus(t, env, numB, "Done", 20)
		WaitForIssueClosed(t, env, numA, 5)
		WaitForIssueClosed(t, env, numB, 5)

		WaitForProjectStatus(t, env, numC, "Queued", 5)
		if projectItem(t, env, numC).IsClosed {
			t.Error("poisoned member C must remain open (ejected, not landed)")
		}
		if !hasCommentContaining(t, env, numC, "ejected") {
			t.Error("expected an ejection comment on poisoned member C")
		}
	})

	t.Run("two independent poison members both isolated and ejected", func(t *testing.T) {
		t.Parallel()
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})

		numA, _ := QueueMember(t, env, "poison2-a", map[string]string{"a.txt": "a\n"})
		numB, _ := QueueMember(t, env, "poison2-b", map[string]string{"b.txt": "b\n"})
		numC, _ := QueueMember(t, env, "poison2-c", map[string]string{"c.txt": "c\n"})
		numD, _ := QueueMember(t, env, "poison2-d", map[string]string{"d.txt": "d\n"})
		startTrialVerdictSeeder(t, env, poisonVerdict(numB, numD))

		RunPoll(t, env)

		WaitForProjectStatus(t, env, numA, "Done", 20)
		WaitForProjectStatus(t, env, numC, "Done", 20)
		WaitForIssueClosed(t, env, numA, 5)
		WaitForIssueClosed(t, env, numC, 5)

		for _, n := range []int{numB, numD} {
			WaitForProjectStatus(t, env, n, "Queued", 5)
			if projectItem(t, env, n).IsClosed {
				t.Errorf("poisoned member #%d must remain open (ejected, not landed)", n)
			}
			if !hasCommentContaining(t, env, n, "ejected") {
				t.Errorf("expected an ejection comment on poisoned member #%d", n)
			}
		}
	})

	t.Run("member poisonous only in combination is not ejected when validated alone", func(t *testing.T) {
		t.Parallel()
		env := mergeTrainEnv(t, mergeTrainEnvOptions{})

		numA, _ := QueueMember(t, env, "interaction-a", map[string]string{"a.txt": "a\n"})
		numB, _ := QueueMember(t, env, "interaction-b", map[string]string{"b.txt": "b\n"})
		startTrialVerdictSeeder(t, env, interactionPoisonVerdict(numA, numB))

		RunPoll(t, env)

		// The redness spans the split (bisect's own base case with 2
		// members: mid=1, both halves are singletons — [A] alone and [B]
		// alone both validate green under interactionPoisonVerdict, which
		// is bisect's "non-isolable interaction" case (D-e)). It degrades
		// to the one-at-a-time fallback, which validates and lands each
		// member individually — and individually, neither is poisoned, so
		// both land. This is the deliberate, documented behavior for a
		// non-isolable cross-PR interaction (FR-5): the fallback dissolves
		// the interaction by construction, since no two members co-reside
		// in a one-at-a-time trial.
		WaitForProjectStatus(t, env, numA, "Done", 20)
		WaitForProjectStatus(t, env, numB, "Done", 20)
		WaitForIssueClosed(t, env, numA, 5)
		WaitForIssueClosed(t, env, numB, 5)
	})
}
