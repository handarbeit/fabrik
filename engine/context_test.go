// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
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

func TestParseMainSHA_NewFormat(t *testing.T) {
	body := "🏭 **Fabrik — stage: Research**\n*branch: fabrik/issue-1 | commit: abc12345 | main: def56789 | 2026-04-01 14:30 UTC*\n\nSome output"
	sha := parseMainSHA(body)
	if sha != "def56789" {
		t.Errorf("parseMainSHA = %q, want %q", sha, "def56789")
	}
}

func TestParseMainSHA_OldFormat(t *testing.T) {
	body := "🏭 **Fabrik — stage: Research**\n*branch: fabrik/issue-1 | commit: abc12345 | 2026-04-01 14:30 UTC*\n\nSome output"
	sha := parseMainSHA(body)
	if sha != "" {
		t.Errorf("parseMainSHA = %q, want empty string for old format", sha)
	}
}

func TestParseMainSHA_EmptyBody(t *testing.T) {
	sha := parseMainSHA("")
	if sha != "" {
		t.Errorf("parseMainSHA = %q, want empty string for empty body", sha)
	}
}

func TestParseMainSHA_SingleLine(t *testing.T) {
	sha := parseMainSHA("🏭 **Fabrik — stage: Plan**")
	if sha != "" {
		t.Errorf("parseMainSHA = %q, want empty string for single line", sha)
	}
}

func TestParseMainSHA_MainAsLastFieldBeforeTimestamp(t *testing.T) {
	// Ensure parsing handles the main: field regardless of position in the pipe-delimited metadata line.
	body := "🏭 **Fabrik — stage: Plan**\n*branch: b | commit: c | main: aaa11111 | 2026-01-01 00:00 UTC*"
	sha := parseMainSHA(body)
	if sha != "aaa11111" {
		t.Errorf("parseMainSHA = %q, want %q", sha, "aaa11111")
	}
}

// initTestRepo creates a git repo with an initial commit and returns its path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "--initial-branch=main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", args, err, out)
		}
	}
	// Create an initial commit.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "initial"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestWriteCodebaseChanges_NopriorSHA(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)

	workDir := t.TempDir()
	fabrikDir := filepath.Join(workDir, ".fabrik-context")
	os.MkdirAll(fabrikDir, 0755)

	item := gh.ProjectItem{
		Number: 1,
		Comments: []gh.Comment{
			// Old-format comment without main: field
			{Body: "🏭 **Fabrik — stage: Research**\n*branch: b | commit: c | 2026-01-01 00:00 UTC*\n\nOutput"},
		},
	}
	stage := &stages.Stage{Name: "Plan", Order: 2}
	eng.writeCodebaseChanges(item, stage, workDir, fabrikDir)

	if _, err := os.ReadFile(filepath.Join(fabrikDir, "codebase-changes.md")); err == nil {
		t.Error("codebase-changes.md should not exist when no prior main SHA")
	}
}

func TestWriteCodebaseChanges_SHAsMatch(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initTestRepo(t)

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(repoDir)

	// Get the current HEAD SHA (there's only one commit, so origin doesn't exist;
	// we simulate by using the repo itself as origin).
	sha, err := gitRevParse(repoDir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	shortSHA := sha[:8]

	// Create a fake "origin/main" ref pointing to the same commit.
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", sha)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	fabrikDir := filepath.Join(repoDir, ".fabrik-context")
	os.MkdirAll(fabrikDir, 0755)

	item := gh.ProjectItem{
		Number: 1,
		Comments: []gh.Comment{
			{Body: fmt.Sprintf("🏭 **Fabrik — stage: Research**\n*branch: b | commit: c | main: %s | 2026-01-01 00:00 UTC*\n\nOutput", shortSHA)},
		},
	}
	stage := &stages.Stage{Name: "Plan", Order: 2}
	eng.writeCodebaseChanges(item, stage, repoDir, fabrikDir)

	if _, err := os.ReadFile(filepath.Join(fabrikDir, "codebase-changes.md")); err == nil {
		t.Error("codebase-changes.md should not exist when SHAs match")
	}
}

func TestWriteCodebaseChanges_DiffWritten(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initTestRepo(t)

	// Record the initial commit SHA as the "prior main" SHA.
	oldSHA, err := gitRevParse(repoDir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	shortOldSHA := oldSHA[:8]

	// Add a second commit on main.
	if err := os.WriteFile(filepath.Join(repoDir, "newfile.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "add newfile"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	newSHA, err := gitRevParse(repoDir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// Create origin/main pointing to the new commit.
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", newSHA)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}

	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.worktreeManagers["owner/repo"] = NewWorktreeManager(repoDir)

	fabrikDir := filepath.Join(repoDir, ".fabrik-context")
	os.MkdirAll(fabrikDir, 0755)

	item := gh.ProjectItem{
		Number: 1,
		Comments: []gh.Comment{
			{Body: fmt.Sprintf("🏭 **Fabrik — stage: Research**\n*branch: b | commit: c | main: %s | 2026-01-01 00:00 UTC*\n\nOutput", shortOldSHA)},
		},
	}
	stage := &stages.Stage{Name: "Plan", Order: 2}
	eng.writeCodebaseChanges(item, stage, repoDir, fabrikDir)

	data, err := os.ReadFile(filepath.Join(fabrikDir, "codebase-changes.md"))
	if err != nil {
		t.Fatalf("codebase-changes.md should exist: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "Changes Since Research") {
		t.Errorf("missing header: %s", content)
	}
	if !strings.Contains(content, "newfile.go") {
		t.Errorf("missing file entry: %s", content)
	}
	if !strings.Contains(content, "1 commit(s)") {
		t.Errorf("missing commit count: %s", content)
	}
	if !strings.Contains(content, "New") {
		t.Errorf("missing 'New' status for added file: %s", content)
	}
}

// TestWriteContextFiles_CreatesGitignore verifies that writeContextFiles writes
// a .gitignore containing "*" inside .fabrik-context/ to prevent untracked
// context files from being staged by git add -A.
func TestWriteContextFiles_CreatesGitignore(t *testing.T) {
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{}
	eng := testEngine(client, claude)
	eng.cfg.Stages = []*stages.Stage{
		{Name: "Research", Order: 1},
	}

	workDir := t.TempDir()
	item := gh.ProjectItem{Number: 99, Body: "spec"}
	stage := &stages.Stage{Name: "Research", Order: 1}

	eng.writeContextFiles(item, stage, workDir, false)

	gitignorePath := filepath.Join(workDir, ".fabrik-context", ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf(".fabrik-context/.gitignore not written: %v", err)
	}
	if string(data) != "*\n" {
		t.Errorf(".gitignore content = %q, want %q", string(data), "*\n")
	}
}
