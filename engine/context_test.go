package engine

import (
	"os"
	"path/filepath"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestFindStageComment_NoComments(t *testing.T) {
	result := findStageComment(nil, "Research")
	if result != nil {
		t.Errorf("expected nil for empty comments, got %+v", result)
	}
}

func TestFindStageComment_NoMatch(t *testing.T) {
	comments := []gh.Comment{
		{Body: "some user comment"},
		{Body: "🏭 **Fabrik — stage: Plan**\nsome content"},
	}
	result := findStageComment(comments, "Research")
	if result != nil {
		t.Errorf("expected nil for no matching stage, got %+v", result)
	}
}

func TestFindStageComment_ExactMatch(t *testing.T) {
	comments := []gh.Comment{
		{Body: "🏭 **Fabrik — stage: Research**\nresearch output", DatabaseID: 42},
	}
	result := findStageComment(comments, "Research")
	if result == nil {
		t.Fatal("expected match, got nil")
	}
	if result.DatabaseID != 42 {
		t.Errorf("DatabaseID = %d, want 42", result.DatabaseID)
	}
}

func TestFindStageComment_ReturnsLast(t *testing.T) {
	// Multiple comments matching the same stage — should return the last one.
	comments := []gh.Comment{
		{Body: "🏭 **Fabrik — stage: Research**\nfirst run", DatabaseID: 1},
		{Body: "🏭 **Fabrik — stage: Research**\nsecond run", DatabaseID: 2},
	}
	result := findStageComment(comments, "Research")
	if result == nil {
		t.Fatal("expected match, got nil")
	}
	if result.DatabaseID != 2 {
		t.Errorf("DatabaseID = %d, want 2 (last match)", result.DatabaseID)
	}
}

func TestFindStageComment_DoesNotMatchVariant(t *testing.T) {
	// "(comment review)" variant should not match base stage name.
	comments := []gh.Comment{
		{Body: "🏭 **Fabrik — stage: Research (comment review)**\ncomment review output", DatabaseID: 99},
	}
	result := findStageComment(comments, "Research")
	if result != nil {
		t.Errorf("expected nil (variant should not match base), got DatabaseID=%d", result.DatabaseID)
	}
}

func TestFindStageComment_MatchesAmongMixed(t *testing.T) {
	comments := []gh.Comment{
		{Body: "user comment"},
		{Body: "🏭 **Fabrik — stage: Plan**\nplan output", DatabaseID: 10},
		{Body: "🏭 **Fabrik — stage: Research**\nresearch output", DatabaseID: 20},
		{Body: "another user comment"},
	}
	result := findStageComment(comments, "Research")
	if result == nil {
		t.Fatal("expected match")
	}
	if result.DatabaseID != 20 {
		t.Errorf("DatabaseID = %d, want 20", result.DatabaseID)
	}
}

func TestWriteContextFiles_IssueAlwaysWritten(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
	}

	workDir := t.TempDir()
	item := gh.ProjectItem{
		Number: 42,
		Body:   "# My Issue\n\nSpec content here.",
	}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.writeContextFiles(item, stage, workDir, false)

	data, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "issue.md"))
	if err != nil {
		t.Fatalf("issue.md not written: %v", err)
	}
	if string(data) != item.Body {
		t.Errorf("issue.md content = %q, want %q", string(data), item.Body)
	}
}

func TestWriteContextFiles_PriorStagesOnly(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
		{Name: "Implement", Order: 3},
	}

	workDir := t.TempDir()
	item := gh.ProjectItem{
		Number: 5,
		Body:   "spec",
		Comments: []gh.Comment{
			{Body: "🏭 **Fabrik — stage: Research**\nresearch out", DatabaseID: 1},
			{Body: "🏭 **Fabrik — stage: Plan**\nplan out", DatabaseID: 2},
		},
	}
	// Current stage is Plan (Order=2): only Research (Order=1) should be written.
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.writeContextFiles(item, stage, workDir, false)

	// Research should be written (Order 1 < 2)
	if _, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "stage-Research.md")); err != nil {
		t.Errorf("stage-Research.md should be written for prior stage: %v", err)
	}

	// Plan should NOT be written (Order 2 == current, not strictly less)
	if _, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "stage-Plan.md")); err == nil {
		t.Error("stage-Plan.md should not be written (current stage, not prior)")
	}
}

func TestWriteContextFiles_CommentProcessingIncludesCurrent(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
	}

	workDir := t.TempDir()
	item := gh.ProjectItem{
		Number: 7,
		Body:   "spec",
		Comments: []gh.Comment{
			{Body: "🏭 **Fabrik — stage: Research**\nresearch out", DatabaseID: 1},
			{Body: "🏭 **Fabrik — stage: Plan**\nplan out", DatabaseID: 2},
		},
	}
	// Comment processing for Plan — should include Plan itself.
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.writeContextFiles(item, stage, workDir, true)

	for _, name := range []string{"stage-Research.md", "stage-Plan.md"} {
		if _, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", name)); err != nil {
			t.Errorf("%s should be written for comment processing: %v", name, err)
		}
	}
}

func TestWriteContextFiles_SkipsStagesWithNoComment(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = []*stages.Stage{
		{Name: "Research", Order: 1},
		{Name: "Plan", Order: 2},
	}

	workDir := t.TempDir()
	item := gh.ProjectItem{
		Number:   8,
		Body:     "spec",
		Comments: []gh.Comment{}, // no stage comments
	}
	stage := &stages.Stage{Name: "Plan", Order: 2}

	eng.writeContextFiles(item, stage, workDir, false)

	// No stage-Research.md since there's no matching comment.
	if _, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "stage-Research.md")); err == nil {
		t.Error("stage-Research.md should not exist when there is no matching comment")
	}
	// But issue.md should still exist.
	if _, err := os.ReadFile(filepath.Join(workDir, ".fabrik-context", "issue.md")); err != nil {
		t.Errorf("issue.md should always be written: %v", err)
	}
}
