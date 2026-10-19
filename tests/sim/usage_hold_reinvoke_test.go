package sim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// TestUsageHold_RefusedReviewReinvokeRunsAfterTheLift is the #2407 twin of the
// #2350 stranding. A review reinvoke that hits the Claude usage limit starts an
// account-wide hold; while it lasts no further reinvoke worker is started; once it
// lifts, the still-unaddressed review is re-invoked without any outside nudge.
//
// Neutralised (SetUsageHoldReinvokeGateDisabledForTest) the item is stranded: the
// refused Phase-1 reinvoke marked it advanced, its admitting cooldown was consumed
// and nothing else re-admits it, so no review pass ever runs after the lift.
func TestUsageHold_RefusedReviewReinvokeRunsAfterTheLift(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		neutralised bool
	}{
		{"fix", false},
		{"fix neutralised (non-vacuity)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := reinvokeCycleCountersEnv(t, 5, 20)
			env.Engine.SetUsageHoldReinvokeGateDisabledForTest(tc.neutralised)

			deadline := env.Clock.Now().Add(time.Hour)
			limited := true
			env.Claude.ForStageComments("Review", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
				if limited {
					limited = false
					return "", false, engine.TokenUsage{}, &claudeerr.UsageLimitError{Message: `terminal_reason="blocking_limit"`, ResetAt: deadline}
				}
				return simclaude.NoOpCommentReview()(ctx, stage, issue, comments, workDir, opts)
			})

			issueNum, prNum := seedReviewGateItem(t, env, "Review", "2407")
			preCreateWorktree(t, env, issueNum)
			seedActionableReview(t, env, prNum, "handarbeit-pruefer", "COMMENTED",
				fmt.Sprintf("**File:** engine/foo.go\nmissing nil check (%s).", tc.name))

			// The first pass reaches Claude and hits the limit: the hold starts.
			AdvanceUntil(t, env, func(env *Env) bool { return env.Claude.CommentCallCount("Review") == 1 }, 20)

			// During the hold nothing reaches Claude, whatever the build.
			for env.Clock.Now().Before(deadline.Add(-5 * time.Minute)) {
				env.Clock.Advance(3 * time.Minute)
				RunPoll(t, env)
			}
			if got := env.Claude.CommentCallCount("Review"); got != 1 {
				t.Fatalf("%d review passes during the hold, want 1 (the one that hit the limit)", got)
			}

			// Past the lift: the fix re-admits the item by its own cooldown.
			env.Clock.Advance(10 * time.Minute)
			ran := false
			for i := 0; i < 15 && !ran; i++ {
				env.Clock.Advance(time.Minute)
				RunPoll(t, env)
				ran = env.Claude.CommentCallCount("Review") > 1
			}
			if tc.neutralised {
				if ran {
					t.Fatalf("neutralised: the review pass ran after the lift; the scenario does not reproduce the #2350 stranding")
				}
				return
			}
			if !ran {
				t.Fatalf("no review pass within 15 polls of the hold lifting: the item is stranded (#2407)\n\n%s", diagnostics(env))
			}
		})
	}
}
