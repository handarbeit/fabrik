package engine

import (
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
func testStagesWithCleanup() []*stages.Stage {
	ss := testStages()
	return append(ss, &stages.Stage{
		Name:            "Done",
		Order:           99,
		CleanupWorktree: true,
	})
}
