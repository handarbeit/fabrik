package sim

import (
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// TestCommentReviewBlockedOnInputPausesAndResumes is the #2280 acceptance
// scenario: a comment-review run that emits FABRIK_BLOCKED_ON_INPUT without
// FABRIK_STAGE_COMPLETE pauses the item exactly as the stage path does (label
// pair plus one comment carrying the question), the comment that triggered it
// stays processed, and only a later human comment resumes the item.
func TestCommentReviewBlockedOnInputPausesAndResumes(t *testing.T) {
	t.Parallel()
	const question = "Should the fix go in the engine or in the skill?"

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
			env.Engine.SetCommentBlockedOnInputDisabledForTest(tc.disabled)
			env.Claude.ForStageComments("Specify", simclaude.CommentReviewBlocked(question), simclaude.CommentReviewNoMarker())

			num := seedCompletedSpecify(t, env, "comment asks a question")
			env.Clock.Advance(time.Minute)
			env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "please tweak the wording")
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedComment: %v", err)
			}

			RunPolls(t, env, 6)
			if got := env.Claude.CommentCallCount("Specify"); got == 0 {
				t.Fatalf("comment-review run never happened; the scenario is vacuous")
			}
			labels := IssueLabels(t, env, num)
			if tc.disabled {
				if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") {
					t.Fatalf("with the handling neutralised the item must not be paused; labels=%v", labels)
				}
				return
			}
			if !hasLabel(labels, "fabrik:paused") || !hasLabel(labels, "fabrik:awaiting-input") {
				t.Fatalf("item not paused by a blocked comment run; labels=%v", labels)
			}
			if n := countCommentsContaining(t, env, num, "awaiting your input"); n != 1 {
				t.Errorf("%d awaiting-input comment(s), want exactly 1", n)
			}
			if n := countCommentsContaining(t, env, num, question); n < 1 {
				t.Errorf("the pause comment does not carry the question %q", question)
			}

			// The comment that triggered the run is processed (R2): more polls change
			// nothing, so the pause is not lifted by re-reading it.
			calls := env.Claude.CommentCallCount("Specify")
			RunPolls(t, env, 6)
			if got := env.Claude.CommentCallCount("Specify"); got != calls {
				t.Errorf("comment path ran again (%d -> %d calls) with no new human comment", calls, got)
			}
			if !hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
				t.Fatalf("pause lifted without a new human comment")
			}

			// A later human comment resumes (#1813 rule unchanged).
			env.Clock.Advance(time.Minute)
			env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "the engine, please")
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedComment: %v", err)
			}
			WaitForLabelAbsent(t, env, num, "fabrik:paused", 40)
			if hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-input") {
				t.Error("fabrik:awaiting-input survived the resume")
			}
			if got := env.Claude.CommentCallCount("Specify"); got <= calls {
				t.Errorf("the resuming comment was never processed (%d calls)", got)
			}
		})
	}
}

// TestCommentReviewWithoutMarkerUnchanged is the control: a comment run that
// emits no blocked marker is not paused.
func TestCommentReviewWithoutMarkerUnchanged(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewNoMarker())

	num := seedCompletedSpecify(t, env, "ordinary comment")
	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "please tweak the wording")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	RunPolls(t, env, 6)
	if env.Claude.CommentCallCount("Specify") == 0 {
		t.Fatalf("comment-review run never happened; the scenario is vacuous")
	}
	labels := IssueLabels(t, env, num)
	if hasLabel(labels, "fabrik:paused") || hasLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("an ordinary comment run paused the item; labels=%v", labels)
	}
	for _, c := range commentsOn(t, env, num) {
		if strings.Contains(c.Body, "awaiting your input") {
			t.Errorf("unexpected awaiting-input comment: %s", c.Body)
		}
	}
}

// TestReviewReinvokeBlockedOnInputPauses covers the review-reinvoke kind of
// #2280: the reinvoke's comment-path run asks a question, the item pauses with
// the label pair and one question comment, and the cycle limit (set to 1, so a
// charged blocked cycle would pause a second time with its own comment) is not
// what paused it.
func TestReviewReinvokeBlockedOnInputPauses(t *testing.T) {
	t.Parallel()
	const question = "Is the reviewer's naming suggestion acceptable?"
	env := reinvokeCycleCountersEnv(t, 1, 20)
	env.Claude.ForStageComments("Review", simclaude.CommentReviewBlocked(question))

	issueNum, prNum := seedReviewGateItem(t, env, "Review", "2280")
	preCreateWorktree(t, env, issueNum)

	driveReinvokeRound(t, env, prNum, 1, 20)
	WaitForIssueLabel(t, env, issueNum, "fabrik:paused", 20)

	labels := IssueLabels(t, env, issueNum)
	if !hasLabel(labels, "fabrik:awaiting-input") {
		t.Fatalf("item paused without fabrik:awaiting-input; labels=%v", labels)
	}
	if n := countCommentsContaining(t, env, issueNum, "awaiting your input"); n != 1 {
		t.Errorf("%d awaiting-input comment(s), want exactly 1", n)
	}
	if n := countCommentsContaining(t, env, issueNum, question); n < 1 {
		t.Errorf("the pause comment does not carry the question")
	}
	if n := countCommentsContaining(t, env, issueNum, noOpCommentBreakerCommentPrefix); n != 0 {
		t.Errorf("the no-op breaker also paused the item (%d comment(s)); a blocked run must bypass it", n)
	}
}
