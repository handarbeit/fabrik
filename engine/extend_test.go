package engine

import (
	"context"
	"os/exec"
	"testing"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
)

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
		progress, err := detectProgress(ctx, stage, &item, b, "/irrelevant", &mockGitHubClient{})
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

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{})
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

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, &mockGitHubClient{})
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

	progress, err := detectProgress(ctx, stage, &item, b, "/irrelevant", client)
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

	progress, err := detectProgress(ctx, stage, &item, b, "/irrelevant", client)
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

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, client)
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

	progress, err := detectProgress(ctx, stage, &item, b, repoDir, client)
	if err == nil {
		t.Fatal("expected error when FetchItemDetails fails")
	}
	if progress {
		t.Error("expected no progress on fetch error")
	}
}

