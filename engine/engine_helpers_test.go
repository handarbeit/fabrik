package engine

import (
	"github.com/verveguy/fabrik/boardcache"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

func testStages() []*stages.Stage {
	return []*stages.Stage{
		{
			Name:       "Research",
			Order:      1,
			Prompt:     "Do research",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:       "Plan",
			Order:      2,
			Prompt:     "Make a plan",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
		{
			Name:       "Implement",
			Order:      3,
			Prompt:     "Implement it",
			Completion: stages.CompletionCriteria{Type: "claude"},
		},
	}
}
func testEngine(client *mockGitHubClient, claude *mockClaudeInvoker) *Engine {
	return NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        testStages(),
		},
		client,
		claude,
		NewWorktreeManager("/tmp/test-repo"),
	)
}
// testEngineWithCache creates an Engine with a live CacheImpl wired as readClient.
// Returns the engine and the cache so tests can query cached state directly.
// The cache is bootstrapped with a single item: owner/repo issue #1 in "Research".
func testEngineWithCache(client *mockGitHubClient, claude *mockClaudeInvoker) (*Engine, *boardcache.CacheImpl) {
	eng := testEngine(client, claude)

	cache := boardcache.NewCacheImpl(client, func(string, ...any) {})
	cache.Bootstrap(&gh.ProjectBoard{
		ProjectID: "PVT_1",
		Title:     "Test Board",
		OwnerType: "organization",
		Items: []gh.ProjectItem{
			{
				ID:     "I_001",
				ItemID: "PVTI_001",
				Number: 1,
				Title:  "Test Issue",
				Repo:   "owner/repo",
				Status: "Research",
				Labels: []string{},
			},
		},
	})
	eng.readClient = cache
	return eng, cache
}

func testStagesWithCleanup() []*stages.Stage {
	ss := testStages()
	return append(ss, &stages.Stage{
		Name:            "Done",
		Order:           99,
		CleanupWorktree: true,
	})
}
