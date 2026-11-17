package inconclusive

import "testing"

func TestIsTransientGitHubWrite(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		// 0.0.93 gate, beta#767: the PR was created, the follow-up update failed.
		{"https://github.com/o/r/pull/767\npull request update failed: GraphQL: Something went wrong while executing your query. Please include `EC03:C49D5` when reporting this issue.", true},
		{"HTTP 502: Bad Gateway (https://api.github.com/graphql)", true},
		{"Post \"https://api.github.com/graphql\": read tcp: connection reset by peer", true},
		{"Post \"https://api.github.com/graphql\": net/http: TLS handshake timeout", true},
		// Test bugs stay failures.
		{"failed to update https://github.com/o/r/issues/1: 'stage:Nope:complete' not found", false},
		{"pull request create failed: GraphQL: Base ref must be a branch (createPullRequest)", false},
		{"HTTP 404: Not Found", false},
		{"HTTP 422: Validation Failed", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsTransientGitHubWrite(c.out); got != c.want {
			t.Errorf("IsTransientGitHubWrite(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}
