package engine

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestHasPauseComment_AnchoredToPauseShape (#1937): only the engine's own
// pause-comment shape counts as an existing pause. A fragment quoted anywhere
// else — a stage output recounting an earlier run's pause (the live failure,
// alpha#6565), or a human quoting one — must not suppress the real pause
// comment.
func TestHasPauseComment_AnchoredToPauseShape(t *testing.T) {
	stage := &stages.Stage{Name: "Validate"}
	frag := "The stage **Validate** has been re-invoked to fix CI failures"
	pause := "🏭 **Fabrik — CI fix cycle limit reached**\n\n" + frag + " 2 time(s), which has reached the maximum configured limit."
	stageOutput := "🏭 **Fabrik — stage: Research**\n*branch: fabrik/issue-6565 | commit: e5c415e6*\n\n## Research Findings\n\n" +
		"After 2 reinvokes, the engine posted: *\"" + frag + " 2 time(s), which has reached the maximum configured limit\"*."
	human := "Last time this happened Fabrik said \"" + frag + " 2 time(s)\" — is that expected?"

	cases := []struct {
		name     string
		comments []string
		want     bool
	}{
		{name: "real pause comment", comments: []string{pause}, want: true},
		{name: "quoted in a stage output", comments: []string{stageOutput}, want: false},
		{name: "quoted by a human", comments: []string{human}, want: false},
		{name: "human quoting a whole pause body mid-comment", comments: []string{"see below\n\n> " + pause}, want: false},
		{name: "real pause alongside a quote", comments: []string{stageOutput, pause}, want: true},
		{name: "no comments", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Number: 1}
			for _, b := range tc.comments {
				item.Comments = append(item.Comments, gh.Comment{Body: b})
			}
			if got := hasCIGatePauseComment(item, stage); got != tc.want {
				t.Errorf("hasCIGatePauseComment = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasPauseComment_EveryDomainMessageMatchesItsOwnFragment pins that each
// pause domain's real message shape is still recognised by the anchored check
// — the #1408 reapply-without-repost behaviour depends on it.
func TestHasPauseComment_EveryDomainMessageMatchesItsOwnFragment(t *testing.T) {
	stage := &stages.Stage{Name: "Validate"}
	for _, tc := range []struct {
		name, header, frag string
	}{
		{"CI wait timeout", "🏭 **Fabrik — CI wait timeout**", "The CI gate for stage **Validate** timed out"},
		{"CI fix cycle limit", "🏭 **Fabrik — CI fix cycle limit reached**", "The stage **Validate** has been re-invoked to fix CI failures"},
		{"review cycle limit", "🏭 **Fabrik — review cycle limit reached**", reviewCyclePauseFragment(stage)},
		{"rebase cycle limit", "🏭 **Fabrik — rebase cycle limit reached**", rebaseCyclePauseFragment(stage)},
		{"enqueue cycle limit", "🏭 **Fabrik — merge-queue re-enqueue limit reached**", enqueueCyclePauseFragment(stage)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Number: 1, Comments: []gh.Comment{{Body: tc.header + "\n\n" + tc.frag + " 3 time(s), which has reached the limit."}}}
			if !hasPauseComment(item, tc.frag) {
				t.Errorf("real %s pause comment not recognised", tc.name)
			}
		})
	}
}
