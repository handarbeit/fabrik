package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// TestPauseHoldsAgainstUnprocessableComment_ResumesOnPostPauseComment is the
// #1813 / ADR-1813 regression: a human comment that predates a pause must not
// lift it on any number of polls (the #1752 loop), while a comment arriving
// after the pause resumes it on the next poll.
func TestPauseHoldsAgainstUnprocessableComment_ResumesOnPostPauseComment(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: smokeStages()})
	// Comment processing is scripted-completed so a genuine resume is
	// observable; a refused resume must never reach it.
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewCompleted())

	num := FileIssue(t, env, "pause anchor", "body", "Specify")

	// A human comment lands first...
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "## D2's blast radius is wider than filed")
	env.Clock.Advance(time.Minute)
	// ...and only then is the issue paused (the breaker-trip shape).
	for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := env.Sim.Sim().AddLabelToIssue(env.Owner, env.Repo, num, l); err != nil {
			t.Fatalf("AddLabelToIssue(%s): %v", l, err)
		}
	}
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	RunPolls(t, env, 6)
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, "fabrik:paused") || !hasLabel(labels, "fabrik:awaiting-input") {
		t.Fatalf("pause was lifted by a comment that predates it; labels=%v", labels)
	}
	if got := env.Claude.CommentCallCount("Specify"); got != 0 {
		t.Errorf("CommentCallCount(Specify) = %d, want 0 while the pause holds", got)
	}

	// A comment created after the pause resumes it.
	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "ok, please continue")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-input", 80)
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Error("fabrik:paused should be removed by the post-pause resume")
	}
}
