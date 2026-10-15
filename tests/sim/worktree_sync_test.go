package sim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twins for #2295 (ADR 2295): the issue-branch sync and the verified-lease push.
// Assertions are scoped to engine decisions and the remote ref, not GitHub's no-diff PR
// closing — see tests/sim/README.md. Mentioned for context: #2058, #2089.

func syncGit(workDir string, args ...string) error {
	out, err := exec.Command("git", append([]string{"-C", workDir}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), out, err)
	}
	return nil
}

// divergedReview reproduces the #2058 shape with local work: a person pushes a commit to
// the remote issue branch, the shared tracking refs are refreshed, and the worktree
// meanwhile commits its own, different change.
func divergedReview(env *Env, humanSHA *string) simclaude.Script {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		branch := fmt.Sprintf("fabrik/issue-%d", issue.Number)
		env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, branch,
			map[string]string{"HUMAN.md": "conflict resolution\n"}, "human: resolve conflict")
		sha, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
		if err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		*humanSHA = sha
		if err := syncGit(workDir, "fetch", "origin"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		if err := os.WriteFile(filepath.Join(workDir, "LOCAL.md"), []byte("local work\n"), 0o644); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		if err := syncGit(workDir, "add", "LOCAL.md"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		if err := syncGit(workDir, "commit", "-m", "review: local change"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 15}, nil
	}
}

func TestWorktreeSync_DivergedBranchIsRefusedAndPausedRemoteUntouched(t *testing.T) {
	t.Parallel()
	var humanSHA string
	env := NewEnv(t, EnvOptions{Stages: pushGuardStages(), Yolo: boolPtr(true), StartTime: time.Now()})
	env.Claude.ForStage("Review", divergedReview(env, &humanSHA))
	num := FileIssue(t, env, "diverged branch", "The #2058 shape where the stale branch carries its own commit.", "Implement")

	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if humanSHA == "" || head != humanSHA {
		t.Errorf("remote %s must be byte-identical to the human's commit: got %s, want %s", branch, head, humanSHA)
	}
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:awaiting-input") || hasLabel(labels, "stage:Review:complete") {
		t.Errorf("want paused awaiting-input without Review complete, labels=%v", labels)
	}
	comments := 0
	for _, c := range projectItem(t, env, num).Comments {
		if strings.Contains(c.Body, "issue branch diverged from origin") {
			comments++
			for _, want := range []string{"left untouched", humanSHA} {
				if !strings.Contains(c.Body, want) {
					t.Errorf("divergence comment missing %q: %s", want, c.Body)
				}
			}
		}
	}
	if comments != 1 {
		t.Errorf("want exactly 1 divergence comment, got %d", comments)
	}
}

// Non-vacuity twin: with the bare lease restored (and the #2089 guard irrelevant — the
// branch is ahead of base) the same scenario overwrites the person's commit.
func TestWorktreeSync_BareLeaseOverwritesDivergedRemote(t *testing.T) {
	t.Parallel()
	var humanSHA string
	env := NewEnv(t, EnvOptions{Stages: pushGuardStages(), Yolo: boolPtr(true), StartTime: time.Now()})
	env.Engine.SetBareLeasePushForTest(true)
	env.Claude.ForStage("Review", divergedReview(env, &humanSHA))
	num := FileIssue(t, env, "diverged branch bare lease", "The #2058 shape where the stale branch carries its own commit.", "Implement")

	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)

	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, fmt.Sprintf("fabrik/issue-%d", num))
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if head == humanSHA {
		t.Fatalf("expected the bare lease to overwrite the human commit")
	}
}

// An out-of-band commit lands on the remote while Review's first run is in progress and
// that run ends incomplete. The retry's worktree setup fast-forwards the stale branch to
// it, and the commit survives the stage's push.
func outOfBandThenRetry(env *Env, humanSHA *string, sawHuman *bool) []simclaude.Script {
	first := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		branch := fmt.Sprintf("fabrik/issue-%d", issue.Number)
		env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, branch,
			map[string]string{"HUMAN.md": "conflict resolution\n"}, "human: resolve conflict")
		sha, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, branch)
		if err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		*humanSHA = sha
		return "still working\n", false, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 15}, nil
	}
	second := func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, nc []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		*sawHuman = syncGit(workDir, "merge-base", "--is-ancestor", *humanSHA, "HEAD") == nil
		if err := os.WriteFile(filepath.Join(workDir, "REVIEW.md"), []byte("review fix\n"), 0o644); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		if err := syncGit(workDir, "add", "REVIEW.md"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		if err := syncGit(workDir, "commit", "-m", "review: fix"); err != nil {
			return "", false, engine.TokenUsage{}, err
		}
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 20, TurnsUsed: 1, MaxTurns: 15}, nil
	}
	return []simclaude.Script{first, second}
}

func runOutOfBandThenRetry(t *testing.T, disableSync bool) (humanSHA string, sawHuman bool, head string) {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages: pushGuardStages(), Yolo: boolPtr(true), StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) { cfg.RetryBackoff = 20 * time.Millisecond },
	})
	if disableSync {
		env.Engine.SetBranchSyncDisabledForTest(true)
	}
	env.Claude.ForStage("Review", outOfBandThenRetry(env, &humanSHA, &sawHuman)...)
	num := FileIssue(t, env, "out of band commit", "A person pushes while the stage runs.", "Implement")
	for poll := 0; poll < 100; poll++ {
		RunPoll(t, env)
		if hasLabel(IssueLabels(t, env, num), "stage:Review:complete") || env.Claude.StageCallCount("Review") >= 2 && disableSync {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	head, err := env.Sim.Sim().HeadSHA(env.OwnerRepo, fmt.Sprintf("fabrik/issue-%d", num))
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	return humanSHA, sawHuman, head
}

func TestWorktreeSync_OutOfBandCommitOnBehindBranchSurvivesStageRun(t *testing.T) {
	t.Parallel()
	humanSHA, sawHuman, head := runOutOfBandThenRetry(t, false)
	if humanSHA == "" || !sawHuman {
		t.Fatalf("the retry's worktree did not contain the out-of-band commit (human=%q)", humanSHA)
	}
	if head == humanSHA {
		t.Fatalf("expected the stage's own commit on top of the human's")
	}
}

// Non-vacuity twin: with the sync neutralised the retry runs on the stale branch.
func TestWorktreeSync_NonVacuous_DisabledSyncRunsOnStaleBranch(t *testing.T) {
	t.Parallel()
	humanSHA, sawHuman, _ := runOutOfBandThenRetry(t, true)
	if humanSHA != "" && sawHuman {
		t.Fatalf("the stale branch contained the out-of-band commit with the sync disabled")
	}
}
