package engine

import (
	"testing"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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
func testEngine(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker) *Engine {
	t.Helper()
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
		NewWorktreeManager(t.TempDir()),
	)
}

// testEngineWithCache creates an Engine with a live CacheImpl wired as readClient.
// Returns the engine and the cache so tests can query cached state directly.
// The cache is bootstrapped with a single item: owner/repo issue #1 in "Research".
func testEngineWithCache(t *testing.T, client *mockGitHubClient, claude *mockClaudeInvoker) (*Engine, *boardcache.CacheImpl) {
	t.Helper()
	eng := testEngine(t, client, claude)

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	cache.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"},
	}, "PVT_1")
	eng.readClient = cache
	return eng, cache
}

// testBootstrapFromBoard adapts a *gh.ProjectBoard to BootstrapFromProbe format,
// seeding labels separately via ApplyLabelAdded. Used to migrate tests off the
// removed Bootstrap method.
func testBootstrapFromBoard(cache *boardcache.CacheImpl, board *gh.ProjectBoard) {
	probeItems := make([]gh.BoardProbeItem, 0, len(board.Items))
	for _, item := range board.Items {
		probeItems = append(probeItems, gh.BoardProbeItem{
			ContentID:          item.ID,
			ItemID:             item.ItemID,
			Number:             item.Number,
			IsPR:               item.IsPR,
			IsClosed:           item.IsClosed,
			Status:             item.Status,
			Repo:               item.Repo,
			EffectiveUpdatedAt: item.UpdatedAt,
			LinkedPRNumber:     item.LinkedPRNumber,
		})
	}
	cache.BootstrapFromProbe(probeItems, board.ProjectID)
	for _, item := range board.Items {
		key := boardcache.ItemKey(item.Repo, item.Number)
		for _, l := range item.Labels {
			cache.ApplyLabelAdded(key, l)
		}
	}
}

func testStagesWithCleanup() []*stages.Stage {
	ss := testStages()
	return append(ss, &stages.Stage{
		Name:            "Done",
		Order:           99,
		CleanupWorktree: true,
	})
}

// testStagesWithBacklog returns testStagesWithCleanup plus a declarative
// unmanaged Backlog stage (order -1, precedes Research).
func testStagesWithBacklog() []*stages.Stage {
	ss := testStagesWithCleanup()
	return append([]*stages.Stage{{
		Name:      "Backlog",
		Order:     -1,
		Unmanaged: true,
	}}, ss...)
}
