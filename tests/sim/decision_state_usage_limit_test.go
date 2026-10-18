package sim

import (
	"strings"
	"testing"
	"time"

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
func TestUsageLimitHold_SurvivesRestart(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: failureShapeStages()})
	EnableDecisionState(t, env)
	env.Claude.ForStage("Specify", simclaude.UsageLimitExit(), simclaude.DefaultScript)

	num := FileIssue(t, env, "usage-limit restart", "body", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:claude-limit", 80)

	callsBefore := env.Claude.StageCallCount("Specify")
	commentsBefore := usageLimitComments(t, env, num)
	if commentsBefore != 1 {
		t.Fatalf("expected exactly one usage-limit comment before the restart, got %d", commentsBefore)
	}

	env2 := RestartEnv(t, env)
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

	// The hold ends on the sim clock (R5): past the fallback hour the label is swept and
	// the stage runs.
	env2.Clock.Advance(2 * time.Hour)
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
