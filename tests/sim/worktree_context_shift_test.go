package sim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of #2517 (ADR 2517): an engine rebase that only shifts a commit's diff context
// used to strand the issue branch. The Implement commit edits line 3 of a shared file, main
// later edits line 5 of it, so the engine's pre-pass rebase is clean but the commit's
// patch-id changes. The restore step (#2150) then declined to undo the rebase, and the next
// stage dispatch paused the item as "issue branch diverged from origin" (#2295).
//
// Flow: a cruise item reaches Validate-complete, main shifts the context, a human comment
// triggers a no-commit Validate comment pass (the engine rebases, nothing pushes), then
// fabrik:revalidate dispatches Validate again — the issue-branch sync runs.
//
// Neutralisation (each scenario fails with the named fix removed):
//   - ContentRestore...  : SetCommentRebaseRestoreContentCheckDisabledForTest(false) — the
//     comment pass restores the branch, so the sync finds it equal to the remote.
//   - SyncResets...      : restore content check off — the sync's replay-equivalence rule
//     resets the branch instead.
//   - BothNeutralised... : both off — the pre-fix behaviour, paused as diverged.

const shiftedFileBefore = "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n"

func shiftedFile(l3, l5 string) string {
	return "l1\nl2\n" + l3 + "\nl4\n" + l5 + "\nl6\nl7\nl8\n"
}

// editSharedImplement is an Implement script whose commit edits line 3 of shared.txt.
func editSharedImplement() simclaude.Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if err := os.WriteFile(filepath.Join(workDir, "shared.txt"), []byte(shiftedFile("l3-branch", "l5")), 0o644); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		for _, args := range [][]string{
			{"add", "shared.txt"},
			{"-c", "user.name=simclaude", "-c", "user.email=simclaude@fabrik.test", "commit", "-m", "feat: edit shared line 3"},
		} {
			if out, err := exec.Command("git", append([]string{"-C", workDir}, args...)...).CombinedOutput(); err != nil {
				return "", false, engine.TokenUsage{}, &scriptGitError{out: string(out), err: err}
			}
		}
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 50}, nil
	}
}

type scriptGitError struct {
	out string
	err error
}

func (e *scriptGitError) Error() string { return e.err.Error() + ": " + e.out }

// noCommitValidate completes Validate without committing, so the remote head only moves if
// the engine moves it.
func noCommitValidate() simclaude.Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 50}, nil
	}
}

type shiftOutcome struct {
	num        int
	orig       string
	afterPass  string // worktree head after the no-commit comment pass
	remote     string // remote branch head after the re-dispatch attempt
	validateN  int    // Validate stage invocations after the re-dispatch attempt
	validate0  int    // ... before it
	paused     bool
	divergence int
}

func runContextShiftScenario(t *testing.T, restoreOff, replayOff bool) shiftOutcome {
	t.Helper()
	env := rebaseScenarioEnv(t)
	env.Engine.SetCommentRebaseRestoreContentCheckDisabledForTest(restoreOff)
	env.Engine.SetReplayEquivalenceDisabledForTest(replayOff)
	advanceMain(t, env, map[string]string{"shared.txt": shiftedFileBefore})
	env.Claude.ForStage("Implement", editSharedImplement())
	env.Claude.ForStage("Validate", noCommitValidate())
	env.Claude.ForStageComments("Validate", simclaude.NoOpCommentReview())

	num := reachValidateCompleteUnderCruise(t, env, "context shift (#2517)")
	orig, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	if got := worktreeHead(t, env, num); got != orig {
		t.Fatalf("fixture: worktree head %s != pushed head %s", got, orig)
	}

	advanceMain(t, env, map[string]string{"shared.txt": shiftedFile("l3", "l5-main")})
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", gateHumanComment)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Validate") >= 1 }, 40)
	AdvanceUntil(t, env, func(env *Env) bool { return !hasLabel(IssueLabels(t, env, num), "fabrik:editing") }, 20)

	if got, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num)); got != orig {
		t.Fatalf("the no-commit pass moved the remote branch (%s -> %s); the engine must not push a rebase", orig, got)
	}
	out := shiftOutcome{num: num, orig: orig, afterPass: worktreeHead(t, env, num)}
	out.validate0 = env.Claude.StageCallCount("Validate")

	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:revalidate"); err != nil {
		t.Fatalf("add revalidate: %v", err)
	}
	AdvanceUntil(t, env, func(env *Env) bool {
		labels := IssueLabels(t, env, num)
		return env.Claude.StageCallCount("Validate") > out.validate0 || hasLabel(labels, "fabrik:paused")
	}, 40)

	out.validateN = env.Claude.StageCallCount("Validate")
	out.remote, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(num))
	out.paused = hasLabel(IssueLabels(t, env, num), "fabrik:paused")
	for _, c := range projectItem(t, env, num).Comments {
		if strings.Contains(c.Body, "issue branch diverged from origin") {
			out.divergence++
		}
	}
	return out
}

func requireNotStranded(t *testing.T, o shiftOutcome) {
	t.Helper()
	if o.paused || o.divergence != 0 {
		t.Fatalf("item paused=%v with %d divergence comment(s); want none (#2498 sequence)", o.paused, o.divergence)
	}
	if o.validateN <= o.validate0 {
		t.Fatalf("Validate was not re-dispatched (%d -> %d)", o.validate0, o.validateN)
	}
	// The re-dispatched Validate run rebases and pushes in the ordinary way, so the remote
	// head legitimately moves here; what matters is that nothing pushed or paused before it.
}

func TestContextShift_ContentRestoreUndoesTheEngineRebase(t *testing.T) {
	t.Parallel()
	o := runContextShiftScenario(t, false, false)
	if o.afterPass != o.orig {
		t.Fatalf("worktree head = %s after the no-commit pass, want the pushed head %s", o.afterPass, o.orig)
	}
	requireNotStranded(t, o)
}

func TestContextShift_SyncResetsWhenRestoreIsNeutralised(t *testing.T) {
	t.Parallel()
	o := runContextShiftScenario(t, true, false)
	if o.afterPass == o.orig {
		t.Fatal("restore content check neutralised, but the worktree is at the pushed head; the scenario no longer reproduces #2498")
	}
	requireNotStranded(t, o)
}

func TestContextShift_BothNeutralised_PausesAsDiverged(t *testing.T) {
	t.Parallel()
	o := runContextShiftScenario(t, true, true)
	if o.afterPass == o.orig {
		t.Fatal("the scenario no longer reproduces #2498: the rebase was undone with both fixes off")
	}
	if !o.paused || o.divergence != 1 {
		t.Fatalf("paused=%v divergence comments=%d, want the pre-fix pause with exactly 1 comment", o.paused, o.divergence)
	}
	if o.remote != o.orig {
		t.Errorf("remote head moved %s -> %s", o.orig, o.remote)
	}
}
