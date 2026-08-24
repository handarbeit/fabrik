package sim

import (
	"testing"
)

// This file covers #1644's Scope requirement of "e2e coverage of both arms":
// AC1 (the fast path fires and lands the member's own PR directly, with no
// trial ever built) and AC2 (the fast path declines when the pinned base is
// not an ancestor of the member's head, and the ordinary trial path — a real
// draft CI PR, a real combined-Validate poll, a real landing integration PR —
// runs exactly as it did before this feature existed).
//
// engine/merge_train_test.go covers the eligibility decision's individual
// conditions (AC3/AC4, the TOCTOU guard, the restart/resume branch, the
// non-default-base close, and AC5's multi-member non-interference) against
// the mocked GitHubClient — this file is deliberately narrower: it proves the
// two ACs' end-to-end, real-git behavior once, rather than re-deriving every
// condition against the sim.

// TestMergeTrainSingletonFastPath_UpToDateAndGreen_LandsWithoutTrial is AC1: a
// single Queued member whose head already contains the pinned base, with a
// green, complete check run already on its own PR's head SHA, lands directly
// — no draft CI PR, no dedicated landing PR, just a direct merge of the
// member's own PR.
//
// Non-vacuousness (AC1's own requirement, "proved non-vacuous by reverting
// the guard and observing the trial return"): manually reverting
// trySingletonFastPath's wiring in runMergeTrainWorker and re-running this
// scenario was confirmed to produce draftPRCount(env) >= 1 — i.e. this
// scenario's green CI and clean ancestry are sufficient to land via the
// ordinary trial path too, so a zero draft-PR count here is actually
// evidence the fast path fired, not an artifact of a scenario the trial path
// could never have landed anyway.
func TestMergeTrainSingletonFastPath_UpToDateAndGreen_LandsWithoutTrial(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, prNum := QueueMember(t, env, "fastpath-uptodate", map[string]string{"fastpath-a.txt": "a\n"})

	pr, err := env.Sim.Sim().FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("could not resolve queued member's own linked PR: %v", err)
	}
	// Seed a green, complete check run directly on the member's OWN PR head
	// SHA — not a trial SHA (startTrialVerdictSeeder is for scenarios that
	// expect a trial to be built; this scenario asserts the opposite).
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, greenCheckRun(""))

	RunPoll(t, env)

	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)

	if got := draftPRCount(env); got != 0 {
		t.Errorf("expected no draft CI PR (no trial should be built), got %d", got)
	}
	if got := len(env.Sim.Log().ByMethod("CreatePR")); got != 0 {
		t.Errorf("expected no dedicated landing PR either, got %d CreatePR call(s)", got)
	}

	// The fast path merges via MergePRAtHeadSHA (#1644 review fix), not the
	// unpinned MergePR — pinning the merge to the SHA singletonFastPathEligible
	// actually validated, since (unlike every other merge-train landing path)
	// this merges the member's own, externally-writable PR branch directly.
	if got := len(env.Sim.Log().ByMethod("MergePR")); got != 0 {
		t.Errorf("expected the unpinned MergePR never called by the fast path, got %d", got)
	}
	merges := env.Sim.Log().ByMethod("MergePRAtHeadSHA")
	if len(merges) != 1 {
		t.Fatalf("expected exactly 1 MergePRAtHeadSHA call, got %d", len(merges))
	}
	if merges[0].Args.Number != prNum {
		t.Errorf("expected the merged PR to be the member's own PR #%d, got #%d", prNum, merges[0].Args.Number)
	}
	if len(merges[0].Args.Values) != 1 || merges[0].Args.Values[0] != pr.HeadSHA {
		t.Errorf("expected the merge pinned to the member's validated head SHA %s, got %+v", pr.HeadSHA, merges[0].Args.Values)
	}
}

// TestMergeTrainSingletonFastPath_BaseMoved_StillBuildsTrial is AC2 — the
// regression that matters most: a direct push lands on the default branch
// after the member's own branch forked from it, so the pinned base is no
// longer an ancestor of the member's head. The combination (member + new
// base tip) is genuinely untested, so the fast path must decline and the
// ordinary trial — a real draft CI PR, a real combined-Validate poll, a real
// landing integration PR distinct from the member's own PR — must run
// exactly as it did before this feature existed.
func TestMergeTrainSingletonFastPath_BaseMoved_StillBuildsTrial(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})

	num, prNum := QueueMember(t, env, "fastpath-basemoved", map[string]string{"fastpath-b.txt": "b\n"})

	// Simulate an external direct push to the default branch, landing after
	// the member's own branch already forked from it — the pinned base SHA
	// (captured when the train worker starts) will not be an ancestor of the
	// member's head.
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, "main", "main",
		map[string]string{"external-push.txt": "external\n"}, "external direct push advancing main")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding external direct push: %v", err)
	}

	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	WaitForProjectStatus(t, env, num, "Done", 20)
	WaitForIssueClosed(t, env, num, 5)

	if got := draftPRCount(env); got < 1 {
		t.Errorf("expected the ordinary trial path to build at least 1 draft CI PR, got %d", got)
	}

	// Landed via the ordinary landMergeTrainBatch path (a green batch always
	// lands immediately regardless of its size — D-d), which merges a
	// dedicated integration PR, never the member's own PR directly.
	merges := env.Sim.Log().ByMethod("MergePR")
	if len(merges) != 1 {
		t.Fatalf("expected exactly 1 MergePR call, got %d", len(merges))
	}
	if merges[0].Args.Number == prNum {
		t.Errorf("expected the merged PR to be a dedicated integration PR, not the member's own PR #%d", prNum)
	}
}
