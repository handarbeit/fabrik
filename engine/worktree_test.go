package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// initBareRepo creates a minimal git repo with one commit in a temp dir.
func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}
	return dir
}

func TestNewWorktreeManager(t *testing.T) {
	wm := NewWorktreeManager("/some/repo")
	if wm.baseDir != "/some/repo" {
		t.Errorf("baseDir = %q", wm.baseDir)
	}
	if wm.rootDir != "/some/repo/.fabrik/worktrees" {
		t.Errorf("rootDir = %q", wm.rootDir)
	}
}

func TestWorktreeDir(t *testing.T) {
	wm := NewWorktreeManager("/repo")
	dir := wm.WorktreeDir(42)
	if !strings.HasSuffix(dir, "issue-42") {
		t.Errorf("WorktreeDir(42) = %q", dir)
	}
}

func TestEnsureWorktree_CreatesAndReturns(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	wtDir, err := wm.EnsureWorktree(99, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	// Check the worktree directory exists
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatal("worktree directory not created")
	}

	// Check the branch is correct
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = wtDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("checking branch: %v", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch != "fabrik/issue-99" {
		t.Errorf("branch = %q, want fabrik/issue-99", branch)
	}
}

func TestEnsureWorktree_Idempotent(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	dir1, err := wm.EnsureWorktree(1, "main", false)
	if err != nil {
		t.Fatalf("first EnsureWorktree: %v", err)
	}

	dir2, err := wm.EnsureWorktree(1, "main", false)
	if err != nil {
		t.Fatalf("second EnsureWorktree: %v", err)
	}

	if dir1 != dir2 {
		t.Errorf("idempotent call returned different dirs: %q vs %q", dir1, dir2)
	}
}

func TestCleanupWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	wtDir, err := wm.EnsureWorktree(5, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if err := wm.CleanupWorktree(5, true); err != nil {
		t.Fatalf("CleanupWorktree: %v", err)
	}

	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Error("worktree directory should be removed")
	}

	// Branch should be deleted
	if wm.branchExists("fabrik/issue-5") {
		t.Error("branch should be deleted")
	}
}

func TestCleanupWorktree_KeepBranch(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	_, err := wm.EnsureWorktree(7, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}

	if err := wm.CleanupWorktree(7, false); err != nil {
		t.Fatalf("CleanupWorktree: %v", err)
	}

	// Branch should still exist
	if !wm.branchExists("fabrik/issue-7") {
		t.Error("branch should still exist when deleteBranch=false")
	}
}

func TestDefaultBaseBranch_Main(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// The default init branch is typically "main" in modern git
	branch := wm.DefaultBaseBranch()
	if branch != "main" && branch != "master" {
		t.Errorf("DefaultBaseBranch = %q, expected main or master", branch)
	}
}

func TestBranchExists(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Default branch should exist
	defBranch := wm.DefaultBaseBranch()
	if !wm.branchExists(defBranch) {
		t.Errorf("expected %q to exist", defBranch)
	}

	// Nonexistent branch
	if wm.branchExists("fabrik/nonexistent-branch-xyz") {
		t.Error("expected nonexistent branch to not exist")
	}
}

func TestEnsureWorktree_StaleDirectoryPreserved(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Create a stale directory (not a git worktree)
	wtDir := wm.WorktreeDir(50)
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write a dummy file so it's not empty
	os.WriteFile(filepath.Join(wtDir, "dummy"), []byte("stale"), 0644)

	// EnsureWorktree should preserve the stale dir (may contain partial work)
	resultDir, err := wm.EnsureWorktree(50, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree with stale dir: %v", err)
	}
	if resultDir != wtDir {
		t.Errorf("dir = %q, want %q", resultDir, wtDir)
	}

	// Verify the dummy file still exists (not destroyed)
	if _, err := os.Stat(filepath.Join(resultDir, "dummy")); err != nil {
		t.Error("stale directory contents should be preserved")
	}
}

func TestBranchName(t *testing.T) {
	wm := NewWorktreeManager("/repo")
	if name := wm.branchName(42); name != "fabrik/issue-42" {
		t.Errorf("branchName(42) = %q", name)
	}
}

func TestCleanupWorktree_NonexistentWorktree(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Cleaning up a worktree that doesn't exist should error
	err := wm.CleanupWorktree(999, false)
	if err == nil {
		t.Error("expected error for cleaning up nonexistent worktree")
	}
}

func TestDefaultBaseBranch_FallbackToMaster(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	// Init with master branch
	cmds := [][]string{
		{"git", "init", "-b", "master"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}

	wm := NewWorktreeManager(dir)
	branch := wm.DefaultBaseBranch()
	if branch != "master" {
		t.Errorf("DefaultBaseBranch = %q, want master", branch)
	}
}

func TestDefaultBaseBranch_NeitherMainNorMaster(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "develop"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v: %s: %v", args, out, err)
		}
	}

	wm := NewWorktreeManager(dir)
	branch := wm.DefaultBaseBranch()
	// Should fall back to "main" when neither main nor master exists
	if branch != "main" {
		t.Errorf("DefaultBaseBranch = %q, want main (fallback)", branch)
	}
}

func TestDefaultBaseBranch_WithOriginHead(t *testing.T) {
	skipIfNoGit(t)

	// Create a bare "remote" repo with an initial commit
	remoteDir := t.TempDir()
	remoteCmds := [][]string{
		{"git", "init", "--bare"},
	}
	for _, args := range remoteCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = remoteDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup remote %v: %s: %v", args, out, err)
		}
	}

	// Create a temporary repo, commit, and push to remote
	tmpDir := t.TempDir()
	setupCmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "remote", "add", "origin", remoteDir},
		{"git", "commit", "--allow-empty", "-m", "initial"},
		{"git", "push", "-u", "origin", "HEAD"},
	}
	for _, args := range setupCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("setup %v: %s: %v", args, out, err)
		}
	}

	// Clone into the actual test directory (so origin/HEAD is set)
	localDir := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "clone", remoteDir, localDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("clone failed: %s: %v", out, err)
	}

	wm := NewWorktreeManager(localDir)
	branch := wm.DefaultBaseBranch()
	if branch != "main" && branch != "master" {
		t.Errorf("DefaultBaseBranch with origin = %q", branch)
	}
}

func TestEnsureWorktree_ExistingBranch(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)

	// Pre-create the branch
	cmd := exec.Command("git", "branch", "fabrik/issue-20", "main")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating branch: %s: %v", out, err)
	}

	// EnsureWorktree should use existing branch
	wtDir, err := wm.EnsureWorktree(20, "main", false)
	if err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatal("worktree dir not created")
	}
}
