package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// toolsDeniedBound is MaxToolsDeniedRetries for the #2301 scenarios.
const toolsDeniedBound = 3

// runToolsDeniedComment seeds a Specify item whose comment review is denied by
// script, then polls until the item is paused or the polls run out.
func runToolsDeniedComment(t *testing.T, script simclaude.CommentScript, disableClassification bool) (env *Env, num int) {
	t.Helper()
	env = NewEnv(t, EnvOptions{Stages: smokeStages(), ConfigureCfg: func(c *engine.Config) { c.MaxToolsDeniedRetries = toolsDeniedBound }})
	env.Engine.SetUngrantableDenialDisabledForTest(disableClassification)
	env.Claude.ForStageComments("Specify", script)

	num = seedCompletedSpecify(t, env, "denied comment review")
	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "please update the skill")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	for i := 0; i < 40 && !hasLabel(IssueLabels(t, env, num), "fabrik:paused"); i++ {
		RunPolls(t, env, 1)
		env.Clock.Advance(2 * time.Minute)
	}
	return env, num
}

// TestUngrantableToolDenialPausesOnFirstInvocation is the #2301 acceptance
// scenario: a comment review whose Edit on .claude/skills/x/SKILL.md is denied
// (a path inside the worktree that Fabrik's own allow rule already covers) pauses
// the item after exactly one invocation, names the path, and dispatches no second
// invocation. With the classification neutralised the same scenario takes the
// bounded retries first (non-vacuity).
func TestUngrantableToolDenialPausesOnFirstInvocation(t *testing.T) {
	t.Parallel()
	const rel = ".claude/skills/x/SKILL.md"

	t.Run("first occurrence", func(t *testing.T) {
		t.Parallel()
		env, num := runToolsDeniedComment(t, simclaude.ToolsDeniedEditComment(rel), false)
		if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") || !hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-input") {
			t.Fatalf("item not paused; labels=%v", IssueLabels(t, env, num))
		}
		if got := env.Claude.CommentCallCount("Specify"); got != 1 {
			t.Errorf("comment review ran %d time(s), want exactly 1", got)
		}
		var body string
		for _, c := range commentsOn(t, env, num) {
			if strings.Contains(c.Body, "cannot be granted") {
				body = c.Body
			}
		}
		if body == "" || !strings.Contains(body, "`"+rel+"`") || !strings.Contains(body, "fabrik:unrestricted") {
			t.Errorf("pause comment does not name %s and list the options: %q", rel, body)
		}

		// More polls change nothing: no second invocation is dispatched.
		RunPolls(t, env, 6)
		if got := env.Claude.CommentCallCount("Specify"); got != 1 {
			t.Errorf("a second invocation was dispatched (%d calls)", got)
		}
	})

	t.Run("classification neutralised (non-vacuity)", func(t *testing.T) {
		t.Parallel()
		env, num := runToolsDeniedComment(t, simclaude.ToolsDeniedEditComment(rel), true)
		if got := env.Claude.CommentCallCount("Specify"); got != toolsDeniedBound {
			t.Errorf("with the classification disabled the comment review ran %d time(s), want the bounded %d", got, toolsDeniedBound)
		}
		if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
			t.Errorf("the bounded path must still pause at the bound; labels=%v", IssueLabels(t, env, num))
		}
	})
}

// TestBashToolDenialKeepsBoundedRetries is the control: a Bash denial is never
// ungrantable and still gets MaxToolsDeniedRetries invocations before the pause.
func TestBashToolDenialKeepsBoundedRetries(t *testing.T) {
	t.Parallel()
	env, num := runToolsDeniedComment(t, simclaude.ToolsDeniedBashComment("git push origin HEAD"), false)
	if got := env.Claude.CommentCallCount("Specify"); got != toolsDeniedBound {
		t.Errorf("comment review ran %d time(s), want the bounded %d", got, toolsDeniedBound)
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Errorf("expected the bounded pause; labels=%v", IssueLabels(t, env, num))
	}
	if n := countCommentsContaining(t, env, num, "cannot be granted"); n != 0 {
		t.Errorf("a Bash denial must not use the ungrantable wording (%d comment(s))", n)
	}
}
