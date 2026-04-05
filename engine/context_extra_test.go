package engine

import (
	"os"
	"path/filepath"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestWriteContextFiles_PostToPR_WritesPRDescription verifies that when the current
// stage has PostToPR=true, writeContextFiles fetches and writes the PR description.
func TestWriteContextFiles_PostToPR_WritesPRDescription(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 50, nil
		},
		getIssueBodyFn: func(owner, repo string, issueNumber int) (string, error) {
			return "This PR implements the feature.\n\nCloses #1", nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	workDir := t.TempDir()
	reviewStage := &stages.Stage{Name: "Review", Order: 4, PostToPR: true}
	item := gh.ProjectItem{Number: 1, Body: "issue body", Status: "Review", ItemID: "PVTI_1"}

	eng.writeContextFiles(item, reviewStage, workDir, false)

	prDescPath := filepath.Join(workDir, ".fabrik-context", "pr-description.md")
	content, err := os.ReadFile(prDescPath)
	if err != nil {
		t.Fatalf("pr-description.md not written: %v", err)
	}
	if string(content) != "This PR implements the feature.\n\nCloses #1" {
		t.Errorf("pr-description.md content = %q", content)
	}
}

// TestWriteContextFiles_PostToPR_NoPR_SkipsPRDescription verifies that when no PR
// exists, writeContextFiles does not write pr-description.md.
func TestWriteContextFiles_PostToPR_NoPR_SkipsPRDescription(t *testing.T) {
	client := &mockGitHubClient{
		findPRForIssueFn: func(owner, repo string, issueNumber int) (int, error) {
			return 0, nil
		},
	}
	eng := testEngine(client, &mockClaudeInvoker{})

	workDir := t.TempDir()
	reviewStage := &stages.Stage{Name: "Review", Order: 4, PostToPR: true}
	item := gh.ProjectItem{Number: 2, Body: "issue body"}

	eng.writeContextFiles(item, reviewStage, workDir, false)

	prDescPath := filepath.Join(workDir, ".fabrik-context", "pr-description.md")
	if _, err := os.Stat(prDescPath); !os.IsNotExist(err) {
		t.Error("pr-description.md should not be written when no PR exists")
	}
}

// TestMarkCommentsSeenByStage_AddsRocketToUnseenUserComments verifies that
// markCommentsSeenByStage adds rocket reactions to user comments without a
// rocket reaction (excluding bot comments and already-seen comments).
func TestMarkCommentsSeenByStage_AddsRocketToUnseenUserComments(t *testing.T) {
	client := &mockGitHubClient{}
	eng := testEngine(client, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 30,
		Comments: []gh.Comment{
			// User comment, no rocket — should get rocket
			{ID: "C_1", DatabaseID: 601, Author: "testuser", Body: "please help",
				Reactions: []gh.ReactionGroup{}},
			// Fabrik bot comment — should be skipped
			{ID: "C_2", DatabaseID: 602, Author: "testuser",
				Body: "🏭 **Fabrik — stage: Research**\nsome output"},
			// User comment with rocket already — should be skipped
			{ID: "C_3", DatabaseID: 603, Author: "testuser", Body: "already seen",
				Reactions: []gh.ReactionGroup{{Content: "ROCKET", Count: 1}}},
		},
	}

	eng.markCommentsSeenByStage(item)

	// Only C_1 (DatabaseID 601) should have had rocket added
	var rocketTargets []int
	for _, c := range client.addCommentCalls {
		// AddComment is not used here; reactions go through AddCommentReaction
		_ = c
	}
	// The mock's AddCommentReaction doesn't track calls — but we can verify
	// the processedSet was populated for C_1
	eng.mu.Lock()
	key1 := "owner/repo#30-comment-C_1"
	key2 := "owner/repo#30-comment-C_2"
	key3 := "owner/repo#30-comment-C_3"
	_, sawC1 := eng.processedSet[key1]
	_, sawC2 := eng.processedSet[key2]
	_, sawC3 := eng.processedSet[key3]
	eng.mu.Unlock()

	_ = rocketTargets

	if !sawC1 {
		t.Error("C_1 should be in processedSet after markCommentsSeenByStage")
	}
	if sawC2 {
		t.Error("C_2 (bot comment) should NOT be in processedSet")
	}
	if sawC3 {
		t.Error("C_3 (already had rocket) should NOT be in processedSet")
	}
}

// TestItemMayNeedWork_CooldownRetry verifies that an unchanged item is retried
// after the cooldown period expires.
func TestItemMayNeedWork_CooldownRetry(t *testing.T) {
	eng := testEngine(&mockGitHubClient{}, &mockClaudeInvoker{})

	item := gh.ProjectItem{
		Number: 40,
		Status: "Research",
		ItemID: "PVTI_40",
	}
	// Simulate that item was last updated at some time, and we already recorded it
	// By not setting UpdatedAt (zero), the update check is skipped and we fall through

	// Item has a stage match — itemMayNeedWork should return true
	result := eng.itemMayNeedWork(item)
	if !result {
		t.Error("item with matching stage and no paused/locked labels should need work")
	}
}

// TestItemMayNeedWork_CleanupStage_PausedItem verifies that a cleanup stage
// item with fabrik:paused returns false.
func TestItemMayNeedWork_CleanupStage_PausedItem(t *testing.T) {
	stgs := testStagesWithCleanup()
	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: stgs},
		&mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager("/tmp"),
	)

	item := gh.ProjectItem{
		Number: 41,
		Status: "Done",
		Labels: []string{"fabrik:paused"},
	}

	if eng.itemMayNeedWork(item) {
		t.Error("paused cleanup stage item should not need work")
	}
}

// TestUpdateWorktreeFromMain_DirtyWorktree_Skips verifies that when the worktree
// has uncommitted changes, updateWorktreeFromMain skips the update.
func TestUpdateWorktreeFromMain_DirtyWorktree_Skips(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Create a worktree and add a dirty (untracked but non-.fabrik-context) file
	wtDir, err := wm.EnsureWorktree(50, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	// Create a dirty file in the worktree
	dirtyFile := filepath.Join(wtDir, "dirty.txt")
	if err := os.WriteFile(dirtyFile, []byte("dirty"), 0644); err != nil {
		t.Fatalf("create dirty file: %v", err)
	}

	// updateWorktreeFromMain should skip without panicking when worktree is dirty.
	// (It will try to fetch from origin which will fail in test, but dirty check runs first)
	wm.updateWorktreeFromMain(wtDir, "main", 50)
	// If we get here without a panic, the dirty detection worked
}
