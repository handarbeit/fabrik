package engine

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// #2344: the live-read markers describe the fetch that just filled the item and
// are cleared once it is applied, so applying the same (older) copy again later
// in the poll cannot present its labels and closed state as freshly read.
func TestApplyDeepFetched_ClearsLiveReadMarkers(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{
		Repo:               "owner/repo",
		Number:             7,
		Status:             "Implement",
		Labels:             []string{"bug"},
		DeepFetchLive:      true,
		DeepFetchStartedAt: time.Now(),
	}

	e.applyDeepFetched("owner/repo", &item)

	if item.DeepFetchLive || !item.DeepFetchStartedAt.IsZero() {
		t.Errorf("markers not cleared after the apply: live=%v startedAt=%v", item.DeepFetchLive, item.DeepFetchStartedAt)
	}
}
