package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

func skillLoadStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1, Skill: "fabrik-stages:specify", CommentSkill: "fabrik-stages:specify-comment"},
		{Name: "Done", Order: 2, CleanupWorktree: true},
	}
}

// TestSkillLoadFailed_StagePausesForAHuman is #2138 / FR-001, FR-003, FR-004 at
// the engine level: a worker whose own skill failed to load never completes the
// stage. The item is paused for a human (not retried in a loop), carries no
// failed label, advances nowhere, and gets exactly one explanatory comment.
func TestSkillLoadFailed_StagePausesForAHuman(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: skillLoadStages()})
	env.Claude.ForStage("Specify", simclaude.SkillLoadFailed("fabrik-stages:specify"), simclaude.DefaultScript)

	num := FileIssue(t, env, "Skill load failure", "body", "Specify")

	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 10)
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "stage:Specify:complete") {
		t.Error("a skill-load failure must never complete the stage")
	}
	if hasLabel(labels, "stage:Specify:failed") {
		t.Error("a skill-load failure is a configuration fault: must not apply stage:Specify:failed")
	}
	if n := countCommentsContaining(t, env, num, "skill failed to load"); n != 1 {
		t.Errorf("%d skill-load comment(s), want exactly 1", n)
	}
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, "skill failed to load") {
			if !strings.Contains(c.Body, "fabrik-stages:specify") || !strings.Contains(c.Body, "Plugins loaded") || !strings.Contains(c.Body, "Unknown skill") {
				t.Errorf("comment must name the skill, the plugins loaded and the error, got:\n%s", c.Body)
			}
		}
	}

	// Paused means held, not looped: no further worker is spawned.
	RunPolls(t, env, 30)
	if got := env.Claude.StageCallCount("Specify"); got != 1 {
		t.Errorf("StageCallCount(Specify) = %d, want 1 — a paused item must not be re-dispatched", got)
	}

	// Lifting the pause by hand resumes the stage; a healthy run completes it.
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("RemoveLabelFromIssue(%s): %v", l, err)
		}
	}
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", retryCooldownPolls)
}

// TestSkillLoadFailed_CommentPathPausesWithoutBreaker covers the comment-review
// path: the same pause, with no comment-breaker involvement.
func TestSkillLoadFailed_CommentPathPausesWithoutBreaker(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: skillLoadStages()})
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		return "FABRIK_BLOCKED_ON_INPUT\n", false, engine.TokenUsage{}, nil
	})
	env.Claude.ForStageComments("Specify", simclaude.SkillLoadFailedComment("fabrik-stages:specify-comment"))

	num := FileIssue(t, env, "Skill load failure on comment", "body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 80)

	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "Here is the answer.")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	AdvanceUntil(t, env, func(env *Env) bool {
		return countCommentsContaining(t, env, num, "skill failed to load") >= 1
	}, 120)
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") {
		t.Errorf("comment-path skill-load failure must pause the item, labels=%v", labels)
	}
	if hasLabel(labels, "stage:Specify:complete") || hasLabel(labels, "stage:Specify:failed") {
		t.Errorf("must neither complete nor fail the stage, labels=%v", labels)
	}
	if n := countCommentsContaining(t, env, num, "comment-processing circuit breaker"); n != 0 {
		t.Errorf("the comment breaker must not be involved, got %d breaker comment(s)", n)
	}
}
