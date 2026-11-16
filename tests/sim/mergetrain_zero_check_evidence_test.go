package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file is the sim twin of #2593 (ADR 2593): the merge train must not land a trial whose
// CI has not started yet. During a GitHub Actions delay the trial has no check runs, no
// workflow runs and no suites — indistinguishable from a repository with no CI — so the
// engine landed it. A member PR head carrying check runs is the positive evidence the
// repository runs CI, and the trial is then held (pending, bounded by CIBackstopTimeout).
//
// There is no live twin: the delayed-workflow-creation window cannot be induced on demand
// against the e2e bed (see adrs/2593-*.md). The trial PR is a draft, which the sim reports
// as "draft" (never accepted); SetDraftMergeableState("clean") models real GitHub's reading
// of the draft trial PR in the incident (tests/sim/simgh/FIDELITY.md).

const zeroCheckBackstop = 1500 * time.Millisecond

func zeroCheckEnv(t *testing.T, withMemberCI bool) (env *Env, a, b int) {
	t.Helper()
	env = mergeTrainEnv(t, mergeTrainEnvOptions{ConfigureCfg: func(cfg *engine.Config) {
		cfg.CIBackstopTimeout = zeroCheckBackstop
	}})
	env.Sim.Sim().SetDraftMergeableState(env.OwnerRepo, "clean")
	a, _ = QueueMember(t, env, "zero-check-a", map[string]string{"a.txt": "a\n"})
	b, _ = QueueMember(t, env, "zero-check-b", map[string]string{"b.txt": "b\n"})
	if withMemberCI {
		seedMemberCI(t, env, a, greenCheckRun("build"))
		seedMemberCI(t, env, b, greenCheckRun("build"))
	}
	return env, a, b
}

func successfulMerges(env *Env) int {
	n := 0
	for _, e := range env.Sim.Log().ByMethod("MergePRAtHeadSHA") {
		if e.Err == nil {
			n++
		}
	}
	return n
}

// The Actions delay: members carry check runs, the trial has nothing. It must never land.
func TestMergeTrainZeroCheck_MembersHaveChecks_TrialHeldNotLanded(t *testing.T) {
	t.Parallel()
	env, a, b := zeroCheckEnv(t, true)
	startTrialSeeder(t, env, func([]int) []gh.CheckRun { return nil }, &simgh.ReadyForReviewStep{})

	for i := 0; i < 3; i++ {
		RunPoll(t, env)
	}

	for _, n := range []int{a, b} {
		if st := projectItem(t, env, n).Status; st != "Queued" {
			t.Errorf("#%d status = %q, want Queued — a trial with no CI run must be held", n, st)
		}
		if projectItem(t, env, n).IsClosed {
			t.Errorf("#%d was closed — an unvalidated batch landed", n)
		}
	}
	if m := successfulMerges(env); m != 0 {
		t.Errorf("successful merges = %d, want 0", m)
	}
}

// Non-vacuity: with the rule neutralised the same scenario lands the unvalidated trial.
func TestMergeTrainZeroCheck_MembersHaveChecks_NeutralisedLands(t *testing.T) {
	t.Parallel()
	env, a, b := zeroCheckEnv(t, true)
	env.Engine.SetZeroCheckMemberEvidenceDisabledForTest(true)
	startTrialSeeder(t, env, func([]int) []gh.CheckRun { return nil }, &simgh.ReadyForReviewStep{})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, a, "Done", 20)
	WaitForProjectStatus(t, env, b, "Done", 10)
}

// A repository with no check-run CI still lands on the zero-check path, in one trial.
func TestMergeTrainZeroCheck_NoCIRepoStillLands(t *testing.T) {
	t.Parallel()
	env, a, b := zeroCheckEnv(t, false)
	startTrialSeeder(t, env, func([]int) []gh.CheckRun { return nil }, &simgh.ReadyForReviewStep{})

	RunPoll(t, env)

	WaitForProjectStatus(t, env, a, "Done", 20)
	WaitForProjectStatus(t, env, b, "Done", 10)
	assertLandedOnFirstTrial(t, env, []int{a, b})
}

// delayedVerdictScript seeds verdictFor's check runs on each trial only after delay of real
// time (the Actions start delay); the script runs in the trial-watcher goroutine.
func delayedVerdictScript(env *Env, delay time.Duration, verdictFor func(members []int) []gh.CheckRun) func(pr int, sha string, trial int) {
	return func(pr int, sha string, trial int) {
		var members []int
		if prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo); err == nil {
			for _, p := range prs {
				if p.Number == pr {
					members = parseClosesNumbers(p.Body)
				}
			}
		}
		env.Sim.Sim().SeedReadyForReviewSteps(env.OwnerRepo, pr, simgh.ReadyForReviewStep{SHA: sha})
		time.Sleep(delay)
		for _, cr := range verdictFor(members) {
			env.Sim.Sim().SeedCheckRun(env.OwnerRepo, sha, cr)
		}
	}
}

// Once the real CI runs and passes, the held trial lands without operator action.
func TestMergeTrainZeroCheck_DelayedCIPasses_Lands(t *testing.T) {
	t.Parallel()
	env, a, b := zeroCheckEnv(t, true)
	startTrialScript(t, env, delayedVerdictScript(env, 300*time.Millisecond, allGreenVerdict))

	RunPoll(t, env)

	WaitForProjectStatus(t, env, a, "Done", 20)
	WaitForProjectStatus(t, env, b, "Done", 10)
}

// Once the real CI runs and fails, the trial is red and goes through bisection: the poison
// member is ejected and never lands.
func TestMergeTrainZeroCheck_DelayedCIFails_BisectedPoisonerEjected(t *testing.T) {
	t.Parallel()
	env, a, b := zeroCheckEnv(t, true)
	startTrialScript(t, env, delayedVerdictScript(env, 200*time.Millisecond, poisonVerdict(a)))

	for i := 0; i < 6; i++ {
		RunPoll(t, env)
		if projectItem(t, env, b).Status == "Done" {
			break
		}
	}

	if st := projectItem(t, env, b).Status; st != "Done" {
		t.Errorf("clean member #%d status = %q, want Done after bisection", b, st)
	}
	if projectItem(t, env, a).Status == "Done" {
		t.Errorf("poison member #%d landed", a)
	}
	if n := draftPRCount(env); n < 2 {
		t.Errorf("trial PRs opened = %d, want >= 2 (the red trial was bisected)", n)
	}
}

// The landing settle's own hold (settleAndMerge re-judges the landing PR's head) cannot be
// isolated in the sim: the landing PR is the trial PR, so a trial verdict that went green
// already had check runs on that head. It is pinned by the engine unit tests
// TestSettleAndMerge_ZeroChecks_* (held, neutralised and no-CI variants).
