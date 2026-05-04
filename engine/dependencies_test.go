package engine

import (
	"context"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// depTestStages returns a two-stage pipeline for dependency gate tests.
func depTestStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1, Prompt: "specify"},
		{Name: "Research", Order: 2, Prompt: "research"},
		{Name: "Implement", Order: 3, Prompt: "implement"},
	}
}

func depTestEngine(client *mockGitHubClient) *Engine {
	return testEngineWithStages(client, depTestStages())
}

func TestCheckDependencies_NoDeps_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: 10, Repo: "owner/repo"}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when no deps")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
	if len(client.removeLabelCalls) != 0 {
		t.Errorf("expected no label removes, got %d", len(client.removeLabelCalls))
	}
}

func TestCheckDependencies_AllDepsClosed_ReturnsFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 9, State: "CLOSED", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all deps closed")
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label adds, got %d", len(client.addLabelCalls))
	}
}

func TestCheckDependencies_AllDepsClosed_RemovesBlockedLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"},
		BlockedBy: []gh.Dependency{
			{Number: 9, State: "CLOSED", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked when all deps closed")
	}
	if len(client.removeLabelCalls) != 1 {
		t.Fatalf("expected 1 remove label call, got %d", len(client.removeLabelCalls))
	}
	if client.removeLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected removal of fabrik:blocked, got %q", client.removeLabelCalls[0].labelName)
	}
}

func TestCheckDependencies_OpenDeps_ReturnsTrue_FirstTime(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// No fabrik:blocked label — first time blocking
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked with open deps")
	}
	// Should have added fabrik:blocked label
	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 add label call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected fabrik:blocked, got %q", client.addLabelCalls[0].labelName)
	}
	// Should have posted comment
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	if !strings.Contains(client.addCommentCalls[0].body, "#8") {
		t.Errorf("comment should mention #8, got: %q", client.addCommentCalls[0].body)
	}
}

func TestCheckDependencies_OpenDeps_AlreadyBlocked_NoComment(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		Labels: []string{"fabrik:blocked"}, // already blocked
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked with open deps")
	}
	// No comment or label add because already blocked
	if len(client.addCommentCalls) != 0 {
		t.Errorf("expected no comment when already blocked, got %d", len(client.addCommentCalls))
	}
	if len(client.addLabelCalls) != 0 {
		t.Errorf("expected no label add when already blocked, got %d", len(client.addLabelCalls))
	}
}

func TestCheckDependencies_FirstStage_BlockedWithOpenDeps(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// Has open blocker — first stage is no longer exempt (#473)
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	// "Specify" is the first stage in depTestStages()
	stage := &stages.Stage{Name: "Specify"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked for first stage when open deps exist")
	}
	if len(client.addLabelCalls) != 1 {
		t.Fatalf("expected 1 add label call, got %d", len(client.addLabelCalls))
	}
	if client.addLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected fabrik:blocked, got %q", client.addLabelCalls[0].labelName)
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
}

func TestCheckDependencies_CrossRepoDep_FormattedCorrectly(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 99, State: "OPEN", Repo: "other/repo"}, // cross-repo
		},
	}
	stage := &stages.Stage{Name: "Research"}

	blocked := eng.checkDependencies(board, item, stage)

	if !blocked {
		t.Error("expected blocked")
	}
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(client.addCommentCalls))
	}
	// Cross-repo dep should be formatted as "other/repo#99"
	if !strings.Contains(client.addCommentCalls[0].body, "other/repo#99") {
		t.Errorf("expected cross-repo format in comment, got: %q", client.addCommentCalls[0].body)
	}
}

// TestProcessItem_SkipsBlockedNonFirstStage verifies that processItem returns nil
// without invoking Claude when an item in a non-first stage has an open BlockedBy
// dependency. This exercises the checkDependencies call added before stage work
// begins (Bug 1 fix).
func TestProcessItem_SkipsBlockedNonFirstStage(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	// depTestStages: Specify (order 1), Research (order 2), Implement (order 3)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        depTestStages(),
		},
		client,
		claude,
		NewWorktreeManager("/tmp/test-repo"),
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 42,
		Title:  "Blocked item",
		Status: "Research", // non-first stage
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 41, State: "OPEN", Repo: "owner/repo"},
		},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// Claude must not be invoked — the dependency gate should return nil before
	// any worktree setup or stage invocation.
	if len(claude.calls) != 0 {
		t.Errorf("expected no Claude invocations for blocked non-first stage item, got %d", len(claude.calls))
	}
}

// TestProcessItem_SkipsBlockedFirstStage verifies the regression path from #473:
// processItem must skip Claude invocation for the first stage (Specify) when the
// item has an open BlockedBy dependency. Previously, checkDependencies exempted
// the first stage and Specify ran against pre-merge code despite open blockers.
func TestProcessItem_SkipsBlockedFirstStage(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	// depTestStages: Specify (order 1), Research (order 2), Implement (order 3)
	eng := NewWithDeps(
		Config{
			Owner:         "owner",
			Repo:          "repo",
			ProjectNum:    1,
			User:          "testuser",
			Token:         "token",
			MaxConcurrent: 5,
			Stages:        depTestStages(),
		},
		client,
		claude,
		NewWorktreeManager("/tmp/test-repo"),
	)

	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 43,
		Title:  "Blocked first-stage item",
		Status: "Specify", // first stage
		Repo:   "owner/repo",
		BlockedBy: []gh.Dependency{
			{Number: 41, State: "OPEN", Repo: "owner/repo"},
		},
	}

	err := eng.processItem(context.Background(), board, item)
	if err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// Claude must not be invoked — the dependency gate now applies to the first stage.
	if len(claude.calls) != 0 {
		t.Errorf("expected no Claude invocations for blocked first-stage item, got %d", len(claude.calls))
	}
	// fabrik:blocked label must be added.
	if len(client.addLabelCalls) != 1 || client.addLabelCalls[0].labelName != "fabrik:blocked" {
		t.Errorf("expected fabrik:blocked to be added, got addLabelCalls=%v", client.addLabelCalls)
	}
}
