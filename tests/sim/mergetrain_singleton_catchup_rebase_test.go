package sim

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of #2150 (ADR-2150): a comment pass that commits nothing used to leave the
// member's worktree on an unpushed engine rebase, which made the singleton catch-up
// (#2044) defer ("local member branch is ahead of or diverged") and cost a whole trial.
//
// Flow: a cruise item reaches Validate-complete with an open PR, main advances, a human
// comment triggers a Validate comment pass whose script commits nothing (so the engine's
// pre-invocation rebase moves the worktree's HEAD and nothing pushes it), then the item
// is approved into the merge train as a lone Queued member now behind main.
//
// Neutralisation (each scenario fails with the named branch removed):
//   - RestoredAfterNoCommitPass* : remove the RestoreUnpushedRebase call in
//     processCommentsClassified (the worktree is left on the rebased head).
//   - PureRebaseBackstop*        : remove the pure-rebase branch in PrepareCatchUp (the
//     member defers to a trial: draftPRCount >= 1, MergePR instead of MergePRAtHeadSHA).
//   - BothNeutralised*           : documents that without either fix the trial is built.

func rebaseScenarioEnv(t *testing.T) *Env {
	t.Helper()
	opts := catchUpEnvOptions()
	opts.ConfigureCfg = func(c *engine.Config) { c.User = "simgh-bot"; c.Yolo = false }
	return mergeTrainEnv(t, opts)
}

func worktreeHead(t *testing.T, env *Env, num int) string {
	t.Helper()
	out, err := exec.Command("git", "-C", env.WM.WorktreeDir(num), "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree HEAD of #%d: %v", num, err)
	}
	return strings.TrimSpace(string(out))
}

// reachValidateThenNoCommitCommentPass drives the item to Validate-complete, advances
// main, runs one human-comment Validate pass that commits nothing, and returns the item,
// its PR number and the remote head before the pass.
func reachValidateThenNoCommitCommentPass(t *testing.T, env *Env) (num, prNum int, remoteHead string) {
	t.Helper()
	env.Claude.ForStageComments("Validate", simclaude.NoOpCommentReview())
	num = reachValidateCompleteUnderCruise(t, env, "rebase then catch-up (#2150)")
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	prNum = pr.Number
	remoteHead, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if got := worktreeHead(t, env, num); got != remoteHead {
		t.Fatalf("fixture: worktree head %s != pushed head %s before the comment pass", got, remoteHead)
	}

	advanceMain(t, env, map[string]string{"base.txt": "base\n"})
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", gateHumanComment)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Validate") >= 1 }, 40)
	// Let the pass finish (its post-invocation handling is what is under test).
	AdvanceUntil(t, env, func(env *Env) bool { return !hasLabel(IssueLabels(t, env, num), "fabrik:editing") }, 20)
	if got, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num)); got != remoteHead {
		t.Fatalf("fixture: the no-commit pass moved the remote branch (%s -> %s)", remoteHead, got)
	}
	return num, prNum, remoteHead
}

// approveAndLand swaps cruise for yolo so the item enters the train as a lone member.
func approveIntoTrain(t *testing.T, env *Env, num int) {
	t.Helper()
	swapCruiseForYolo(t, env, num)
}

func TestMergeTrainSingletonCatchUp_RestoredAfterNoCommitPass_LandsWithoutTrial(t *testing.T) {
	t.Parallel()
	env := rebaseScenarioEnv(t)
	num, prNum, orig := reachValidateThenNoCommitCommentPass(t, env)

	// R1: the engine's rebase was undone — the worktree is the pushed head, not a rebased copy.
	if got := worktreeHead(t, env, num); got != orig {
		t.Fatalf("worktree head = %s after a no-commit pass, want the pushed head %s (unpushed engine rebase left behind)", got, orig)
	}

	startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, false, false, []gh.CheckRun{greenCheckRun("")})
	draftsBefore := draftPRCount(env)
	approveIntoTrain(t, env, num)
	WaitForProjectStatus(t, env, num, "Done", 60)
	assertLandedViaCatchUp(t, env, num, prNum, orig, draftsBefore)
}

func TestMergeTrainSingletonCatchUp_PureRebaseBackstop_LandsWithoutTrial(t *testing.T) {
	t.Parallel()
	env := rebaseScenarioEnv(t)
	env.Engine.SetCommentRebaseRestoreDisabledForTest(true)
	num, prNum, orig := reachValidateThenNoCommitCommentPass(t, env)

	// With R1 neutralised the worktree holds the unpushed rebase — the pre-fix state.
	if got := worktreeHead(t, env, num); got == orig {
		t.Fatalf("worktree head == pushed head with the restore neutralised; the scenario no longer reproduces #2150")
	}

	startCatchUpCISeeder(t, env, memberBranch(num), prNum, orig, false, false, []gh.CheckRun{greenCheckRun("")})
	draftsBefore := draftPRCount(env)
	approveIntoTrain(t, env, num)
	WaitForProjectStatus(t, env, num, "Done", 60)
	assertLandedViaCatchUp(t, env, num, prNum, orig, draftsBefore)
}

func TestMergeTrainSingletonCatchUp_BothNeutralised_BuildsTrial(t *testing.T) {
	t.Parallel()
	env := rebaseScenarioEnv(t)
	env.Engine.SetCommentRebaseRestoreDisabledForTest(true)
	env.Engine.SetCatchUpPureRebaseDisabledForTest(true)
	num, _, orig := reachValidateThenNoCommitCommentPass(t, env)
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	draftsBefore := draftPRCount(env)
	approveIntoTrain(t, env, num)
	WaitForProjectStatus(t, env, num, "Done", 60)

	if got := draftPRCount(env); got <= draftsBefore {
		t.Errorf("without R1 and R2 the diverged member must fall back to a trial, got %d draft PR(s) (%d before approval)", got, draftsBefore)
	}
	if head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num)); head != orig {
		t.Error("the member's own branch must not be touched when the catch-up defers")
	}
}

// assertLandedViaCatchUp checks the landing took catch-up plus the singleton fast path.
// draftPRsBefore is the CreateDraftPR count before the member was approved into the
// train: the Implement stage opens the member's own draft PR, so only a rise past it is
// a trial PR.
func assertLandedViaCatchUp(t *testing.T, env *Env, num, prNum int, orig string, draftPRsBefore int) {
	t.Helper()
	if got := draftPRCount(env); got != draftPRsBefore {
		t.Errorf("expected no draft CI PR (no trial): %d before approval, %d after", draftPRsBefore, got)
	}
	newHead, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if newHead == orig {
		t.Fatal("the member branch was not caught up")
	}
	merges := env.Sim.Log().ByMethod("MergePRAtHeadSHA")
	if len(merges) != 1 || merges[0].Args.Number != prNum || len(merges[0].Args.Values) != 1 || merges[0].Args.Values[0] != newHead {
		t.Fatalf("want exactly one MergePRAtHeadSHA on PR #%d pinned to the caught-up head %s, got %+v", prNum, newHead, merges)
	}
	// The catch-up merge descends from the remote head CI validated, never from a local rebase.
	parents, err := exec.Command("git", "-C", env.WM.WorktreeDir(num), "rev-list", "--parents", "-n", "1", newHead).Output()
	if err == nil && !strings.Contains(string(parents), orig) {
		t.Errorf("caught-up head %s does not descend from the validated remote head %s: %s", newHead, orig, parents)
	}
}
