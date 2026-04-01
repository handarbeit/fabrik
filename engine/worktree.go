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

	// If worktree directory exists, validate it's a proper worktree on the right branch
	if _, err := os.Stat(wtDir); err == nil {
		cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		cmd.Dir = wtDir
		if out, cmdErr := cmd.CombinedOutput(); cmdErr == nil {
			if strings.TrimSpace(string(out)) == branch {
				return wtDir, nil
			}
		}
		// Directory exists but isn't a valid worktree for expected branch — remove and recreate
		fmt.Printf("  [worktree] stale directory for issue #%d, recreating\n", issueNumber)
		_ = os.RemoveAll(wtDir)
	}

	// Ensure root directory exists
	if err := os.MkdirAll(wm.rootDir, 0755); err != nil {
		return "", fmt.Errorf("creating worktree root: %w", err)
	}

	// Create the branch if it doesn't exist, forking from origin/<base>
	if !wm.branchExists(branch) {
		// Prefer origin/<base> to handle cases where the local branch doesn't exist
		baseRef := "origin/" + baseBranch
		if !wm.branchExists(baseRef) {
			baseRef = baseBranch
		}
		cmd := exec.Command("git", "branch", branch, baseRef)
		cmd.Dir = wm.baseDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("creating branch %s from %s: %s: %w", branch, baseRef, string(out), err)
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
	cmd := exec.Command("git", "worktree", "remove", "--force", wtDir)
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
	// Try origin HEAD symref first
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = wm.baseDir
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	// Fallback: check which of main/master exists
	for _, candidate := range []string{"main", "master"} {
		if wm.branchExists(candidate) || wm.branchExists("origin/"+candidate) {
			return candidate
		}
	}
	return "main"
}
