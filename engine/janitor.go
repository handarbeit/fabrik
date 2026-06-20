package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// janitorWMEntry maps a "owner-repo" directory name to its resolved state.
type janitorWMEntry struct {
	ownerRepo string
	wm        *WorktreeManager
}

// runWorktreeJanitor scans .fabrik/worktrees/<owner-repo>/issue-N/ directories
// and reaps worktrees whose issues are provably terminal and whose directories
// are clean. Conservative: when in doubt, leaves the worktree and logs the reason.
//
// Reaping criteria (all must hold):
//   (a) issue is closed
//   (b) issue is NOT on the configured project board — OR is on the board at a
//       cleanup_worktree:true stage AND carries stage:<Name>:complete AND has no
//       in-flight worker per the store
//   (c) worktree is clean (git status --porcelain returns no non-engine-managed paths)
//   (d) no in-flight dispatch is registered for (repo, number) in the store
func (e *Engine) runWorktreeJanitor(ctx context.Context) {
	worktreesRoot := filepath.Join(e.fabrikDir, ".fabrik", "worktrees")

	// Build reverse map: "owner-repo" dir name → ownerRepo string + WorktreeManager.
	// Held only long enough to copy; the rest of the cycle is lock-free.
	e.mu.Lock()
	wmByDirName := make(map[string]janitorWMEntry, len(e.worktreeManagers))
	for ownerRepo, wm := range e.worktreeManagers {
		owner, repo := parseOwnerRepo(ownerRepo)
		dirName := owner + "-" + repo
		wmByDirName[dirName] = janitorWMEntry{ownerRepo: ownerRepo, wm: wm}
	}
	e.mu.Unlock()

	closedCache := make(map[string]bool) // "owner/repo#N" → isClosed; reset each cycle
	var scanned, reaped int
	var skipReasons []string

	ownerRepoDirs, err := os.ReadDir(worktreesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			e.logf(0, "janitor", "cycle complete: scanned 0 worktrees, reaped 0, skipped 0\n")
			return
		}
		e.logf(0, "janitor", "warn: cannot read worktrees root %s: %v\n", worktreesRoot, err)
		return
	}

	for _, orDir := range ownerRepoDirs {
		if !orDir.IsDir() {
			continue
		}
		dirName := orDir.Name()

		ownerRepo, wm := e.janitorResolveOwnerRepo(ctx, dirName, wmByDirName, worktreesRoot)
		if ownerRepo == "" {
			e.logf(0, "janitor", "warn: cannot resolve owner/repo for dir %s — skipping\n", dirName)
			continue
		}
		owner, repo := parseOwnerRepo(ownerRepo)

		issueDirs, err := os.ReadDir(filepath.Join(worktreesRoot, dirName))
		if err != nil {
			e.logf(0, "janitor", "warn: cannot read dir %s: %v — skipping\n", dirName, err)
			continue
		}

		for _, issueDir := range issueDirs {
			if !issueDir.IsDir() {
				continue
			}
			issueNumber := janitorParseIssueN(issueDir.Name())
			if issueNumber == 0 {
				continue
			}
			scanned++
			wtPath := filepath.Join(worktreesRoot, dirName, issueDir.Name())

			// FR-4(d): no in-flight worker — cheapest check, no I/O.
			// A worker whose heartbeat is stale AND whose PID is dead is treated as
			// absent so the janitor does not block indefinitely on crashed workers.
			snap, snapErr := e.store.Get(ownerRepo, issueNumber)
			if snapErr == nil && snap.Worker() != nil && !isWorkerStale(snap.Worker(), e.workerStaleTimeout()) {
				skipReasons = append(skipReasons, "in-flight worker")
				e.logf(0, "janitor", "skip #%d in %s: in-flight worker\n", issueNumber, ownerRepo)
				continue
			}

			// FR-4(a): issue is closed
			if !e.janitorIsClosed(ctx, owner, repo, issueNumber, snap, snapErr, closedCache) {
				skipReasons = append(skipReasons, "issue open")
				continue
			}

			// FR-4(b): off-board OR cleanup-complete
			if !e.janitorIsOffBoardOrCleanupComplete(snap, snapErr) {
				skipReasons = append(skipReasons, "on active board")
				continue
			}

			// FR-4(c): clean worktree
			dirty, err := isWorkingTreeDirty(wtPath)
			if err != nil {
				reason := fmt.Sprintf("cannot check dirty state: %v", err)
				skipReasons = append(skipReasons, reason)
				e.logf(0, "janitor", "skip #%d in %s: %s\n", issueNumber, ownerRepo, reason)
				continue
			}
			if dirty {
				skipReasons = append(skipReasons, "dirty worktree")
				e.logf(0, "janitor", "skip #%d in %s: dirty worktree\n", issueNumber, ownerRepo)
				continue
			}

			// All conditions met — reap via CleanupWorktree or os.RemoveAll fallback.
			cleanupWM := wm
			if cleanupWM == nil {
				// Unregistered repo: construct a temporary WM.
				bareDir := filepath.Join(e.fabrikDir, ".fabrik", "repos", dirName+".git")
				if fi, statErr := os.Stat(bareDir); statErr == nil && fi.IsDir() {
					cleanupWM = NewWorktreeManagerForRepo(bareDir, worktreesRoot, dirName)
					cleanupWM.logfFn = e.logf
				}
			}

			if cleanupWM != nil {
				if err := cleanupWM.CleanupWorktree(issueNumber, false); err != nil {
					reason := fmt.Sprintf("cleanup error: %v", err)
					skipReasons = append(skipReasons, reason)
					e.logf(0, "janitor", "warn: could not clean up worktree for #%d in %s: %v\n", issueNumber, ownerRepo, err)
					continue
				}
			} else {
				// Bare repo absent — fall back to direct removal.
				e.logf(0, "janitor", "warn: bare repo not found for %s — using os.RemoveAll for #%d\n", dirName, issueNumber)
				if err := os.RemoveAll(wtPath); err != nil {
					reason := fmt.Sprintf("removeall error: %v", err)
					skipReasons = append(skipReasons, reason)
					e.logf(0, "janitor", "warn: os.RemoveAll %s: %v\n", wtPath, err)
					continue
				}
			}
			reaped++
			e.logf(0, "janitor", "reaped worktree for #%d in %s\n", issueNumber, ownerRepo)
		}
	}

	skipped := scanned - reaped
	reasonStr := ""
	if len(skipReasons) > 0 {
		counts := make(map[string]int)
		for _, r := range skipReasons {
			counts[r]++
		}
		var parts []string
		for r, n := range counts {
			if n == 1 {
				parts = append(parts, r)
			} else {
				parts = append(parts, fmt.Sprintf("%s ×%d", r, n))
			}
		}
		reasonStr = " (reasons: " + strings.Join(parts, ", ") + ")"
	}
	e.logf(0, "janitor", "cycle complete: scanned %d worktrees, reaped %d, skipped %d%s\n",
		scanned, reaped, skipped, reasonStr)
}

// janitorParseIssueN parses "issue-N" → N; returns 0 for non-matching inputs.
func janitorParseIssueN(name string) int {
	if !strings.HasPrefix(name, "issue-") {
		return 0
	}
	n, err := strconv.Atoi(name[len("issue-"):])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// janitorResolveOwnerRepo maps a "owner-repo" directory name to "owner/repo".
// For registered repos it uses the reverse map directly. For unregistered repos
// it runs `git remote get-url origin` from the first issue subdirectory found.
// Returns ("", nil) when the repo cannot be identified.
func (e *Engine) janitorResolveOwnerRepo(ctx context.Context, dirName string, wmByDirName map[string]janitorWMEntry, worktreesRoot string) (ownerRepo string, wm *WorktreeManager) {
	if entry, ok := wmByDirName[dirName]; ok {
		return entry.ownerRepo, entry.wm
	}

	// Unregistered: try to read git remote from first issue subdirectory.
	issueDirs, err := os.ReadDir(filepath.Join(worktreesRoot, dirName))
	if err != nil || len(issueDirs) == 0 {
		return "", nil
	}
	for _, d := range issueDirs {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "issue-") {
			continue
		}
		wtPath := filepath.Join(worktreesRoot, dirName, d.Name())
		cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
		cmd.Dir = wtPath
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		remoteURL := strings.TrimSpace(string(out))
		resolvedDirName := ownerRepoDirFromURL(remoteURL)
		if resolvedDirName == "" {
			continue
		}
		// Reconstruct "owner/repo" from the dirName (or the resolved one).
		// ownerRepoDirFromURL gives "owner-repo"; we need "owner/repo".
		// Parse from the URL directly.
		u := strings.TrimSuffix(remoteURL, ".git")
		if colonIdx := strings.LastIndex(u, ":"); colonIdx >= 0 {
			if slashIdx := strings.Index(u, "/"); slashIdx < 0 || slashIdx > colonIdx {
				u = u[colonIdx+1:]
			}
		}
		parts := strings.Split(u, "/")
		if len(parts) < 2 {
			continue
		}
		owner := parts[len(parts)-2]
		repo := parts[len(parts)-1]
		if owner == "" || repo == "" {
			continue
		}
		return owner + "/" + repo, nil
	}
	return "", nil
}

// janitorIsClosed reports whether the issue is closed.
// Uses snap when available; falls back to a REST lookup when not in the store.
// Results are cached in closedCache (keyed by "owner/repo#N") for the cycle.
func (e *Engine) janitorIsClosed(ctx context.Context, owner, repo string, issueNumber int, snap itemstate.Snapshot, snapErr error, closedCache map[string]bool) bool {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, issueNumber)
	if cached, ok := closedCache[key]; ok {
		return cached
	}
	if snapErr == nil {
		result := snap.IsClosed()
		closedCache[key] = result
		return result
	}
	// REST fallback for items not in the store (off-board / never seen).
	issue, err := e.client.FetchIssue(owner, repo, issueNumber)
	if err != nil {
		e.logf(0, "janitor", "warn: cannot determine closed state for #%d in %s/%s: %v — skipping\n", issueNumber, owner, repo, err)
		closedCache[key] = false
		return false
	}
	if issue == nil {
		e.logf(0, "janitor", "warn: cannot determine closed state for #%d in %s/%s: received nil issue — skipping\n", issueNumber, owner, repo)
		closedCache[key] = false
		return false
	}
	result := issue.State == "closed"
	closedCache[key] = result
	return result
}

// runLogJanitor prunes .fabrik/logs/ by age and total size, then removes
// empty issue/repo directories. Disabled when JanitorIntervalHours == 0
// (same gate as the worktree janitor). Called from poll.go at startup and
// on every periodic janitor tick — the tick is the primary mechanism for
// long-running instances that never restart.
func (e *Engine) runLogJanitor(ctx context.Context) {
	_ = ctx // reserved for future use (graceful cancellation on long walks)
	logsRoot := filepath.Join(e.fabrikDir, ".fabrik", "logs")
	scanned, removed, bytes, ageRemoved, sizeRemoved, err := pruneLogs(
		logsRoot, e.cfg.LogRetentionDays, e.cfg.LogMaxBytes, time.Now(),
	)
	if err != nil {
		e.logf(0, "log-janitor", "warn: prune error: %v\n", err)
		return
	}
	e.logf(0, "log-janitor",
		"cycle complete: scanned %d files, removed %d (%d bytes), age=%d size=%d\n",
		scanned, removed, bytes, ageRemoved, sizeRemoved,
	)
}

// logFileEntry holds metadata for a single file under .fabrik/logs/.
type logFileEntry struct {
	path  string
	mtime time.Time
	size  int64
}

// pruneLogs walks root, deletes files older than retentionDays (when > 0),
// then deletes oldest files until total size is under maxBytes (when > 0),
// and finally removes any now-empty subdirectories. The now parameter
// enables deterministic testing via os.Chtimes. Returns counts of files
// scanned, files removed, bytes freed, and counts per pruning phase.
//
// Scope guard: root is resolved with filepath.Clean before any deletion; every
// candidate path is asserted to start with resolvedRoot+sep. Paths outside the
// prune root are silently skipped. If root does not exist, returns zero counts
// without error (normal first-run case).
func pruneLogs(root string, retentionDays int, maxBytes int64, now time.Time) (
	filesScanned, filesRemoved int, bytesRemoved int64, ageRemoved, sizeRemoved int, err error,
) {
	// Resolve and guard the prune root once.
	// Use filepath.Abs (not EvalSymlinks) so the same resolution strategy applies
	// to both root and individual paths — avoiding false-positive mismatches when
	// root itself is under a symlinked prefix (e.g. /tmp → /private/tmp on macOS).
	absRoot, resolveErr := filepath.Abs(root)
	if resolveErr != nil {
		absRoot = filepath.Clean(root)
	}
	// Check existence after resolving the path.
	if _, statErr := os.Stat(absRoot); statErr != nil {
		if os.IsNotExist(statErr) {
			return 0, 0, 0, 0, 0, nil
		}
	}
	rootPrefix := absRoot + string(filepath.Separator)

	var files []logFileEntry
	var dirs []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkerr error) error {
		if walkerr != nil {
			return nil // skip unreadable entries; continue walk
		}
		// Scope guard: every path must be under absRoot.
		absPath, absErr := filepath.Abs(path)
		if absErr != nil {
			absPath = filepath.Clean(path)
		}
		if absPath != absRoot && !strings.HasPrefix(absPath, rootPrefix) {
			// Path is outside the prune root (e.g. a symlink escape); skip it.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if absPath != absRoot {
				dirs = append(dirs, path)
			}
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil // skip unreadable file metadata
		}
		files = append(files, logFileEntry{path: path, mtime: info.ModTime(), size: info.Size()})
		filesScanned++
		return nil
	})
	if walkErr != nil {
		err = fmt.Errorf("walking %s: %w", root, walkErr)
		return
	}

	// Phase 1: Age prune — delete files older than the retention window.
	if retentionDays > 0 {
		cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
		var remaining []logFileEntry
		for _, f := range files {
			if f.mtime.Before(cutoff) {
				if removeErr := os.Remove(f.path); removeErr == nil {
					filesRemoved++
					bytesRemoved += f.size
					ageRemoved++
				} else {
					remaining = append(remaining, f) // deletion failed; treat as still present
				}
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}

	// Phase 2: Size-cap prune — delete oldest files first until under the cap.
	if maxBytes > 0 {
		var totalSize int64
		for _, f := range files {
			totalSize += f.size
		}
		if totalSize > maxBytes {
			// Sort ascending by mtime so we delete the oldest files first.
			sort.Slice(files, func(i, j int) bool {
				return files[i].mtime.Before(files[j].mtime)
			})
			for _, f := range files {
				if totalSize <= maxBytes {
					break
				}
				if removeErr := os.Remove(f.path); removeErr == nil {
					filesRemoved++
					bytesRemoved += f.size
					sizeRemoved++
					totalSize -= f.size
				}
			}
		}
	}

	// Phase 3: Empty-dir cleanup — walk dirs bottom-up (reverse order from WalkDir).
	// os.Remove on a non-empty directory returns an error, so only empty dirs are removed.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}

	return
}

// janitorIsOffBoardOrCleanupComplete reports whether the worktree is eligible
// for reaping based on its board state:
//   - Item not in store (off-board) → true
//   - Item at a cleanup_worktree:true stage with stage:<Name>:complete label → true
//   - Otherwise → false (on an active board stage or cleanup incomplete)
func (e *Engine) janitorIsOffBoardOrCleanupComplete(snap itemstate.Snapshot, snapErr error) bool {
	if errors.Is(snapErr, itemstate.ErrNotFound) || snapErr != nil {
		return true
	}
	status := snap.Status()
	if status == "" {
		return true
	}
	// Find the stage matching the current board column.
	var matchedStage *stages.Stage
	for _, s := range e.cfg.Stages {
		if s.Name == status {
			matchedStage = s
			break
		}
	}
	if matchedStage == nil || !matchedStage.CleanupWorktree {
		return false
	}
	// Cleanup stage found; check that it carries the completion label.
	completeLabel := "stage:" + status + ":complete"
	for _, label := range snap.Labels() {
		if label == completeLabel {
			return true
		}
	}
	return false
}
