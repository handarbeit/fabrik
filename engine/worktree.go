package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeManager handles git worktrees for issue isolation.
type WorktreeManager struct {
	baseDir string // directory containing the main repo
	rootDir string // where worktrees are stored (e.g., .fabrik/worktrees)
}

func NewWorktreeManager(repoDir string) *WorktreeManager {
	return &WorktreeManager{
		baseDir: repoDir,
		rootDir: filepath.Join(repoDir, ".fabrik", "worktrees"),
	}
}

// EnsureWorktree creates or returns the path to a worktree for the given issue.
// Each issue gets its own branch (fabrik/issue-N) and worktree directory.
func (wm *WorktreeManager) EnsureWorktree(issueNumber int, baseBranch string) (string, error) {
	wtDir := wm.worktreeDir(issueNumber)
	branch := wm.branchName(issueNumber)

	// If worktree already exists, just return the path
	if _, err := os.Stat(wtDir); err == nil {
		return wtDir, nil
	}

	// Ensure root directory exists
	if err := os.MkdirAll(wm.rootDir, 0755); err != nil {
		return "", fmt.Errorf("creating worktree root: %w", err)
	}

	// Create the branch if it doesn't exist
	if !wm.branchExists(branch) {
		cmd := exec.Command("git", "branch", branch, baseBranch)
		cmd.Dir = wm.baseDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("creating branch %s: %s: %w", branch, string(out), err)
		}
	}

	// Create the worktree
	cmd := exec.Command("git", "worktree", "add", wtDir, branch)
	cmd.Dir = wm.baseDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating worktree: %s: %w", string(out), err)
	}

	fmt.Printf("  [worktree] created %s for issue #%d\n", wtDir, issueNumber)
	return wtDir, nil
}

// CleanupWorktree removes the worktree and optionally the branch for an issue.
func (wm *WorktreeManager) CleanupWorktree(issueNumber int, deleteBranch bool) error {
	wtDir := wm.worktreeDir(issueNumber)

	// Remove the worktree
	cmd := exec.Command("git", "worktree", "remove", wtDir, "--force")
	cmd.Dir = wm.baseDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("removing worktree: %s: %w", string(out), err)
	}

	if deleteBranch {
		branch := wm.branchName(issueNumber)
		cmd := exec.Command("git", "branch", "-D", branch)
		cmd.Dir = wm.baseDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("deleting branch %s: %s: %w", branch, string(out), err)
		}
	}

	fmt.Printf("  [worktree] cleaned up for issue #%d\n", issueNumber)
	return nil
}

// WorktreeDir returns the path to the worktree for an issue (whether it exists or not).
func (wm *WorktreeManager) WorktreeDir(issueNumber int) string {
	return wm.worktreeDir(issueNumber)
}

func (wm *WorktreeManager) worktreeDir(issueNumber int) string {
	return filepath.Join(wm.rootDir, fmt.Sprintf("issue-%d", issueNumber))
}

func (wm *WorktreeManager) branchName(issueNumber int) string {
	return fmt.Sprintf("fabrik/issue-%d", issueNumber)
}

func (wm *WorktreeManager) branchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", branch)
	cmd.Dir = wm.baseDir
	out, err := cmd.CombinedOutput()
	_ = out
	return err == nil
}

// DefaultBaseBranch returns the default branch of the repo (main or master).
func (wm *WorktreeManager) DefaultBaseBranch() string {
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = wm.baseDir
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		// refs/remotes/origin/main -> main
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return "main"
}
