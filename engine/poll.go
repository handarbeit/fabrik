package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const idleUpgradeThreshold = 2

// isTTY reports whether stdout is connected to a terminal.
var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}()

// lastStatusLen tracks the length of the last overwritten status line so we
// can clear any leftover characters when the next line is shorter.
var lastStatusLen int

// pollStatus prints a transient status line that overwrites itself on a TTY.
// On non-TTY output it prints a normal line.
func pollStatus(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if isTTY {
		// Pad with spaces to clear any leftover characters from the previous line.
		pad := ""
		if len(msg) < lastStatusLen {
			pad = strings.Repeat(" ", lastStatusLen-len(msg))
		}
		fmt.Printf("\r%s%s", msg, pad)
		lastStatusLen = len(msg)
	} else {
		fmt.Println(msg)
	}
}

// pollStatusClear ends the current transient status line (if on a TTY) so that
// subsequent output starts on a fresh line.
func pollStatusClear() {
	if isTTY && lastStatusLen > 0 {
		fmt.Println()
		lastStatusLen = 0
	}
}

func gitToplevel() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (e *Engine) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	if e.cfg.ReadyCh != nil {
		close(e.cfg.ReadyCh)
	}

	fmt.Println("\nFabrik is running. Press Ctrl+C to stop.")
	fmt.Println()

	// Handle signals in a dedicated goroutine so cancel() fires immediately
	// even while poll() is blocking on wg.Wait(). This ensures CommandContext
	// kills in-flight Claude child processes without waiting for the current
	// poll cycle to finish naturally.
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Printf("\nReceived %v — shutting down gracefully (Ctrl-C again to force-quit)...\n", sig)
			cancel()
		case <-ctx.Done():
			return
		}
		// Listen for a second signal during drain and force-exit.
		select {
		case <-sigCh:
			fmt.Println("\nForce-quitting...")
			os.Exit(1)
		case <-ctx.Done():
		}
	}()

	ticker := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on tick
	if err := e.poll(ctx); err != nil && ctx.Err() == nil {
		fmt.Printf("  [warn] poll error: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			// Signal goroutine called cancel(); poll() returned because
			// CommandContext killed the child processes.
			e.cleanupLockedIssues()
			return nil
		case <-ticker.C:
			if ctx.Err() != nil {
				e.cleanupLockedIssues()
				return nil
			}
			if err := e.poll(ctx); err != nil {
				fmt.Printf("  [warn] poll error: %v\n", err)
			}
		}
	}
}

// cleanupLockedIssues removes fabrik:locked labels for any issues that were locked
// at shutdown time but never released (e.g., because the worker was killed mid-run).
func (e *Engine) cleanupLockedIssues() {
	e.mu.Lock()
	issues := make([]int, 0, len(e.lockedIssues))
	for num := range e.lockedIssues {
		issues = append(issues, num)
	}
	e.mu.Unlock()

	if len(issues) == 0 {
		return
	}
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	pollStatusClear()
	fmt.Printf("[shutdown] removing lock labels from %d issue(s)\n", len(issues))
	for _, num := range issues {
		if err := e.client.RemoveLabelFromIssue(e.cfg.Owner, e.cfg.Repo, num, lockLabel); err != nil {
			logf(num, "warn", "could not remove lock label during shutdown: %v\n", err)
		} else {
			logf(num, "shutdown", "removed lock label\n")
		}
		e.mu.Lock()
		delete(e.lockedIssues, num)
		e.mu.Unlock()
	}
}

func (e *Engine) poll(ctx context.Context) error {
	pollStatus("[poll] fetching board %s/%s#%d ...", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)

	board, err := e.client.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)
	if err != nil {
		pollStatusClear()
		return err
	}

	// Fetch status field metadata (for mutations) on first poll
	e.mu.Lock()
	if e.statusField == nil && board.ProjectID != "" {
		sf, err := e.client.FetchStatusField(board.ProjectID)
		if err != nil {
			pollStatusClear()
			fmt.Printf("  [warn] could not fetch status field: %v\n", err)
		} else {
			e.statusField = sf
		}
	}
	e.mu.Unlock()

	// Build a compact status summary with item count and rate limits.
	var rateParts []string
	restStats, graphqlStats := e.client.RateLimitStats()
	if restStats.Limit > 0 {
		rateParts = append(rateParts, fmt.Sprintf("REST %d/%d", restStats.Remaining, restStats.Limit))
	}
	if graphqlStats.Limit > 0 {
		rateParts = append(rateParts, fmt.Sprintf("GQL %d/%d", graphqlStats.Remaining, graphqlStats.Limit))
	}
	status := fmt.Sprintf("[poll] %d items", len(board.Items))
	if len(rateParts) > 0 {
		status += " | " + strings.Join(rateParts, ", ")
	}

	// Update the updatedAt cache for all items. This is done before dispatch
	// so that itemNeedsWork can compare against the previous poll's timestamps.
	// We defer the actual cache update to after the dispatch loop so that
	// itemNeedsWork sees the OLD timestamps during this poll.
	defer func() {
		e.mu.Lock()
		for _, item := range board.Items {
			if !item.UpdatedAt.IsZero() {
				e.lastUpdatedAt[item.Number] = item.UpdatedAt
			}
		}
		e.mu.Unlock()
	}()

	// Deep-fetch details (comments, linked PRs) only for items that pass the
	// shallow pre-filter. This avoids the expensive nested GraphQL cost for
	// items that can be ruled out by status, labels, or updatedAt alone.
	var deepFetched int
	for i := range board.Items {
		if !e.itemMayNeedWork(board.Items[i]) {
			continue
		}
		if err := e.client.FetchItemDetails(&board.Items[i]); err != nil {
			pollStatusClear()
			logf(board.Items[i].Number, "warn", "could not fetch item details: %v\n", err)
		}
		deepFetched++
	}
	if deepFetched > 0 {
		status += fmt.Sprintf(" | deep-fetched %d", deepFetched)
	}

	var dispatched int
	for _, item := range board.Items {
		item := item
		// Full check including comments (populated by deep fetch above).
		if !e.itemNeedsWork(item) {
			continue
		}
		// Skip issues already being processed by a previous poll cycle's worker
		if _, ok := e.inFlight.Load(item.Number); ok {
			continue
		}
		// Acquire semaphore slot, but abort if the context is cancelled so we
		// don't block indefinitely when all slots are taken at shutdown time.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			goto doneDispatching
		}
		e.inFlight.Store(item.Number, item.IsPR)
		e.wg.Add(1)
		dispatched++
		go func() {
			defer e.wg.Done()
			defer func() { <-e.sem }()
			defer e.inFlight.Delete(item.Number)
			if err := e.processItem(ctx, board, item); err != nil {
				logf(item.Number, "error", "%v\n", err)
			}
		}()
	}
doneDispatching:

	// Report cumulative token consumption only when new cost has accrued since
	// the last print, to avoid repeated log noise on idle polls.
	e.mu.Lock()
	tokens := e.totalTokens
	newCost := tokens.CostUSD > e.lastReportedCost
	if newCost {
		e.lastReportedCost = tokens.CostUSD
	}
	e.mu.Unlock()
	if newCost {
		pollStatusClear()
		fmt.Printf("[stats] cost: $%.4f | in: %d | out: %d | cache_read: %d | cache_write: %d\n",
			tokens.CostUSD, tokens.InputTokens, tokens.OutputTokens, tokens.CacheReadTokens, tokens.CacheCreationTokens)
	}

	if dispatched == 0 {
		// Check whether any workers from a previous poll cycle are still running.
		// If so, the engine is not truly idle — auto-upgrade must not run because
		// checkAndUpgrade calls syscall.Exec which would kill in-flight workers.
		var inFlightLabels []string
		e.inFlight.Range(func(key, val any) bool {
			if num, ok := key.(int); ok {
				if isPR, _ := val.(bool); isPR {
					inFlightLabels = append(inFlightLabels, fmt.Sprintf("PR#%d", num))
				} else {
					inFlightLabels = append(inFlightLabels, fmt.Sprintf("#%d", num))
				}
			}
			return true
		})

		if len(inFlightLabels) > 0 {
			status += fmt.Sprintf(" | in-flight: %v", inFlightLabels)
			pollStatus("%s", status)
			e.idleCount = 0
		} else {
			status += " | idle"
			pollStatus("%s", status)
			if e.cfg.AutoUpgrade {
				e.idleCount++
				if e.idleCount >= idleUpgradeThreshold {
					e.idleCount = 0
					e.checkAndUpgrade()
				}
			}
		}
	} else {
		// Workers dispatched — clear status line so worker logs appear cleanly.
		pollStatusClear()
		e.idleCount = 0
	}

	return nil
}

// checkAndUpgrade checks origin/main for new commits and, if found, performs a
// fast-forward pull, rebuilds the binary, and re-execs the process in place.
func (e *Engine) checkAndUpgrade() {
	baseBranch := e.worktrees.DefaultBaseBranch()
	dir := e.worktrees.BaseDir()

	pollStatus("[upgrade] checking origin/%s ...", baseBranch)

	// Fetch from origin
	fetchCmd := exec.Command("git", "fetch", "origin", baseBranch)
	fetchCmd.Dir = dir
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		pollStatusClear()
		fmt.Printf("[upgrade] git fetch failed: %v\n%s\n", err, out)
		return
	}

	// Compare HEAD to origin/baseBranch
	localRef, err := gitRevParse(dir, "HEAD")
	if err != nil {
		pollStatusClear()
		fmt.Printf("[upgrade] could not resolve HEAD: %v\n", err)
		return
	}
	remoteRef, err := gitRevParse(dir, "origin/"+baseBranch)
	if err != nil {
		pollStatusClear()
		fmt.Printf("[upgrade] could not resolve origin/%s: %v\n", baseBranch, err)
		return
	}
	if localRef == remoteRef {
		// Nothing to do — leave status line for next poll to overwrite
		return
	}

	// Real upgrade happening — print permanent output
	pollStatusClear()
	fmt.Printf("[upgrade] new commits detected — pulling origin/%s\n", baseBranch)

	pullCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
	pullCmd.Dir = dir
	if out, err := pullCmd.CombinedOutput(); err != nil {
		fmt.Printf("[upgrade] git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
		return
	}

	// Determine current executable path
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("[upgrade] could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		fmt.Printf("[upgrade] could not resolve symlinks for executable: %v\n", err)
		return
	}

	fmt.Printf("[upgrade] rebuilding binary: %s\n", exe)

	buildCmd := exec.Command("go", "build", "-o", exe, ".")
	buildCmd.Dir = dir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Printf("[upgrade] build failed: %v\n%s\n", err, out)
		return
	}

	fmt.Printf("[upgrade] re-executing new binary\n")

	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		fmt.Printf("[upgrade] exec failed: %v\n", err)
	}
}

func gitRevParse(dir, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// captureGitMeta captures the current branch name, short commit SHA, and a
// human-readable UTC timestamp from the given worktree directory.
// Returns "unknown" values gracefully if git commands fail (e.g. empty workDir).
func captureGitMeta(workDir string) (branch, commit, timestamp string) {
	timestamp = time.Now().UTC().Format("2006-01-02 15:04 UTC")

	if workDir == "" {
		return "unknown", "unknown", timestamp
	}

	sha, err := gitRevParse(workDir, "HEAD")
	if err != nil || sha == "" {
		commit = "unknown"
	} else if len(sha) >= 8 {
		commit = sha[:8]
	} else {
		commit = sha
	}

	branchCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = workDir
	out, err := branchCmd.Output()
	if err != nil {
		branch = "unknown"
	} else {
		branch = strings.TrimSpace(string(out))
	}

	return branch, commit, timestamp
}
