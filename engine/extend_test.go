package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// nopLogf is a no-op logfFn for tests that don't assert log output.
func nopLogf(tag, format string, args ...any) {}

func TestHasExtendTurnsLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"label present", []string{"fabrik:extend-turns"}, true},
		{"label absent", []string{"fabrik:yolo", "model:opus"}, false},
		{"empty labels", nil, false},
		{"wrong prefix only", []string{"extend-turns", "fabrik:extend"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := gh.ProjectItem{Labels: tc.labels}
			if got := hasExtendTurnsLabel(item); got != tc.want {
				t.Errorf("hasExtendTurnsLabel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSnapshotBaseline_NoSignalStages(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	item := gh.ProjectItem{
		Comments: []gh.Comment{{Body: "c1"}, {Body: "c2"}},
		LinkedPRResolvedThreadCount: 3,
	}

	for _, stageName := range []string{"Research", "Specify", "Done"} {
		stage := &stages.Stage{Name: stageName}
		b := snapshotBaseline(stage, item, repoDir)
		if b.gitHeadSHA != "" {
			t.Errorf("stage %s: expected empty gitHeadSHA for no-signal stage, got %q", stageName, b.gitHeadSHA)
		}
		if b.commentCount != 0 {
			t.Errorf("stage %s: expected 0 commentCount, got %d", stageName, b.commentCount)
		}
		if b.resolvedThreadCount != 0 {
			t.Errorf("stage %s: expected 0 resolvedThreadCount, got %d", stageName, b.resolvedThreadCount)
		}
	}
}

func TestSnapshotBaseline_Implement(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{Comments: []gh.Comment{{Body: "c1"}}, LinkedPRResolvedThreadCount: 5}
	b := snapshotBaseline(stage, item, repoDir)

	if b.gitHeadSHA == "" {
		t.Fatal("expected non-empty gitHeadSHA for Implement stage")
	}
	if b.commentCount != 0 {
		t.Errorf("Implement: commentCount should be 0 (not a Validate signal), got %d", b.commentCount)
	}
	if b.resolvedThreadCount != 0 {
		t.Errorf("Implement: resolvedThreadCount should be 0, got %d", b.resolvedThreadCount)
	}
}

func TestSnapshotBaseline_Review(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{LinkedPRResolvedThreadCount: 7}
	b := snapshotBaseline(stage, item, repoDir)

	if b.gitHeadSHA == "" {
		t.Fatal("expected non-empty gitHeadSHA for Review stage")
	}
	if b.resolvedThreadCount != 7 {
		t.Errorf("Review: resolvedThreadCount = %d, want 7", b.resolvedThreadCount)
	}
}

func TestSnapshotBaseline_Validate(t *testing.T) {
	stage := &stages.Stage{Name: "Validate"}
	item := gh.ProjectItem{Comments: []gh.Comment{{Body: "a"}, {Body: "b"}, {Body: "c"}}}
	b := snapshotBaseline(stage, item, "/irrelevant")

	if b.commentCount != 3 {
		t.Errorf("Validate: commentCount = %d, want 3", b.commentCount)
	}
	if b.gitHeadSHA != "" {
		t.Errorf("Validate: gitHeadSHA should be empty, got %q", b.gitHeadSHA)
	}
}

func TestDetectProgress_NoSignalStage(t *testing.T) {
	ctx := context.Background()
	for _, stageName := range []string{"Research", "Specify", "Plan", "Done"} {
		stage := &stages.Stage{Name: stageName}
		item := gh.ProjectItem{}
		b := progressBaseline{}
		progress, err := detectProgress(ctx, stage, &item, b, "/irrelevant", &mockGitHubClient{}, nopLogf)
		if err != nil {
			t.Errorf("stage %s: unexpected error: %v", stageName, err)
		}
		if progress {
			t.Errorf("stage %s: expected no progress for no-signal stage", stageName)
		}
	}
}

func TestDetectProgress_Implement_NoNewCommits(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	sha, err := gitHeadSHA(repoDir)
	if err != nil {
		t.Fatalf("gitHeadSHA: %v", err)
	}
	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: sha}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{}, nopLogf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progress {
		t.Error("expected no progress when SHA unchanged")
	}
}

func TestDetectProgress_Implement_NewCommit(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	// Snapshot the old SHA
	oldSHA, err := gitHeadSHA(repoDir)
	if err != nil {
		t.Fatalf("gitHeadSHA: %v", err)
	}

	// Create a new commit
	cmds := [][]string{
		{"git", "commit", "--allow-empty", "-m", "progress commit"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: oldSHA}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{}, nopLogf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progress {
		t.Error("expected progress detected after new commit")
	}
}

func TestDetectProgress_Validate_NoNewComments(t *testing.T) {
	ctx := context.Background()
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{{Body: "existing"}}
			return nil
		},
	}
	stage := &stages.Stage{Name: "Validate"}
	item := gh.ProjectItem{}
	b := progressBaseline{commentCount: 1}

	progress, err := detectProgress(ctx, stage, &item, b, "/irrelevant", client, nopLogf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progress {
		t.Error("expected no progress when comment count unchanged")
	}
}

func TestDetectProgress_Validate_NewComment(t *testing.T) {
	ctx := context.Background()
	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.Comments = []gh.Comment{{Body: "old"}, {Body: "new"}}
			return nil
		},
	}
	stage := &stages.Stage{Name: "Validate"}
	item := gh.ProjectItem{}
	b := progressBaseline{commentCount: 1}

	progress, err := detectProgress(ctx, stage, &item, b, "/irrelevant", client, nopLogf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progress {
		t.Error("expected progress when comment count increased")
	}
}

func TestDetectProgress_Review_NewResolvedThread(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	sha, err := gitHeadSHA(repoDir)
	if err != nil {
		t.Fatalf("gitHeadSHA: %v", err)
	}

	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			item.LinkedPRResolvedThreadCount = 2
			return nil
		},
	}
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: sha, resolvedThreadCount: 1}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, client, nopLogf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progress {
		t.Error("expected progress when resolved thread count increased")
	}
}

func TestDetectProgress_Review_FetchError(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	sha, err := gitHeadSHA(repoDir)
	if err != nil {
		t.Fatalf("gitHeadSHA: %v", err)
	}

	client := &mockGitHubClient{
		fetchItemDetailsFn: func(item *gh.ProjectItem) error {
			return &testError{"api failure"}
		},
	}
	stage := &stages.Stage{Name: "Review"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: sha, resolvedThreadCount: 0}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, client, nopLogf)
	if err == nil {
		t.Fatal("expected error when FetchItemDetails fails")
	}
	if progress {
		t.Error("expected no progress on fetch error")
	}
}

// --- logging tests ---

// captureLogf returns a logfFn that records all calls and a getter function.
func captureLogf() (func(tag, format string, args ...any), func() []string) {
	var lines []string
	fn := func(tag, format string, args ...any) {
		import_ := fmt.Sprintf("[%s] ", tag) + fmt.Sprintf(format, args...)
		lines = append(lines, import_)
	}
	return fn, func() []string { return lines }
}

func TestDetectProgress_Implement_SHAChanged_LogsProgress(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	oldSHA, err := gitHeadSHA(repoDir)
	if err != nil {
		t.Fatalf("gitHeadSHA: %v", err)
	}
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "new work")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s: %v", out, err)
	}
	newSHA, _ := gitHeadSHA(repoDir)

	logFn, getLines := captureLogf()
	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: oldSHA}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{}, logFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progress {
		t.Error("expected progress=true when SHA changed")
	}
	lines := getLines()
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "has_progress=true") {
		t.Errorf("log line should contain has_progress=true: %q", lines[0])
	}
	if !strings.Contains(lines[0], oldSHA) || !strings.Contains(lines[0], newSHA) {
		t.Errorf("log line should contain both SHAs: %q", lines[0])
	}
}

func TestDetectProgress_Implement_NoCommitCleanTree_LogsFalse(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	sha, _ := gitHeadSHA(repoDir)
	logFn, getLines := captureLogf()
	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: sha, workingTreeDirty: false}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{}, logFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progress {
		t.Error("expected progress=false when SHA unchanged and tree clean")
	}
	lines := getLines()
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "has_progress=false") {
		t.Errorf("log line should contain has_progress=false: %q", lines[0])
	}
}

func TestDetectProgress_Implement_DirtyTree_BaselineClean_LogsProgress(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	sha, _ := gitHeadSHA(repoDir)
	// Create an uncommitted file (not engine-managed).
	if err := os.WriteFile(filepath.Join(repoDir, "work.go"), []byte("package foo\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logFn, getLines := captureLogf()
	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{}
	b := progressBaseline{gitHeadSHA: sha, workingTreeDirty: false}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{}, logFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !progress {
		t.Error("expected progress=true when baseline clean and working tree dirty")
	}
	lines := getLines()
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "has_progress=true") {
		t.Errorf("log line should contain has_progress=true: %q", lines[0])
	}
}

func TestDetectProgress_Implement_DirtyTree_BaselineDirty_NoProgress(t *testing.T) {
	skipIfNoGit(t)
	ctx := context.Background()
	repoDir := initBareRepo(t)

	sha, _ := gitHeadSHA(repoDir)
	// Pre-existing dirty file at baseline time.
	if err := os.WriteFile(filepath.Join(repoDir, "old-work.go"), []byte("package foo\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	logFn, getLines := captureLogf()
	stage := &stages.Stage{Name: "Implement"}
	item := gh.ProjectItem{}
	// Baseline records dirty=true (already dirty going in).
	b := progressBaseline{gitHeadSHA: sha, workingTreeDirty: true}

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{}, logFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progress {
		t.Error("expected progress=false when baseline was already dirty")
	}
	lines := getLines()
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "has_progress=false") {
		t.Errorf("log line should contain has_progress=false: %q", lines[0])
	}
	if !strings.Contains(lines[0], "baseline already dirty") {
		t.Errorf("log line should mention baseline guard: %q", lines[0])
	}
}

// --- isWorkingTreeDirty tests ---

func TestIsWorkingTreeDirty_CleanRepo(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	dirty, err := isWorkingTreeDirty(repoDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dirty {
		t.Error("expected clean repo to report dirty=false")
	}
}

func TestIsWorkingTreeDirty_WithUncommittedFile(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	if err := os.WriteFile(filepath.Join(repoDir, "changes.go"), []byte("package foo\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dirty, err := isWorkingTreeDirty(repoDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dirty {
		t.Error("expected repo with uncommitted file to report dirty=true")
	}
}

func TestIsWorkingTreeDirty_EngineManagedOnly(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)

	// Create a .fabrik-context/ file — should be ignored.
	contextDir := filepath.Join(repoDir, ".fabrik-context")
	if err := os.MkdirAll(contextDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "issue.md"), []byte("# Issue\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dirty, err := isWorkingTreeDirty(repoDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dirty {
		t.Error("engine-managed files should not cause dirty=true")
	}
}

