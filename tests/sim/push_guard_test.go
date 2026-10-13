package sim

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2089 sim scenarios: the zero-ahead push guard against the #2058 stale-worktree
// shape. Implement opens a draft PR; Review then runs against a worktree whose
// local issue branch has been left at the base tip, while someone has pushed a
// commit to origin/fabrik/issue-N out of band and the shared bare clone's
// tracking refs have been refreshed (so the bare --force-with-lease would pass).
// Assertions are scoped to engine decisions and the remote ref, not GitHub
// no-diff semantics — see tests/sim/README.md.

func pushGuardStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// staleWorktreeReview returns a Review script that reproduces the #2058 shape
// and records the SHA of the out-of-band commit.
func staleWorktreeReview(env *Env, humanSHA *string) simclaude.Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		branch := fmt.Sprintf("fabrik/issue-%d", issue.Number)
		git := func(args ...string) error {
			out, err := exec.Command("git", append([]string{"-C", workDir}, args...)...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), out, err)
			}
			return nil
		}
		// The out-of-band push: a human commit lands on the remote issue branch.
		env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, branch,
			map[string]string{"HUMAN.md": "conflict resolution\n"}, "human: resolve conflict")
		sha, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
		if err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		*humanSHA = sha
		// Any full fetch refreshes the shared tracking refs, defeating the lease.
		if err := git("fetch", "origin"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		// The stale local branch ends up at the base tip (no commits of its own).
		if err := git("reset", "--hard", "origin/main"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 15}, nil
	}
}

func TestPushGuard_StaleZeroAheadWorktreeIsRefusedAndPaused(t *testing.T) {
	t.Parallel()
	var humanSHA string
	env := NewEnv(t, EnvOptions{Stages: pushGuardStages(), Yolo: boolPtr(true), StartTime: time.Now()})
	env.Claude.ForStage("Review", staleWorktreeReview(env, &humanSHA))
	num := FileIssue(t, env, "stale worktree", "Reproduce the #2058 stale-worktree shape.", "Implement")

	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	if humanSHA == "" {
		t.Fatal("Review script never ran")
	}
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if head != humanSHA {
		t.Errorf("remote %s moved: got %s, want the human's %s", branch, head, humanSHA)
	}
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("want fabrik:awaiting-input, labels=%v", labels)
	}
	if hasLabel(labels, "stage:Review:complete") {
		t.Errorf("a refused push must not complete the stage, labels=%v", labels)
	}
	if st := projectItem(t, env, num).Status; st != "Review" {
		t.Errorf("item must not advance past Review, status=%q", st)
	}
	prs, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || prs == nil || prs.State != "open" {
		t.Errorf("PR must stay open, got %+v err=%v", prs, err)
	}
	guardComments := 0
	for _, c := range projectItem(t, env, num).Comments {
		if strings.Contains(c.Body, "push refused: local branch has no commits") {
			guardComments++
			for _, want := range []string{"carries no commits", "left untouched", fmt.Sprintf("PR #%d", prs.Number)} {
				if !strings.Contains(c.Body, want) {
					t.Errorf("guard comment missing %q: %s", want, c.Body)
				}
			}
		}
	}
	if guardComments != 1 {
		t.Errorf("want exactly 1 guard comment, got %d", guardComments)
	}
}

// Non-vacuity twin: with the guard neutralised the same scenario overwrites the
// human's commit on the remote.
func TestPushGuard_StaleZeroAheadWorktree_GuardDisabledOverwrites(t *testing.T) {
	t.Parallel()
	var humanSHA string
	env := NewEnv(t, EnvOptions{Stages: pushGuardStages(), Yolo: boolPtr(true), StartTime: time.Now()})
	env.Engine.SetPushZeroAheadGuardDisabledForTest(true)
	env.Claude.ForStage("Review", staleWorktreeReview(env, &humanSHA))
	num := FileIssue(t, env, "stale worktree", "Reproduce the #2058 stale-worktree shape.", "Implement")

	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if head == humanSHA {
		t.Fatalf("expected the stale zero-ahead branch to overwrite the human commit when the guard is off")
	}
}
