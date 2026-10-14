package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
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

	t.Run("stage comment alone is not a signal", func(t *testing.T) {
		item := gh.ProjectItem{Number: 1, Comments: []gh.Comment{{ID: "C1", Body: "🏭 **Fabrik — stage: Plan**\npartial"}}}
		if eng.stageRanOnEntry(item, stage, false, false) {
			t.Error("a partial stage comment must not count as the stage having run")
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
