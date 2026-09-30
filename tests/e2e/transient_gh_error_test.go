//go:build e2e

package e2e

import "testing"

func TestIsTransientGitHubError(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"GraphQL: Something went wrong while executing your query on 2026-09-30T04:08:17Z. Please include `F477:399CD` when reporting this issue.", true},
		{"HTTP 502: Bad Gateway (https://api.github.com/graphql)", true},
		{"HTTP 503: Service Unavailable", true},
		{"Post \"https://api.github.com/graphql\": net/http: request canceled (Client.Timeout exceeded) timed out", true},
		{"GraphQL: Pull Request is not mergeable (mergePullRequest)", false},
		{"X Pull request handarbeit/fabrik-test-alpha#1 is not mergeable: the base branch policy prohibits the merge.", false},
		{"", false},
	} {
		if got := isTransientGitHubError(tc.out); got != tc.want {
			t.Errorf("isTransientGitHubError(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}
