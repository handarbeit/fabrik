package sim

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
)

// This file covers R3: the bisection algorithm's trial count is exercised
// against known batch shapes, and the cost cap (effectiveBisectCap,
// engine/merge_train.go) is proven to actually bound it rather than merely
// being computed and ignored.
//
// bisect (engine/merge_train.go) tests red[:mid] first and only tests
// red[mid:] when the first half comes back green (bors-ng test order) — so
// the trial count to isolate a single poisoner is position-dependent, not a
// single closed-form number: a poisoner in the first half at every level
// costs one trial per level (the second half is never tested); a poisoner
// that keeps landing in the second half costs two. For a 3-member batch
// (mid=1 at every level, since each recursion after finding a red pair has
// exactly 2 members and bisect's own base case — len(red)==1 — costs zero
// further trials to confirm), this issue's own worked derivation (pinned
// directly against the real algorithm, not asserted a priori) gives:
//
//	poison at index 0 (first):  2 trials (1 initial + 1 isolating)
//	poison at index 1 (middle): 4 trials (1 initial + 2 first-level + 1 isolating)
//	poison at index 2 (last):   5 trials (1 initial + 2 first-level + 2 second-level)
//
// The "last" position is the worst case for N=3 and lands exactly on
// effectiveBisectCap()'s default formula (2*ceilLog2(3)+1 = 2*2+1 = 5) — not
// a coincidence: the cap is derived from exactly this worst-case shape
// (ADR-059 D-f). A regression that makes bisection less efficient (e.g. an
// off-by-one in the halving or the used-budget check) would either exceed
// this count directly or blow the cap and silently fall back to
// one-at-a-time, both of which the exact-count assertions below catch.
func TestMergeTrainBisect_TrialCountsMatchExpected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		poisonIndex    int // 0, 1, or 2 of a 3-member batch
		expectedTrials int
	}{
		{"poison at first position costs 2 trials", 0, 2},
		{"poison at middle position costs 4 trials", 1, 4},
		{"poison at last position costs 5 trials (== cap)", 2, 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := mergeTrainEnv(t, mergeTrainEnvOptions{})

			nums := make([]int, 3)
			files := []map[string]string{
				{"a.txt": "a\n"},
				{"b.txt": "b\n"},
				{"c.txt": "c\n"},
			}
			for i := range nums {
				nums[i], _ = QueueMember(t, env, "bisectcount", files[i])
			}
			startTrialVerdictSeeder(t, env, poisonVerdict(nums[tc.poisonIndex]))

			before := draftPRCount(env)
			RunPoll(t, env)

			poisoned := nums[tc.poisonIndex]
			WaitForProjectStatus(t, env, poisoned, "Queued", 20)
			if !hasCommentContaining(t, env, poisoned, "ejected") {
				t.Fatalf("expected #%d to have been ejected as the isolated poisoner", poisoned)
			}
			var survivors []int
			for i, n := range nums {
				if i != tc.poisonIndex {
					survivors = append(survivors, n)
				}
			}
			for _, n := range survivors {
				WaitForProjectStatus(t, env, n, "Done", 10)
			}

			// The episode's own trial count: every draft CI PR opened up to
			// and including the isolating trial, but not the next episode's
			// fresh re-formed-batch validation (that belongs to a new episode
			// with its own used:=1 budget, not this one — counting it would
			// conflate two different episodes' costs into one number).
			got := draftPRCount(env) - before - 1 // -1 excludes the re-formed survivors' own green land trial
			if got != tc.expectedTrials {
				t.Errorf("poison at index %d: got %d trials for this red-batch episode, want %d (draft PR count before=%d, after=%d)",
					tc.poisonIndex, got, tc.expectedTrials, before, draftPRCount(env))
			}
		})
	}
}

// TestMergeTrainBisect_CostCapHonored is R3's second half: with a
// deliberately small MaxBisectValidations (2, well under what a 3-member
// batch's worst-case isolation would need per the table above), the episode
// must stop bisecting once the cap is reached and degrade to the
// one-at-a-time fallback (FR-5, landOneAtATime) — never simply keep trying
// until it finds an answer. This is what makes the cap meaningful: an
// unbounded or off-by-one bisection would burn one more CI run than budgeted
// and be invisible until someone reads a log (the issue's own framing).
//
// Non-vacuity (AC10): if handleRedBatch's `*used >= costCap` check were
// broken (e.g. checked after issuing the trial instead of before, or with a
// `>` instead of `>=`), this batch would either take more than 2 trials to
// reach the fallback, or would successfully isolate the poisoner via
// bisection instead of falling back at all — both are exactly what the
// assertions below distinguish.
func TestMergeTrainBisect_CostCapHonored(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxBisectValidations = 2 },
	})

	numA, prA := QueueMember(t, env, "cap-a", map[string]string{"a.txt": "a\n"})
	numB, prB := QueueMember(t, env, "cap-b", map[string]string{"b.txt": "b\n"})
	numC, _ := QueueMember(t, env, "cap-c", map[string]string{"c.txt": "c\n"})
	// Poison at the last position — the shape that would otherwise need 5
	// trials (the table above) to isolate, well beyond this test's cap of 2.
	startTrialVerdictSeeder(t, env, poisonVerdict(numC))

	before := draftPRCount(env)
	RunPoll(t, env)

	// A and B land via landOneAtATime's own singleton validation (genuinely
	// clean alone); C — poisoned regardless of combination — fails even its
	// own singleton trial and gets the same red-singleton disposition
	// TestMergeTrainRedSingleton_ReroutesOffQueued pins (#1440/#1545): reroute
	// off Queued, not bisect-and-eject, since a singleton trial has nothing
	// left to isolate.
	WaitForProjectStatus(t, env, numA, "Done", 20)
	WaitForProjectStatus(t, env, numB, "Done", 10)
	WaitForProjectStatus(t, env, numC, "Validate", 10)
	WaitForIssueLabel(t, env, numC, "fabrik:paused", 5)

	// landSingleton's "Landed one-at-a-time" comment goes on the member's own
	// PR (addLandedCommentWithRetry posts via prNum, not issueNumber) — check
	// the mutation log directly rather than the issue's comment thread.
	for _, pr := range []int{prA, prB} {
		var found bool
		for _, e := range env.Sim.Log().ByMethod("AddComment") {
			if e.Args.Number == pr && strings.Contains(e.Args.Body, "Landed one-at-a-time") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected PR #%d to carry a 'Landed one-at-a-time' comment (cost-cap exhaustion), not combined bisection", pr)
		}
	}
	if !hasCommentContaining(t, env, numC, "own combined Validate is failing") {
		t.Errorf("expected #%d (poisoned even alone) to be disposed as a red singleton by the one-at-a-time fallback", numC)
	}

	// The whole episode's draft-PR count pins the cap: exactly 2 spent on the
	// abandoned bisection attempt (the initial combined-3 trial, then one
	// partial-bisection trial before the `*used >= costCap` check trips) plus
	// exactly 3 more from landOneAtATime's per-member singleton trials (one
	// each for A, B, and C) — 5 total. A broken cap check that let bisection
	// keep going (or one that gave up a trial too early) would change this
	// count directly, since every real trial — bisection or singleton — opens
	// exactly one draft CI PR (assembleAndValidateInner, unconditionally).
	got := draftPRCount(env) - before
	if got != 5 {
		t.Errorf("expected exactly 5 draft PRs for this episode (2 bisection + 3 one-at-a-time singletons), got %d", got)
	}
}
