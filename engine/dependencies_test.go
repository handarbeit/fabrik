package engine

import (
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

func TestCheckDependencies_FirstStage_AlwaysFalse(t *testing.T) {
	client := &mockGitHubClient{}
	eng := depTestEngine(client)
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{
		Number: 10,
		Repo:   "owner/repo",
		// Has open blocker — but should be ignored for first stage
		BlockedBy: []gh.Dependency{
			{Number: 8, State: "OPEN", Repo: "owner/repo"},
		},
	}
	// "Specify" is the first stage in depTestStages()
	stage := &stages.Stage{Name: "Specify"}

	blocked := eng.checkDependencies(board, item, stage)

	if blocked {
		t.Error("expected not blocked for first stage regardless of deps")
	}
	if len(client.addLabelCalls) != 0 || len(client.addCommentCalls) != 0 {
		t.Error("expected no label or comment ops for first stage")
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
