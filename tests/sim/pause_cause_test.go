package sim

import (
	"context"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Sim twin of #2487 (ADR 2487): an engine pause states its cause on the issue as
// a fabrik:pause:<cause> label, written with fabrik:paused and removed with it,
// while fabrik:awaiting-input and the resume rule are unchanged. The other causes
// are pinned where their pause is already driven (TestRebaseCycleLimit:
// cycle-limit) and exhaustively in engine/pause_cause_test.go.

func blockedOnInputScript(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
	return "FABRIK_BLOCKED_ON_INPUT\n", false, engine.TokenUsage{}, nil
}

func causeLabels(labels []string) []string {
	var out []string
	for _, l := range labels {
		if len(l) > len("fabrik:pause:") && l[:len("fabrik:pause:")] == "fabrik:pause:" {
			out = append(out, l)
		}
	}
	return out
}

// A worker's question: paused + awaiting-input + the question cause; a human
// comment after the pause resumes it exactly as before and the cause goes with it.
func TestPauseCause_QuestionIsLabelledAndLiftedWithThePause(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStage("Specify", blockedOnInputScript)
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewCompleted())

	num := FileIssue(t, env, "pause cause: question (#2487)", "body", "Specify")

	WaitForIssueLabel(t, env, num, "fabrik:pause:question", 80)
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") || !hasLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("the pause pair must be unchanged beside the cause: %v", labels)
	}
	if got := causeLabels(labels); len(got) != 1 {
		t.Errorf("want exactly one cause label, got %v", got)
	}

	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "proceed")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	WaitForLabelAbsent(t, env, num, "fabrik:paused", 80)
	WaitForLabelAbsent(t, env, num, "fabrik:pause:question", 40)
}

// The dominant way a pause ends is a human removing fabrik:paused in the UI,
// which no engine path sees: the stale-cause scan removes the leftover cause.
func TestPauseCause_StaleCauseAfterAHumanRemovesThePauseIsCleaned(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStage("Specify", blockedOnInputScript)

	num := FileIssue(t, env, "pause cause: stale (#2487)", "body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:pause:question", 80)

	// A human removes only fabrik:paused in the UI.
	if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, "fabrik:paused"); err != nil {
		t.Fatal(err)
	}
	WaitForLabelAbsent(t, env, num, "fabrik:pause:question", 80)
}

// A cause label next to a live pause is kept: the scan only strips a cause whose
// fabrik:paused is gone.
func TestPauseCause_CauseBesideALivePauseIsKept(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStage("Specify", blockedOnInputScript)

	num := FileIssue(t, env, "pause cause: kept (#2487)", "body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:pause:question", 80)
	for i := 0; i < 10; i++ {
		RunPoll(t, env)
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:pause:question") {
		t.Error("the cause label was removed while the pause is still in force")
	}
}
