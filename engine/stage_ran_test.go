package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

func TestStageRanOnEntry(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	stage := &stages.Stage{Name: "Plan"}

	cases := []struct {
		name             string
		labels           []string
		enginePaused     bool
		resumedFromPause bool
		want             bool
	}{
		{name: "no signal runs the stage", labels: []string{"fabrik:cruise", "stage:Research:complete"}, want: false},
		{name: "complete", labels: []string{"stage:Plan:complete"}, want: true},
		{name: "failed", labels: []string{"stage:Plan:failed"}, want: true},
		{name: "reworking", labels: []string{"fabrik:reworking:Plan"}, want: true},
		{name: "awaiting-ci", labels: []string{"fabrik:awaiting-ci"}, want: true},
		{name: "awaiting-review", labels: []string{"fabrik:awaiting-review"}, want: true},
		{name: "awaiting-done", labels: []string{"fabrik:awaiting-done"}, want: true},
		{name: "engine paused", enginePaused: true, want: true},
		{name: "resumed from pause", resumedFromPause: true, want: true},
		{name: "other stage's complete is no signal", labels: []string{"stage:Implement:complete"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Number: 1, Labels: tc.labels}
			if got := eng.stageRanOnEntry(item, stage, tc.enginePaused, tc.resumedFromPause); got != tc.want {
				t.Errorf("stageRanOnEntry = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("the stage's own output comment is a signal", func(t *testing.T) {
		item := gh.ProjectItem{Number: 1, Comments: []gh.Comment{{ID: "C1", Body: "🏭 **Fabrik — stage: Plan**\noutput"}}}
		if !eng.stageRanOnEntry(item, stage, false, false) {
			t.Error("a stage output comment means the stage has run")
		}
	})

	t.Run("another stage's output comment is not", func(t *testing.T) {
		item := gh.ProjectItem{Number: 1, Comments: []gh.Comment{{ID: "C1", Body: "🏭 **Fabrik — stage: Research**\noutput"}}}
		if eng.stageRanOnEntry(item, stage, false, false) {
			t.Error("Research's comment must not count for Plan")
		}
	})

	t.Run("seam restores today's routing", func(t *testing.T) {
		eng.SetStageFirstRoutingDisabledForTest(true)
		defer eng.SetStageFirstRoutingDisabledForTest(false)
		if !eng.stageRanOnEntry(gh.ProjectItem{Number: 1}, stage, false, false) {
			t.Error("with the seam on, every item routes to the comment path")
		}
	})
}

// TestItemNeedsWork_PendingCommentOnStageNotRun covers admission for #2220.
// A pending comment on a stage that has run admits the item (the comment pass
// runs). On a stage that has not run it no longer short-circuits admission: the
// ordinary checks apply, so an item inside its retry backoff is not admitted
// just because a comment is waiting. The seam restores the old behaviour, so the
// backoff case is what proves the mirrored guard bites.
func TestItemNeedsWork_PendingCommentOnStageNotRun(t *testing.T) {
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.cfg.RetryBackoff = time.Hour
	comments := []gh.Comment{{ID: "C1", Author: "testuser", Body: "also cover offline"}}

	notRun := gh.ProjectItem{Number: 1, Status: "Research", Comments: comments}
	if !eng.itemNeedsWork(notRun) {
		t.Error("a never-run stage with a pending comment must be admitted (the stage runs)")
	}

	ran := gh.ProjectItem{Number: 2, Status: "Research", Labels: []string{"stage:Research:complete"}, Comments: comments}
	if !eng.itemNeedsWork(ran) {
		t.Error("a completed stage with a pending comment must be admitted (the comment pass runs)")
	}

	// A stage that has not run to completion and is inside its retry backoff:
	// the pending comment must not admit it, or a worker would be dispatched
	// every poll only for processItem to return without running anything.
	backoff := gh.ProjectItem{Number: 3, Status: "Research", Comments: comments}
	eng.store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: 3, StageName: "Research", At: time.Now()})
	if eng.itemNeedsWork(backoff) {
		t.Error("a not-run stage inside its retry backoff must not be admitted by a pending comment")
	}

	eng.SetStageFirstRoutingDisabledForTest(true)
	if !eng.itemNeedsWork(backoff) {
		t.Error("with the seam on, the pending comment admits the item as before")
	}
}
