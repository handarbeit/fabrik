package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// This file is the restart coverage for the Claude usage-limit hold (#2349, ADR 2349).
// The hold used to be memory only: after a restart the label sweep concluded nothing was
// limited, stripped fabrik:claude-limit from every item, every slot dispatched into the
// limit and each item got its explanatory comment again.

func usageLimitComments(t *testing.T, env *Env, num int) int {
	t.Helper()
	n := 0
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, "Claude usage limit hit") {
			n++
		}
	}
	return n
}

// TestUsageLimitHold_SurvivesRestart: a restart during the hold dispatches nothing into the
// limit, keeps the label and posts no duplicate comment; once the sim clock passes the
// deadline the hold lifts and work resumes.
//
// The exit carries an exact structured reset 5h out, well beyond the 1h fallback that
// label-based seeding would use. That is what makes the test prove the persisted file: with
// persistence disabled the restart re-seeds a hold of at most one hour from the surviving
// label, which has lifted by the 2h mark and fails the "still held" assertions below.
func TestUsageLimitHold_SurvivesRestart(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	EnableDecisionState(t, env)

	deadline := env.Clock.Now().Add(5 * time.Hour)
	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if env.Clock.Now().Before(deadline) {
			return "", false, engine.TokenUsage{}, &claudeerr.UsageLimitError{Message: `terminal_reason="blocking_limit"`, ResetAt: deadline}
		}
		return simclaude.DefaultScript(ctx, stage, issue, newComments, resume, workDir, opts)
	})

	num := FileIssue(t, env, "usage-limit restart", "body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:claude-limit", 80)

	callsBefore := env.Claude.StageCallCount("Specify")
	commentsBefore := usageLimitComments(t, env, num)
	if commentsBefore != 1 {
		t.Fatalf("expected exactly one usage-limit comment before the restart, got %d", commentsBefore)
	}

	env2 := RestartEnv(t, env)
	// Past the 1h fallback, inside the persisted 5h deadline.
	env2.Clock.Advance(2 * time.Hour)
	RunPolls(t, env2, 4)

	if !hasLabel(IssueLabels(t, env2, num), "fabrik:claude-limit") {
		t.Fatal("fabrik:claude-limit was stripped after a restart inside the hold — the sweep must not run while the restored hold is active")
	}
	if got := env2.Claude.StageCallCount("Specify"); got != callsBefore {
		t.Fatalf("Claude was invoked %d time(s) after the restart (was %d) — a restart inside the hold must dispatch nothing into the limit", got, callsBefore)
	}
	if got := usageLimitComments(t, env2, num); got != commentsBefore {
		t.Fatalf("usage-limit comment count went %d -> %d across the restart — no duplicate comment may be posted", commentsBefore, got)
	}

	// The hold ends on the sim clock (R5): past the persisted deadline the label is swept
	// and the stage runs.
	env2.Clock.Advance(4 * time.Hour)
	WaitForLabelAbsent(t, env2, num, "fabrik:claude-limit", 40)
	AdvanceUntil(t, env2, func(e *Env) bool { return e.Claude.StageCallCount("Specify") > callsBefore }, retryCooldownPolls)
}

// TestUsageLimitHold_ExpiredAtStartupSweeps: a persisted hold whose deadline passed while
// the daemon was down behaves as no hold — the sweep clears the labels as it always did.
func TestUsageLimitHold_ExpiredAtStartupSweeps(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	EnableDecisionState(t, env)
	env.Claude.ForStage("Specify", simclaude.UsageLimitExit(), simclaude.DefaultScript)

	num := FileIssue(t, env, "usage-limit expired", "body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:claude-limit", 80)

	env.Clock.Advance(3 * time.Hour) // the daemon is down across the whole hold
	env2 := RestartEnv(t, env)
	WaitForLabelAbsent(t, env2, num, "fabrik:claude-limit", 40)
}

// TestUsageLimitHold_SeededFromLabelsWhenNoFile: state from before the hold was persisted
// (labels, no file) is held conservatively instead of being swept on the first poll.
func TestUsageLimitHold_SeededFromLabelsWhenNoFile(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	EnableDecisionState(t, env)

	// Paused, so it never dispatches: only the sweep can remove its label.
	num := FileIssue(t, env, "usage-limit seeded", "body", "Specify", "fabrik:paused", "fabrik:claude-limit")

	RunPolls(t, env, 3)
	if !hasLabel(IssueLabels(t, env, num), "fabrik:claude-limit") {
		t.Fatal("a labelled item with no persisted hold was swept on startup — the hold must be seeded from the label first")
	}

	env.Clock.Advance(2 * time.Hour)
	WaitForLabelAbsent(t, env, num, "fabrik:claude-limit", 20)
}

// TestUsageLimitHold_OperatorClearBeatsSeeding: an operator's fabrik:clear-claude-limit on
// the first poll after an upgrade (labels, no persisted file) must not be undone by the
// label-based seeding — the hold stays cleared and the sweep strips the label at once.
func TestUsageLimitHold_OperatorClearBeatsSeeding(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	EnableDecisionState(t, env)

	// Paused, so it never dispatches: only the sweep can remove its limit label.
	num := FileIssue(t, env, "usage-limit operator clear", "body", "Specify",
		"fabrik:paused", "fabrik:claude-limit", "fabrik:clear-claude-limit")

	WaitForLabelAbsent(t, env, num, "fabrik:claude-limit", 6)
	if hasLabel(IssueLabels(t, env, num), "fabrik:clear-claude-limit") {
		t.Fatal("the one-shot fabrik:clear-claude-limit command label was not consumed")
	}
}
