package engine

import (
	"context"
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// #2345: the engine hands the Status field ID it resolves on its first poll to
// the board cache, which filters projects_v2_item edits by it.
func TestPoll_PushesStatusFieldIDIntoCache(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectBoardFn: func(string, string, int, string) (*gh.ProjectBoard, error) {
			return &gh.ProjectBoard{ProjectID: "PVT_1"}, nil
		},
		fetchStatusFieldFn: func(string) (*gh.StatusField, error) {
			return &gh.StatusField{FieldID: "PVTSSF_status", Options: map[string]string{}}, nil
		},
	}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	eng.readClient = cache

	if err := eng.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := cache.StatusFieldID(); got != "PVTSSF_status" {
		t.Fatalf("cache StatusFieldID = %q, want PVTSSF_status", got)
	}
}
