//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// noWorkNeededSkipCommentPrefix is the literal prefix shared by both
// variants of the engine's own no-work-needed skip comment
// (engine/no_work_needed_settle.go: noWorkNeededSkipComment). Matching on
// HasPrefix rather than Contains ensures hasNoWorkNeededSkipComment can only
// be satisfied by a genuine engine-authored comment, not by a stage-output
// comment where an agent quotes the marker text in prose while reasoning
// about the feature — the same body-prefix convention findNewComments
// (engine/comments.go) uses to distinguish Fabrik's own output from prose,
// and the same fix pattern already shipped for #1320's cycle-limit marker.
const noWorkNeededSkipCommentPrefix = "🏭 **Fabrik — skipped: no work needed**"

// hasNoWorkNeededSkipComment reports whether bodies contains a genuine
// engine-authored no-work-needed skip comment. It matches on
// strings.HasPrefix, mirroring hasEngineCycleLimitComment
// (ci_fix_reinvoke_test.go).
func hasNoWorkNeededSkipComment(bodies []string) bool {
	for _, b := range bodies {
		if strings.HasPrefix(b, noWorkNeededSkipCommentPrefix) {
			return true
		}
	}
	return false
}

// TestHasNoWorkNeededSkipComment proves hasNoWorkNeededSkipComment can only
// be satisfied by a genuine engine-authored no-work-needed skip comment, not
// by a stage-output comment where an agent quotes the marker text while
// reasoning about the feature. This is a fast, directly-runnable check (no
// live bed, no GitHub API, no Claude cost) — run it with:
//
//	go test -tags e2e -run TestHasNoWorkNeededSkipComment ./tests/e2e/
//
// It exists to satisfy handarbeit/fabrik#1355's R2, which strengthens
// TestNoWorkNeeded to assert on this engine-unique observable rather than
// WaitForIssueClosed alone (which cannot distinguish the no-work-needed path
// from an ordinary close — the exact defect class #1320 and #1319 already
// exposed elsewhere in this suite).
func TestHasNoWorkNeededSkipComment(t *testing.T) {
	genuineGeneral := "🏭 **Fabrik — skipped: no work needed**\n\n" +
		"_Skipped: no work needed (FABRIK_NO_WORK_NEEDED emitted by Plan)._"

	genuineChildrenSpawned := "🏭 **Fabrik — skipped: no work needed**\n\n" +
		"_Skipped: no work needed — all work was delegated to spawned children " +
		"and this issue has no commits of its own._"

	// prose mirrors an agent quoting the marker text in prose while
	// discussing the feature — the shape of the #4049/#1320 false-pass, but
	// for this marker instead of the CI-fix cycle-limit one.
	prose := "🏭 **Fabrik — stage: Research**\n" +
		"*branch: fabrik/issue-1355 | commit: abc123*\n\n" +
		"## Research Findings\n\n" +
		"When Plan emits FABRIK_NO_WORK_NEEDED, the engine posts a comment " +
		"beginning \"🏭 **Fabrik — skipped: no work needed**\" to record the " +
		"decision. This is the observable TestNoWorkNeeded is designed to verify.\n"

	unrelated := "Thanks for filing this — I'll take a look."

	tests := []struct {
		name   string
		bodies []string
		want   bool
	}{
		{
			name:   "genuine general-path comment is accepted",
			bodies: []string{unrelated, genuineGeneral},
			want:   true,
		},
		{
			name:   "genuine children-spawned-variant comment is accepted",
			bodies: []string{genuineChildrenSpawned},
			want:   true,
		},
		{
			name:   "prose quoting the marker is rejected",
			bodies: []string{prose, unrelated},
			want:   false,
		},
		{
			name:   "unrelated comment alone is rejected",
			bodies: []string{unrelated},
			want:   false,
		},
		{
			name:   "no comments at all",
			bodies: nil,
			want:   false,
		},
		{
			name:   "both genuine and prose present — still accepted on the genuine one",
			bodies: []string{prose, genuineGeneral},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasNoWorkNeededSkipComment(tt.bodies)
			if got != tt.want {
				t.Fatalf("hasNoWorkNeededSkipComment(%q) = %v, want %v", tt.bodies, got, tt.want)
			}
		})
	}
}
