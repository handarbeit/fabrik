package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
	"github.com/handarbeit/fabrik/tests/sim/simgh/ghfault"
)

// seedCompletedSpecify files an issue whose Specify stage has already run
// (stage:Specify:complete), so a human comment routes to the comment-review
// path rather than re-running the stage (#2220).
func seedCompletedSpecify(t *testing.T, env *Env, title string, labels ...string) int {
	t.Helper()
	num := FileIssue(t, env, title, "body", "Specify")
	for _, l := range append([]string{"stage:Specify:complete"}, labels...) {
		if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("AddLabelToIssue(%s): %v", l, err)
		}
	}
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return num
}

// TestBareAwaitingInputClearedByCompletedCommentRun is the #1970 route-A
// reproduction: the operator follows the pause message and removes only
// fabrik:paused, leaving a bare fabrik:awaiting-input. A comment-review run that
// does the work but never emits FABRIK_STAGE_COMPLETE must not leave it behind.
func TestBareAwaitingInputClearedByCompletedCommentRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		disabled bool
	}{
		{"fix", false},
		{"fix neutralised (non-vacuity)", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := NewEnv(t, EnvOptions{Stages: smokeStages()})
			env.Engine.SetOrphanAwaitingInputClearDisabledForTest(tc.disabled)
			env.Claude.ForStageComments("Specify", simclaude.CommentReviewNoMarker())

			num := seedCompletedSpecify(t, env, "bare awaiting-input", "fabrik:awaiting-input")
			env.Clock.Advance(time.Minute)
			env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "please tweak the wording")
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedComment: %v", err)
			}

			RunPolls(t, env, 8)
			if got := env.Claude.CommentCallCount("Specify"); got == 0 {
				t.Fatalf("comment-review run never happened; the scenario is vacuous")
			}
			labels := IssueLabels(t, env, num)
			if tc.disabled {
				if !hasLabel(labels, "fabrik:awaiting-input") {
					t.Fatalf("with the fix neutralised the stale label should survive; labels=%v", labels)
				}
				return
			}
			if hasLabel(labels, "fabrik:awaiting-input") {
				t.Errorf("stale fabrik:awaiting-input survived a completed comment-review run; labels=%v", labels)
			}
		})
	}
}

// TestRealPauseNotLiftedByOrphanRule guards ADR-069: with both labels present
// and no post-pause human comment, nothing is stripped.
func TestRealPauseNotLiftedByOrphanRule(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewNoMarker())
	num := seedCompletedSpecify(t, env, "real pause", "fabrik:paused", "fabrik:awaiting-input")

	RunPolls(t, env, 6)
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") || !hasLabel(labels, "fabrik:awaiting-input") {
		t.Fatalf("a real pause was lifted; labels=%v", labels)
	}
}

// TestFailedPauseRemovalAtResumeRetriesNextPoll is the #1970 route-B
// reproduction: when removing fabrik:paused fails during the resume, the
// comment must stay unprocessed so the next poll resumes, instead of being
// marked processed with the item left paused and nothing to resume it.
func TestFailedPauseRemovalAtResumeRetriesNextPoll(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewNoMarker())

	num := seedCompletedSpecify(t, env, "failed resume removal", "fabrik:paused", "fabrik:awaiting-input")
	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "ok, continue")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	env.Sim.Faults().FailWhen("RemoveLabelFromIssue", func(a simgh.Args) bool {
		return a.Label == "fabrik:paused"
	}, 1, ghfault.ServerError())

	RunPolls(t, env, 1)
	if env.Sim.Faults().FiredCount("RemoveLabelFromIssue") != 1 {
		t.Fatalf("fault did not fire; the scenario is vacuous")
	}
	// The fault fired, so the pause could not be lifted: the comment must not
	// have been processed, whatever the labels look like now.
	if got := env.Claude.CommentCallCount("Specify"); got != 0 {
		t.Fatalf("comment processed (%d calls) although the pause could not be lifted", got)
	}

	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-input", 80)
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Error("fabrik:paused should be removed once the removal succeeds on retry")
	}
	if got := env.Claude.CommentCallCount("Specify"); got == 0 {
		t.Error("the resume comment was never processed after the retry")
	}
}
