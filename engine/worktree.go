package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// WorktreeManager handles git worktrees for issue isolation.
type WorktreeManager struct {
	mu      sync.Mutex // serializes worktree/branch creation (git config isn't concurrent-safe)
	baseDir string     // directory containing the main repo
	rootDir string     // where worktrees are stored (e.g., .fabrik/worktrees)
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
	wm.mu.Lock()
	defer wm.mu.Unlock()

	wtDir := wm.worktreeDir(issueNumber)
	branch := wm.branchName(issueNumber)

	// If worktree directory exists, try to use it as-is
	if _, err := os.Stat(wtDir); err == nil {
		cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		cmd.Dir = wtDir
		if out, cmdErr := cmd.CombinedOutput(); cmdErr == nil {
			if strings.TrimSpace(string(out)) == branch {
				// Worktree exists and is on the right branch — update from origin
				wm.updateWorktreeFromMain(wtDir, baseBranch, issueNumber)
				return wtDir, nil
			}
		}
		// Directory exists but git can't identify it — still usable, don't destroy it
		// The directory might have uncommitted work from a killed Claude session
		logf(issueNumber, "worktree", "directory exists but branch check failed, using as-is\n")
		return wtDir, nil
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

	// Create the worktree (use -f to handle edge cases with stale registrations)
	cmd := exec.Command("git", "worktree", "add", "-f", wtDir, branch)
	cmd.Dir = wm.baseDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("creating worktree: %s: %w", string(out), err)
	}

	logf(issueNumber, "worktree", "created %s\n", wtDir)
	return wtDir, nil
}

// PushBranch pushes the issue's worktree branch to origin.
// Uses -u to set upstream tracking on the first push.
// Serialized with mu because git push -u writes upstream tracking to .git/config,
// which is not safe to update concurrently across workers.
// Errors are non-fatal (e.g., no commits yet) — the caller decides how to handle them.
func (wm *WorktreeManager) PushBranch(issueNumber int) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	wtDir := wm.worktreeDir(issueNumber)
	branch := wm.branchName(issueNumber)
	cmd := exec.Command("git", "push", "-u", "origin", branch)
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pushing branch %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	fmt.Printf("  [worktree] pushed %s to origin\n", branch)
	return nil
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

	logf(issueNumber, "worktree", "cleaned up\n")
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

// updateWorktreeFromMain fetches latest origin and merges main into the worktree branch.
// This ensures stages always start from an up-to-date base.
// Errors are non-fatal — the worktree is still usable, just potentially behind.
func (wm *WorktreeManager) updateWorktreeFromMain(wtDir, baseBranch string, issueNumber int) {
	// Check for uncommitted changes — skip update if dirty
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = wtDir
	if out, err := statusCmd.Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		logf(issueNumber, "worktree", "has uncommitted changes, skipping update from main\n")
		return
	}

	// Fetch latest from origin
	cmd := exec.Command("git", "fetch", "origin", baseBranch)
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		logf(issueNumber, "worktree", "warn: could not fetch origin: %s\n", strings.TrimSpace(string(out)))
		return
	}

	// Merge origin/main into the current branch (no-edit to avoid interactive prompts)
	cmd = exec.Command("git", "merge", "origin/"+baseBranch, "--no-edit")
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		outStr := strings.TrimSpace(string(out))
		// If merge conflicts, abort and let Claude handle it during the stage
		if strings.Contains(outStr, "CONFLICT") || strings.Contains(outStr, "Automatic merge failed") {
			logf(issueNumber, "worktree", "merge conflicts detected, aborting merge — Claude will resolve during stage\n")
			abort := exec.Command("git", "merge", "--abort")
			abort.Dir = wtDir
			_ = abort.Run()
		} else {
			logf(issueNumber, "worktree", "warn: could not merge origin/%s: %s\n", baseBranch, outStr)
		}
		return
	}

	logf(issueNumber, "worktree", "updated from origin/%s\n", baseBranch)
}

// Prune removes stale worktree registrations from git.
// Should be called once per poll cycle, before workers spawn — never during concurrent work.
func (wm *WorktreeManager) Prune() {
	cmd := exec.Command("git", "worktree", "prune")
	cmd.Dir = wm.baseDir
	_ = cmd.Run()
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
