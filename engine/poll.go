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

	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
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

// tuiMode is set to true when the TUI owns stdout (alt-screen). When true,
// pollStatus/pollStatusClear are no-ops since all output goes through the
// event channel.
var tuiMode bool

// lastStatusLen tracks the length of the last overwritten status line so we
// can clear any leftover characters when the next line is shorter.
var lastStatusLen int

// pollStatus prints a transient status line that overwrites itself on a TTY.
// On non-TTY output it prints a normal line. No-op in TUI mode.
func pollStatus(format string, args ...any) {
	if tuiMode {
		return
	}
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
// subsequent output starts on a fresh line. No-op in TUI mode.
func pollStatusClear() {
	if tuiMode {
		return
	}
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

	// Handle signals in a dedicated goroutine so cancel() fires immediately
	// even while poll() is blocking on wg.Wait(). This ensures CommandContext
	// kills in-flight Claude child processes without waiting for the current
	// poll cycle to finish naturally.
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "\nReceived %v — shutting down gracefully (Ctrl-C again to force-quit)...\n", sig)
			cancel()
		case <-ctx.Done():
			return
		}
		// Listen for a second signal during drain and force-exit.
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\nForce-quitting...")
			os.Exit(1)
		case <-ctx.Done():
		}
	}()

	// Validate stage names against project board columns before first poll.
	if err := e.checkStageColumnAlignment(ctx); err != nil {
		return err
	}

	if e.events == nil {
		fmt.Println("\nFabrik is running. Press Ctrl+C to stop.")
		fmt.Println()
	}

	ticker := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on tick
	if err := e.poll(ctx); err != nil && ctx.Err() == nil {
		e.logf(0, "warn", "poll error: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			// Signal goroutine called cancel(); poll() returned because
			// CommandContext killed the child processes.
			e.cleanupLockedIssues()
			// Wait for all worker goroutines before returning.
			// emitStructural now sends synchronously, so events are in the buffer
			// before wg.Done() fires — no separate structuralWg needed.
			e.wg.Wait()
			return nil
		case <-ticker.C:
			if ctx.Err() != nil {
				e.cleanupLockedIssues()
				e.wg.Wait()
				return nil
			}
			if err := e.poll(ctx); err != nil {
				e.logf(0, "warn", "poll error: %v\n", err)
			}
		}
	}
}

// cleanupLockedIssues removes fabrik:locked labels for any issues that were locked
// at shutdown time but never released (e.g., because the worker was killed mid-run).
func (e *Engine) cleanupLockedIssues() {
	e.mu.Lock()
	keys := make([]string, 0, len(e.lockedIssues))
	for key := range e.lockedIssues {
		keys = append(keys, key)
	}
	e.mu.Unlock()

	if len(keys) == 0 {
		return
	}
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	e.logf(0, "shutdown", "removing lock labels from %d issue(s)\n", len(keys))
	for _, key := range keys {
		// Parse "owner/repo#N" back into components for the API call.
		owner, repo, num := parseIssueKey(key, e.cfg.Owner, e.cfg.Repo)
		if err := e.client.RemoveLabelFromIssue(owner, repo, num, lockLabel); err != nil {
			e.logf(num, "warn", "could not remove lock label during shutdown: %v\n", err)
		} else {
			e.logf(num, "shutdown", "removed lock label\n")
		}
		e.mu.Lock()
		delete(e.lockedIssues, key)
		e.mu.Unlock()
	}
}

func (e *Engine) poll(ctx context.Context) error {
	e.emitStructural(tui.PollStartedEvent{Owner: e.cfg.Owner, Repo: e.cfg.Repo, Project: e.cfg.ProjectNum})
	e.logf(0, "poll", "fetching project board %s/%s#%d\n", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)

	board, err := e.client.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		pollStatusClear()
		return err
	}

	// Fetch status field metadata (for mutations) on first poll
	e.mu.Lock()
	if e.statusField == nil && board.ProjectID != "" {
		sf, err := e.client.FetchStatusField(board.ProjectID)
		if err != nil {
			e.logf(0, "warn", "could not fetch status field: %v\n", err)
		} else {
			e.statusField = sf
		}
	}
	e.mu.Unlock()

	e.logf(0, "poll", "found %d items on board\n", len(board.Items))

	// Report rate limit stats when we have seen at least one response.
	restStats, graphqlStats := e.client.RateLimitStats()
	if restStats.Limit > 0 {
		resetStr := "unknown"
		if !restStats.Reset.IsZero() {
			resetStr = restStats.Reset.Local().Format("15:04")
		}
		e.logf(0, "poll", "rate limit REST: %d/%d remaining, resets at %s\n",
			restStats.Remaining, restStats.Limit, resetStr)
	}
	if graphqlStats.Limit > 0 {
		resetStr := "unknown"
		if !graphqlStats.Reset.IsZero() {
			resetStr = graphqlStats.Reset.Local().Format("15:04")
		}
		e.logf(0, "poll", "rate limit GraphQL: %d/%d remaining, resets at %s\n",
			graphqlStats.Remaining, graphqlStats.Limit, resetStr)
	}

	// Update the updatedAt cache for all items. This is done before dispatch
	// so that itemNeedsWork can compare against the previous poll's timestamps.
	// We defer the actual cache update to after the dispatch loop so that
	// itemNeedsWork sees the OLD timestamps during this poll.
	defer func() {
		e.mu.Lock()
		for _, item := range board.Items {
			if !item.UpdatedAt.IsZero() {
				e.lastUpdatedAt[issueKey(item, e.defaultRepo())] = item.UpdatedAt
			}
		}
		e.mu.Unlock()
	}()

	// Deep-fetch details (comments, linked PRs) only for items that pass the
	// shallow pre-filter. This avoids the expensive nested GraphQL cost for
	// items that can be ruled out by status, labels, or updatedAt alone.
	// Filter repo early to avoid wasting deep-fetch API points on other repos.
	// In multi-repo mode (cfg.Repo == ""), all repos on the board are processed.
	repoFilter := ""
	if e.cfg.Repo != "" {
		repoFilter = fmt.Sprintf("%s/%s", e.cfg.Owner, e.cfg.Repo)
	}

	// Log newly discovered repos for visibility.
	seenRepos := make(map[string]bool)
	for _, it := range board.Items {
		if it.Repo != "" {
			seenRepos[it.Repo] = true
		}
	}
	if len(seenRepos) > 0 {
		repos := make([]string, 0, len(seenRepos))
		for r := range seenRepos {
			repos = append(repos, r)
		}
		e.logf(0, "poll", "repos on board: %v\n", repos)
	}

	var deepFetched int
	for i := range board.Items {
		if repoFilter != "" && board.Items[i].Repo != "" && board.Items[i].Repo != repoFilter {
			continue
		}
		if !e.itemMayNeedWork(board.Items[i]) {
			continue
		}
		// Cleanup stages don't need comments or linked-PR data — skip FetchItemDetails
		// to avoid wasting GraphQL points on items that only need a worktree existence
		// check and a completion label.
		if st := stages.FindStage(e.cfg.Stages, board.Items[i].Status); st != nil && st.CleanupWorktree {
			e.logf(0, "poll", "skipping deep-fetch for cleanup-stage item #%d\n", board.Items[i].Number)
			continue
		}
		e.logf(0, "poll", "deep-fetching details for #%d\n", board.Items[i].Number)
		if err := e.client.FetchItemDetails(&board.Items[i]); err != nil {
			e.logf(0, "warn", "could not fetch details for #%d: %v\n", board.Items[i].Number, err)
		}
		deepFetched++
	}
	if deepFetched > 0 {
		e.logf(0, "poll", "deep-fetched details for %d item(s)\n", deepFetched)
	}

	// Catch-up: auto-advance items that have fabrik:yolo + stage complete
	// but are still sitting in the completed stage's column.
	for _, item := range board.Items {
		if repoFilter != "" && item.Repo != "" && item.Repo != repoFilter {
			continue
		}
		if !e.cfg.Yolo && !hasYoloLabel(item) {
			continue
		}
		isPaused := false
		for _, l := range item.Labels {
			if l == "fabrik:paused" {
				isPaused = true
				break
			}
		}
		if isPaused {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil || stage.CleanupWorktree {
			continue
		}
		isYolo := hasYoloLabel(item)
		if !isYolo && stage.AutoAdvance != nil && !*stage.AutoAdvance {
			continue
		}
		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		hasComplete := false
		for _, l := range item.Labels {
			if l == completeLabel {
				hasComplete = true
				break
			}
		}
		if hasComplete {
			if e.checkDependencies(board, item, stage) {
				continue // blocked; checkDependencies handled label + comment
			}
			e.logf(item.Number, "advance", "yolo catch-up: stage %q already complete, advancing\n", stage.Name)
			if err := e.advanceToNextStage(board, item, stage); err != nil {
				e.logf(item.Number, "warn", "could not advance: %v\n", err)
			}
		}
	}

	var dispatched int
	for _, item := range board.Items {
		item := item
		// In single-repo mode, skip items from other repos on the same project board.
		if repoFilter != "" && item.Repo != "" && item.Repo != repoFilter {
			continue
		}
		// Full check including comments (populated by deep fetch above).
		if !e.itemNeedsWork(item) {
			continue
		}
		// Skip issues already being processed by a previous poll cycle's worker
		if _, ok := e.inFlight.Load(issueKey(item, e.defaultRepo())); ok {
			continue
		}
		// Acquire semaphore slot, but abort if the context is cancelled so we
		// don't block indefinitely when all slots are taken at shutdown time.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			goto doneDispatching
		}
		// Capture stage name, model, and start time for job tracking.
		var stageName, stageModel string
		if s := stages.FindStage(e.cfg.Stages, item.Status); s != nil {
			stageName = s.Name
			stageModel = s.Model
		}
		isComment := len(e.findNewComments(item)) > 0
		startTime := time.Now()
		iKey := issueKey(item, e.defaultRepo())
		itemRepo := itemOwnerRepoString(item, e.defaultRepo())
		e.inFlight.Store(iKey, item.IsPR)
		e.wg.Add(1)
		dispatched++
		go func() {
			defer e.wg.Done()
			defer func() { <-e.sem }()
			defer e.inFlight.Delete(iKey)
			e.emitStructural(tui.JobStartedEvent{
				IssueNumber: item.Number,
				Repo:        itemRepo,
				Title:       item.Title,
				StageName:   stageName,
				IsComment:   isComment,
				StartedAt:   startTime,
			})
			err := e.processItem(ctx, board, item)
			e.mu.Lock()
			usage := e.lastUsage[iKey]
			completed := e.lastCompleted[iKey]
			blocked := e.lastBlocked[iKey]
			delete(e.lastUsage, iKey)
			delete(e.lastCompleted, iKey)
			delete(e.lastBlocked, iKey)
			e.mu.Unlock()
			e.emitStructural(tui.JobCompletedEvent{
				IssueNumber:    item.Number,
				Repo:           itemRepo,
				Title:          item.Title,
				StageName:      stageName,
				StageModel:     stageModel,
				IsComment:      isComment,
				Success:        err == nil,
				Completed:      completed,
				BlockedOnInput: blocked,
				Duration:       time.Since(startTime),
				CompletedAt:    time.Now(),
				TurnsUsed:      usage.TurnsUsed,
				MaxTurns:       usage.MaxTurns,
				CostUSD:        usage.CostUSD,
			})
			if err != nil {
				e.logf(item.Number, "error", "%v\n", err)
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
		e.logf(0, "stats", "cost: $%.4f | in: %d | out: %d | cache_read: %d | cache_write: %d\n",
			tokens.CostUSD, tokens.InputTokens, tokens.OutputTokens, tokens.CacheReadTokens, tokens.CacheCreationTokens)
	}

	if dispatched == 0 {
		// Check whether any workers from a previous poll cycle are still running.
		// If so, the engine is not truly idle — auto-upgrade must not run because
		// checkAndUpgrade calls syscall.Exec which would kill in-flight workers.
		var inFlightLabels []string
		e.inFlight.Range(func(key, val any) bool {
			if k, ok := key.(string); ok {
				if isPR, _ := val.(bool); isPR {
					inFlightLabels = append(inFlightLabels, "PR:"+k)
				} else {
					inFlightLabels = append(inFlightLabels, k)
				}
			}
			return true
		})

		if len(inFlightLabels) > 0 {
			e.logf(0, "poll", "workers active\n")
			e.idleCount = 0
		} else {
			e.logf(0, "poll", "nothing to do\n")
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

	e.emitStructural(tui.PollCompletedEvent{ItemCount: len(board.Items), Dispatched: dispatched})
	return nil
}

// checkAndUpgrade selects the upgrade path based on the running version:
//   - dev builds (version starts with "dev"): git pull → go build → re-exec
//   - release builds (all other versions): GitHub Releases API → download → atomic replace → re-exec
func (e *Engine) checkAndUpgrade() {
	if !strings.HasPrefix(e.cfg.Version, "dev") {
		e.checkReleaseUpgrade()
		return
	}

	wm := e.devCheckout()
	if wm == nil {
		e.logf(0, "upgrade", "no Fabrik source checkout — skipping dev auto-upgrade\n")
		return
	}
	baseBranch := wm.DefaultBaseBranch()
	dir := wm.BaseDir()

	e.logf(0, "upgrade", "checking origin/%s ...\n", baseBranch)
	pollStatus("[upgrade] checking origin/%s ...", baseBranch)

	// Fetch from origin
	fetchCmd := exec.Command("git", "fetch", "origin", baseBranch)
	fetchCmd.Dir = dir
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		e.logf(0, "upgrade", "git fetch failed: %v\n%s\n", err, out)
		return
	}

	// Compare HEAD to origin/baseBranch
	localRef, err := gitRevParse(dir, "HEAD")
	if err != nil {
		e.logf(0, "upgrade", "could not resolve HEAD: %v\n", err)
		return
	}
	remoteRef, err := gitRevParse(dir, "origin/"+baseBranch)
	if err != nil {
		e.logf(0, "upgrade", "could not resolve origin/%s: %v\n", baseBranch, err)
		return
	}
	if localRef == remoteRef {
		// Nothing to do — leave status line for next poll to overwrite
		return
	}

	e.logf(0, "upgrade", "new commits detected — pulling origin/%s\n", baseBranch)

	pullCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
	pullCmd.Dir = dir
	if out, err := pullCmd.CombinedOutput(); err != nil {
		e.logf(0, "upgrade", "git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
		return
	}

	// Determine current executable path
	exe, err := os.Executable()
	if err != nil {
		e.logf(0, "upgrade", "could not determine executable path: %v\n", err)
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		e.logf(0, "upgrade", "could not resolve symlinks for executable: %v\n", err)
		return
	}

	e.logf(0, "upgrade", "rebuilding binary: %s\n", exe)

	buildCmd := exec.Command("go", "build", "-o", exe, ".")
	buildCmd.Dir = dir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		e.logf(0, "upgrade", "build failed: %v\n%s\n", err, out)
		return
	}

	// Refresh plugin skills from the new binary.
	e.logf(0, "upgrade", "refreshing plugin skills\n")
	upgradeCmd := exec.Command(exe, "upgrade")
	upgradeCmd.Dir = dir
	if out, err := upgradeCmd.CombinedOutput(); err != nil {
		e.logf(0, "upgrade", "fabrik upgrade failed: %v\n%s\n", err, out)
		// Non-fatal — continue with re-exec, old skills still work
	}

	e.logf(0, "upgrade", "re-executing new binary\n")

	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		e.logf(0, "upgrade", "exec failed: %v\n", err)
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

// checkReleaseUpgrade is the release-based upgrade path. It checks the GitHub
// Releases API for a version newer than the running binary, downloads the
// matching platform asset, atomically replaces the running binary, and re-execs.
//
// All failures are non-fatal: a warning is logged and the poll loop continues.
func (e *Engine) checkReleaseUpgrade() {
	logf := func(format string, args ...any) {
		e.logf(0, "upgrade", format, args...)
	}
	PerformReleaseUpgrade(e.client, e.cfg.Version, e.cfg.Token, []string{"FABRIK_AUTO_UPGRADED=1"}, logf)
}

// captureGitMeta captures the current branch name, short commit SHA,
// origin/{baseBranch} SHA, and a human-readable UTC timestamp from the given
// worktree directory. Returns "unknown" values gracefully if git commands fail.
// mainSHA is empty (not "unknown") when it cannot be resolved — callers treat
// empty as "no data" rather than an error sentinel.
func captureGitMeta(workDir, baseBranch string) (branch, commit, mainSHA, timestamp string) {
	timestamp = time.Now().UTC().Format("2006-01-02 15:04 UTC")

	if workDir == "" {
		return "unknown", "unknown", "", timestamp
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

	// Capture origin/{baseBranch} SHA for staleness tracking.
	// Store full SHA — it is used as a git revision in writeCodebaseChanges;
	// abbreviated SHAs can become ambiguous in larger repos.
	if baseBranch != "" {
		if mSHA, err := gitRevParse(workDir, "origin/"+baseBranch); err == nil {
			mainSHA = mSHA
		}
	}

	return branch, commit, mainSHA, timestamp
}
