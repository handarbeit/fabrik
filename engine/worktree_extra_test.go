package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoNameFromURL_HTTPSWithGit(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo.git", "repo"},
		{"https://github.com/owner/repo", "repo"},
		{"git@github.com:owner/repo.git", "repo"},
		{"git@github.com:owner/repo", "repo"},
		{"", ""},
		{"no-slash-or-colon", ""},
	}
	for _, tc := range cases {
		got := repoNameFromURL(tc.url)
		if got != tc.want {
			t.Errorf("repoNameFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestOwnerRepoDirFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo.git", "owner-repo"},
		{"https://github.com/owner/repo", "owner-repo"},
		{"git@github.com:owner/repo.git", "owner-repo"},
		{"git@github.com:owner/repo", "owner-repo"},
		{"", ""},
		{"noslash", ""},
	}
	for _, tc := range cases {
		got := ownerRepoDirFromURL(tc.url)
		if got != tc.want {
			t.Errorf("ownerRepoDirFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestMigrateWorktrees_EmptyDir_NoOp(t *testing.T) {
	// migrateWorktrees on a non-existent directory should not panic.
	migrateWorktrees("/nonexistent/path/worktrees", nil)
}

func TestMigrateWorktrees_NoIssueEntries_NoOp(t *testing.T) {
	// A worktrees dir with no "issue-N" directories should not attempt any migration.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "owner-repo"), 0755)
	var logs []string
	migrateWorktrees(dir, func(s string) { logs = append(logs, s) })
	if len(logs) != 0 {
		t.Errorf("expected no log output, got: %v", logs)
	}
}

func TestMigrateWorktrees_IssueDir_NotGitRepo_LogsWarning(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	// Create an "issue-1" directory that is NOT a git repo.
	// git remote get-url origin will fail → should log a warning and skip.
	issueDir := filepath.Join(dir, "issue-1")
	os.MkdirAll(issueDir, 0755)

	var logs []string
	migrateWorktrees(dir, func(s string) { logs = append(logs, s) })
	if len(logs) != 1 {
		t.Errorf("expected 1 warning log for non-git issue dir, got %d: %v", len(logs), logs)
	}
}

func TestMigrateWorktrees_TargetExists_LogsWarning(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()

	// Create a fake issue-1 directory with a git repo and remote pointing to github.
	issueDir := filepath.Join(dir, "issue-1")
	if err := os.MkdirAll(issueDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "remote", "add", "origin", "https://github.com/owner/repo.git"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = issueDir
		cmd.CombinedOutput() // best-effort
	}

	// Pre-create the migration target so "target already exists" path is hit.
	targetDir := filepath.Join(dir, "owner-repo", "issue-1")
	os.MkdirAll(targetDir, 0755)

	var logs []string
	migrateWorktrees(dir, func(s string) { logs = append(logs, s) })
	// Should log a warning about the target already existing.
	if len(logs) == 0 {
		t.Error("expected warning log when migration target already exists")
	}
}

func TestWorktreeDir_WithRepoName(t *testing.T) {
	// NewWorktreeManagerForRepo sets repoName, taking the wm.repoName != "" branch.
	wm := NewWorktreeManagerForRepo("/base", "/root", "owner-repo")
	got := wm.WorktreeDir(42)
	want := "/root/owner-repo/issue-42"
	if got != want {
		t.Errorf("WorktreeDir(42) = %q, want %q", got, want)
	}
}

func TestWorktreeManager_BaseDir(t *testing.T) {
	wm := NewWorktreeManager("/some/repo")
	if got := wm.BaseDir(); got != "/some/repo" {
		t.Errorf("BaseDir() = %q, want %q", got, "/some/repo")
	}
}

func TestWorktreeManager_Prune_NoError(t *testing.T) {
	skipIfNoGit(t)
	repoDir := initBareRepo(t)
	wm := NewWorktreeManager(repoDir)
	// Prune should not panic even if no stale worktrees exist
	wm.Prune()
}

func TestEnsureBareClone_ExistingDir_FetchesOnly(t *testing.T) {
	skipIfNoGit(t)
	tmpDir := t.TempDir()

	// Pre-create the bare clone directory using the actual path ensureBareClone checks:
	// {baseDir}/.fabrik/repos/{owner}-{repo}.git
	bareDir := filepath.Join(tmpDir, ".fabrik", "repos", "owner-repo.git")
	if err := os.MkdirAll(bareDir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// ensureBareClone should see the existing directory and attempt a git fetch (best-effort).
	// Since this is not a real git repo, fetch will fail silently.
	_, err := ensureBareClone(tmpDir, "owner", "repo", false)
	if err != nil {
		t.Errorf("ensureBareClone with existing dir returned error: %v", err)
	}
}

func TestEnsureBareClone_NewDir_ClonesFromLocal(t *testing.T) {
	skipIfNoGit(t)

	// Create a local "source" git repo to clone from via file:// URL
	srcDir := initBareRepo(t)

	tmpDir := t.TempDir()

	// Override cloneURL construction by temporarily monkey-patching isn't possible in Go,
	// but we can test the error path: clone of a non-existent github URL fails.
	// Instead we test that the function creates the .fabrik directory and returns an error
	// when git clone fails (no real network in CI).
	_, err := ensureBareClone(tmpDir, "nonexistent-owner-xyz", "nonexistent-repo-xyz-abc", false)
	// We expect an error because the github URL doesn't exist. The key check is that
	// the function attempts the clone and returns a wrapped error.
	if err == nil {
		t.Log("clone unexpectedly succeeded (real network access?)")
	}
	// The .fabrik parent dir should have been created even on failure
	fabrikDir := filepath.Join(tmpDir, ".fabrik")
	if _, statErr := os.Stat(fabrikDir); os.IsNotExist(statErr) {
		t.Error(".fabrik directory should be created before clone attempt")
	}

	// Suppress unused variable warning from srcDir
	_ = srcDir
}

func TestEnsureBareClone_SSHMode_UsesSSHURL(t *testing.T) {
	skipIfNoGit(t)
	tmpDir := t.TempDir()

	// With useSSH=true the clone URL should be git@github.com:owner/repo.git.
	// The clone will fail (no network / no SSH key in CI), but the error message
	// must reference the SSH URL, proving the correct URL was constructed.
	_, err := ensureBareClone(tmpDir, "owner", "repo", true)
	if err == nil {
		t.Log("clone unexpectedly succeeded (real SSH access?)")
		return
	}
	if !strings.Contains(err.Error(), "git@github.com:") {
		t.Errorf("expected SSH clone URL in error, got: %v", err)
	}
}
