//go:build e2e

package e2e

import "testing"

// TestHasEngineCycleLimitComment proves hasEngineCycleLimitComment can only
// be satisfied by a genuine engine-authored CI-fix cycle-limit pause
// comment, not by a stage-output comment where an agent quotes the marker
// text while reasoning about it. This is a fast, directly-runnable check
// (no live bed, no GitHub API, no Claude cost) — run it with:
//
//	go test -tags e2e -run TestHasEngineCycleLimitComment ./tests/e2e/
//
// It exists to satisfy handarbeit/fabrik#1320's "verified directly, not
// just inferred" requirement for the false-pass fix, using a shape modeled
// directly on the real evidence from handarbeit/fabrik-test-alpha#4049: the
// only comment containing the marker text there was the Research stage's
// own output, quoting it in prose while researching the feature — the
// engine never posted a cycle-limit comment on that issue at all.
func TestHasEngineCycleLimitComment(t *testing.T) {
	// prose4049 mirrors the actual Research-stage comment body from #4049
	// that satisfied the old strings.Contains scan despite the engine never
	// pausing for the cycle limit on that issue.
	prose4049 := "🏭 **Fabrik — stage: Research**\n" +
		"*branch: fabrik/issue-1234 | commit: abc123*\n\n" +
		"## Research Findings\n\n" +
		"When `pauseForCIFixCycleLimit` fires, the engine posts a comment " +
		"beginning \"🏭 **Fabrik — CI fix cycle limit reached**\" directly to " +
		"the issue (not the PR). This is the behavior TestCIFixReinvokeCycleLimit " +
		"is designed to verify.\n"

	genuine := "🏭 **Fabrik — CI fix cycle limit reached**\n\n" +
		"The stage **Validate** has been re-invoked to fix CI failures 2 time(s), " +
		"which has reached the maximum configured limit (`FABRIK_MAX_CI_FIX_CYCLES=2`).\n\n" +
		"CI checks are still failing after repeated fix attempts. " +
		"Fabrik has paused this issue for human review. Once the CI situation is resolved, " +
		"remove the `fabrik:paused` label to resume."

	waitTimeout := "🏭 **Fabrik — CI wait timeout**\n\n" +
		"The CI gate for stage **Validate** timed out waiting for checks to pass.\n\n" +
		"Fabrik has paused this issue. Please check the PR's CI status, address any " +
		"failures, and then remove the `fabrik:paused` label to resume."

	unrelated := "Thanks for filing this — I'll take a look."

	tests := []struct {
		name   string
		bodies []string
		want   bool
	}{
		{
			name:   "genuine engine comment is accepted",
			bodies: []string{unrelated, genuine},
			want:   true,
		},
		{
			name:   "prose quoting the marker (the #4049 false-pass shape) is rejected",
			bodies: []string{prose4049, unrelated},
			want:   false,
		},
		{
			name:   "prose quoting the marker alongside no genuine comment stays rejected",
			bodies: []string{prose4049},
			want:   false,
		},
		{
			name:   "sibling CI wait-timeout comment is not mistaken for the cycle-limit marker",
			bodies: []string{waitTimeout},
			want:   false,
		},
		{
			name:   "no comments at all",
			bodies: nil,
			want:   false,
		},
		{
			name:   "both genuine and prose present — still accepted on the genuine one",
			bodies: []string{prose4049, genuine},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasEngineCycleLimitComment(tt.bodies)
			if got != tt.want {
				t.Fatalf("hasEngineCycleLimitComment(%q) = %v, want %v", tt.bodies, got, tt.want)
			}
		})
	}
}

// TestHasEngineCIWaitTimeoutComment covers the sibling helper used to build
// the race-vs-regression diagnostic when the cycle-limit comment is absent.
func TestHasEngineCIWaitTimeoutComment(t *testing.T) {
	genuine := "🏭 **Fabrik — CI wait timeout**\n\nThe CI gate for stage **Validate** timed out waiting for checks to pass."
	prose := "While researching, I confirmed the engine posts \"🏭 **Fabrik — CI wait timeout**\" when the timer elapses."

	tests := []struct {
		name   string
		bodies []string
		want   bool
	}{
		{name: "genuine comment accepted", bodies: []string{genuine}, want: true},
		{name: "prose quoting the marker rejected", bodies: []string{prose}, want: false},
		{name: "no comments", bodies: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasEngineCIWaitTimeoutComment(tt.bodies)
			if got != tt.want {
				t.Fatalf("hasEngineCIWaitTimeoutComment(%q) = %v, want %v", tt.bodies, got, tt.want)
			}
		})
	}
}
