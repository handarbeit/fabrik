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
	mu       sync.Mutex                        // serializes worktree/branch creation (git config isn't concurrent-safe)
	baseDir  string                            // directory containing the main repo
	rootDir  string                            // where worktrees are stored (e.g., .fabrik/worktrees)
	repoName string                            // repo name used to namespace worktree paths (e.g., "develop"); empty = legacy flat layout
	logfFn   func(int, string, string, ...any) // optional; set by Engine after construction
}

// logf calls logfFn if set, otherwise prints directly.
func (wm *WorktreeManager) logf(issueNumber int, tag, format string, args ...any) {
	if wm.logfFn != nil {
		wm.logfFn(issueNumber, tag, format, args...)
		return
	}
	fmt.Printf("[#%d %s] "+format, append([]any{issueNumber, tag}, args...)...)
}

func NewWorktreeManager(repoDir string) *WorktreeManager {
	return NewWorktreeManagerWithRoot(repoDir, filepath.Join(repoDir, ".fabrik", "worktrees"))
}

func NewWorktreeManagerWithRoot(repoDir, worktreeRoot string) *WorktreeManager {
	return &WorktreeManager{
		baseDir: repoDir,
		rootDir: worktreeRoot,
	}
}

// NewWorktreeManagerForRepo creates a WorktreeManager that namespaces all worktree
// paths under worktreeRoot/<repoName>/. Use this when managing multiple repos from
// a single Fabrik job-control directory.
func NewWorktreeManagerForRepo(baseDir, worktreeRoot, rName string) *WorktreeManager {
	return &WorktreeManager{
		baseDir:  baseDir,
		rootDir:  worktreeRoot,
		repoName: rName,
	}
}

// ensureBareClone creates a bare clone of the target repo at
// .fabrik/repos/<owner>-<repo>.git if it doesn't already exist.
// Returns the path to the bare clone directory on success.
// This is used when Fabrik runs from a non-git job-control directory.
func ensureBareClone(baseDir, owner, repo string) (string, error) {
	bareDir := filepath.Join(baseDir, ".fabrik", "repos", owner+"-"+repo+".git")
	if _, err := os.Stat(bareDir); err == nil {
		// Already cloned — fetch latest
		cmd := exec.Command("git", "fetch", "origin")
		cmd.Dir = bareDir
		cmd.CombinedOutput() // best-effort
		return bareDir, nil
	}

	if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
		return "", fmt.Errorf("creating .fabrik/repos dir: %w", err)
	}

	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	cmd := exec.Command("git", "clone", "--bare", cloneURL, bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cloning %s: %s: %w", cloneURL, strings.TrimSpace(string(out)), err)
	}

	return bareDir, nil
}

// BaseDir returns the main repository directory.
func (wm *WorktreeManager) BaseDir() string {
	return wm.baseDir
}

// EnsureWorktree creates or returns the path to a worktree for the given issue.
// Each issue gets its own branch (fabrik/issue-N) and worktree directory.
// When skipUpdate is true (e.g. on retry attempts), the worktree is returned as-is
// without rebasing onto main. This avoids introducing unrelated changes mid-session.
func (wm *WorktreeManager) EnsureWorktree(issueNumber int, baseBranch string, skipUpdate bool) (string, error) {
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
				if !skipUpdate {
					wm.updateWorktreeFromMain(wtDir, baseBranch, issueNumber)
				}
				wm.writeGitExclude(wtDir, issueNumber)
				return wtDir, nil
			}
		}
		// Directory exists but git can't identify it — still usable, don't destroy it
		// The directory might have uncommitted work from a killed Claude session
		wm.logf(issueNumber, "worktree", "directory exists but branch check failed, using as-is\n")
		wm.writeGitExclude(wtDir, issueNumber)
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

	wm.logf(issueNumber, "worktree", "created %s\n", wtDir)
	wm.writeGitExclude(wtDir, issueNumber)
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
	cmd := exec.Command("git", "push", "--force-with-lease", "-u", "origin", branch)
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pushing branch %s: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	wm.logf(issueNumber, "worktree", "pushed %s to origin\n", branch)
	return nil
}

// CleanupWorktree removes the worktree and optionally the branch for an issue.
// Serialized with mu because git worktree remove writes to .git/worktrees/ metadata
// and .git/config, which are not safe to update concurrently with EnsureWorktree or PushBranch.
func (wm *WorktreeManager) CleanupWorktree(issueNumber int, deleteBranch bool) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
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

	wm.logf(issueNumber, "worktree", "cleaned up\n")
	return nil
}

// WorktreeDir returns the path to the worktree for an issue (whether it exists or not).
func (wm *WorktreeManager) WorktreeDir(issueNumber int) string {
	return wm.worktreeDir(issueNumber)
}

func (wm *WorktreeManager) worktreeDir(issueNumber int) string {
	if wm.repoName != "" {
		return filepath.Join(wm.rootDir, wm.repoName, fmt.Sprintf("issue-%d", issueNumber))
	}
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

// updateWorktreeFromMain fetches latest origin and rebases the worktree branch
// onto origin/main. This ensures stages start from an up-to-date base without
// creating noise merge commits that confuse Claude on retries.
// Errors are non-fatal — the worktree is still usable, just potentially behind.
func (wm *WorktreeManager) updateWorktreeFromMain(wtDir, baseBranch string, issueNumber int) {
	// Check for uncommitted changes — skip update if dirty.
	// Ignore .fabrik-context/ (engine-written context files) which are always
	// present but should never block a rebase. Other untracked files (e.g. new
	// source files from an interrupted Claude session) DO block the rebase
	// to avoid losing work-in-progress.
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = wtDir
	if out, err := statusCmd.Output(); err == nil {
		dirty := false
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			// Extract the file path (status is first 2 chars + space + path)
			path := strings.TrimSpace(line[2:])
			if strings.HasPrefix(path, ".fabrik-context/") || strings.HasPrefix(path, ".fabrik/issue.md") {
				continue // engine-managed, safe to ignore
			}
			dirty = true
			break
		}
		if dirty {
			wm.logf(issueNumber, "worktree", "has uncommitted changes, skipping update from main\n")
			return
		}
	}

	// Fetch latest from origin
	cmd := exec.Command("git", "fetch", "origin", baseBranch)
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		wm.logf(issueNumber, "worktree", "warn: could not fetch origin: %s\n", strings.TrimSpace(string(out)))
		return
	}

	// Rebase onto origin/main to keep a linear history and avoid merge commits
	// that introduce unrelated changes into the worktree.
	cmd = exec.Command("git", "rebase", "origin/"+baseBranch)
	cmd.Dir = wtDir
	if out, err := cmd.CombinedOutput(); err != nil {
		outStr := strings.TrimSpace(string(out))
		if strings.Contains(outStr, "CONFLICT") || strings.Contains(outStr, "could not apply") {
			// Abort the rebase and leave the branch as-is — Claude will work
			// from the current state and can be rebased in a later stage.
			abortCmd := exec.Command("git", "rebase", "--abort")
			abortCmd.Dir = wtDir
			_ = abortCmd.Run()
			wm.logf(issueNumber, "worktree", "rebase conflicts with origin/%s — staying on current base\n", baseBranch)
		} else {
			// Unknown error — abort to leave worktree in a clean state
			abortCmd := exec.Command("git", "rebase", "--abort")
			abortCmd.Dir = wtDir
			_ = abortCmd.Run()
			wm.logf(issueNumber, "worktree", "warn: could not rebase onto origin/%s: %s\n", baseBranch, outStr)
		}
		return
	}

	wm.logf(issueNumber, "worktree", "rebased onto origin/%s\n", baseBranch)
}

// writeGitExclude writes `.fabrik/` to the per-worktree git exclude file so
// context files never get accidentally staged or committed. This is idempotent —
// safe to call on every EnsureWorktree invocation.
//
// In a linked worktree, `.git` is a file pointing to the per-worktree git dir
// (e.g. <main>/.git/worktrees/issue-N/). `git rev-parse --git-dir` returns
// that absolute path, so we append `info/exclude` to it.
func (wm *WorktreeManager) writeGitExclude(wtDir string, issueNumber int) {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = wtDir
	out, err := cmd.Output()
	if err != nil {
		wm.logf(issueNumber, "warn", "could not determine git-dir for exclude setup: %v\n", err)
		return
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtDir, gitDir)
	}
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0755); err != nil {
		wm.logf(issueNumber, "warn", "could not create git info dir: %v\n", err)
		return
	}
	excludePath := filepath.Join(infoDir, "exclude")
	const entry = ".fabrik-context/\n"
	existing, _ := os.ReadFile(excludePath)
	if strings.Contains(string(existing), ".fabrik-context/") {
		return // already present
	}
	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		wm.logf(issueNumber, "warn", "could not open git exclude file: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		wm.logf(issueNumber, "warn", "could not write git exclude entry: %v\n", err)
	}
}

// migrateWorktrees scans worktreeRoot for old-style issue-N/ directories and moves
// each one to the per-repo layout <repoName>/issue-N/ using git worktree move.
// This is called once at startup before any workers dispatch.
// logfn is optional; pass nil to suppress output.
func migrateWorktrees(worktreeRoot string, logfn func(string)) {
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil {
		return // no worktrees directory yet, nothing to migrate
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Old-style entries match issue-N pattern; new-style are just repo names.
		if len(name) < 7 || name[:6] != "issue-" {
			continue
		}
		oldPath := filepath.Join(worktreeRoot, name)

		// Read the git remote to determine which repo this worktree belongs to.
		cmd := exec.Command("git", "remote", "get-url", "origin")
		cmd.Dir = oldPath
		out, err := cmd.Output()
		if err != nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: cannot read remote for worktree %s — leaving in place\n", oldPath))
			}
			continue
		}
		remoteURL := strings.TrimSpace(string(out))
		rName := repoNameFromURL(remoteURL)
		if rName == "" {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: cannot parse repo name from remote URL %q for %s — leaving in place\n", remoteURL, oldPath))
			}
			continue
		}

		// Compute new path: worktreeRoot/<repoName>/issue-N/
		newDir := filepath.Join(worktreeRoot, rName)
		newPath := filepath.Join(newDir, name)

		if _, err := os.Stat(newPath); err == nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: migration target %s already exists — skipping %s\n", newPath, oldPath))
			}
			continue
		}

		if err := os.MkdirAll(newDir, 0755); err != nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: cannot create dir %s: %v\n", newDir, err))
			}
			continue
		}

		// git worktree move requires git ≥ 2.17. If it fails, log and leave in place.
		// We run git worktree move from the worktree itself (it finds the main repo).
		moveCmd := exec.Command("git", "worktree", "move", oldPath, newPath)
		moveCmd.Dir = oldPath
		if out, err := moveCmd.CombinedOutput(); err != nil {
			if logfn != nil {
				logfn(fmt.Sprintf("warn: git worktree move %s → %s failed: %s\n",
					oldPath, newPath, strings.TrimSpace(string(out))))
			}
			continue
		}
		if logfn != nil {
			logfn(fmt.Sprintf("migrated %s → %s\n", oldPath, newPath))
		}
	}
}

// repoNameFromURL parses a git remote URL and returns just the repository name.
// Handles both HTTPS (https://github.com/owner/repo.git) and
// SSH (git@github.com:owner/repo.git) formats.
func repoNameFromURL(remoteURL string) string {
	// Strip trailing .git
	u := strings.TrimSuffix(remoteURL, ".git")
	// Find the last '/' or ':'
	lastSlash := strings.LastIndex(u, "/")
	lastColon := strings.LastIndex(u, ":")
	idx := lastSlash
	if lastColon > idx {
		idx = lastColon
	}
	if idx < 0 || idx+1 >= len(u) {
		return ""
	}
	return u[idx+1:]
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
