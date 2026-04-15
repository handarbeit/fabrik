package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
)

const idleUpgradeThreshold = 2 // consecutive idle polls before checking for upgrades

// rateLimitBackoffThreshold is the fraction of GraphQL rate limit remaining
// below which the engine activates poll backoff and logs a warning.
const rateLimitBackoffThreshold = 0.20

// rateLimitMaxBackoffMultiplier caps the backoff interval as a multiple of the
// configured poll interval (e.g. 10× = 10 * PollSeconds).
const rateLimitMaxBackoffMultiplier = 10

// maxIdleBackoff is the absolute maximum poll interval during idle backoff,
// regardless of the configured poll interval.
const maxIdleBackoff = 5 * time.Minute

// idleBackoffMultiplier returns the backoff multiplier for the given idle duration.
// Schedule: 0–5min → 1x, 5–10min → 2x, 10–20min → 4x, 20+ min → 0 (use maxIdleBackoff).
func idleBackoffMultiplier(idleDuration time.Duration) int {
	switch {
	case idleDuration < 5*time.Minute:
		return 1
	case idleDuration < 10*time.Minute:
		return 2
	case idleDuration < 20*time.Minute:
		return 4
	default:
		return 0
	}
}

// computeEffectiveInterval returns the effective poll interval considering both
// idle backoff and rate-limit backoff. The result is max(idle, rateLimit),
// capped at maxIdleBackoff (5 minutes).
func computeEffectiveInterval(configuredInterval time.Duration, idleDuration time.Duration, rateLimitLow bool) time.Duration {
	idleInterval := configuredInterval
	mult := idleBackoffMultiplier(idleDuration)
	if mult == 0 {
		idleInterval = maxIdleBackoff
	} else {
		idleInterval = configuredInterval * time.Duration(mult)
	}
	if idleInterval > maxIdleBackoff {
		idleInterval = maxIdleBackoff
	}

	rateLimitInterval := configuredInterval
	if rateLimitLow {
		rateLimitInterval = configuredInterval * 2
		maxRL := configuredInterval * time.Duration(rateLimitMaxBackoffMultiplier)
		if rateLimitInterval > maxRL {
			rateLimitInterval = maxRL
		}
	}

	effective := idleInterval
	if rateLimitInterval > effective {
		effective = rateLimitInterval
	}
	if effective > maxIdleBackoff {
		effective = maxIdleBackoff
	}
	return effective
}

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

// pollLogFile is the persistent log file handle, set in Engine.Run() and used
// by pollStatus to write timestamped lines. Nil when no log file is open (tests).
var pollLogFile *os.File

// lastStatusLen tracks the length of the last overwritten status line so we
// can clear any leftover characters when the next line is shorter.
var lastStatusLen int

// pollStatus prints a transient status line that overwrites itself on a TTY.
// On non-TTY output it prints a normal line. No-op in TUI mode.
// Always writes a timestamped line to the persistent log file when one is open.
func pollStatus(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if pollLogFile != nil {
		ts := time.Now().UTC().Format(time.RFC3339)
		fmt.Fprintf(pollLogFile, "%s [poll] %s\n", ts, msg)
	}
	if tuiMode {
		return
	}
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

func (e *Engine) Run() error {
	// Acquire an exclusive file lock to prevent multiple Fabrik instances from
	// processing the same project board concurrently. The lock file lives in
	// .fabrik/ so it's scoped to the project. syscall.Flock is advisory but
	// sufficient — it's automatically released on process exit or crash.
	lockPath := filepath.Join(e.fabrikDir, ".fabrik", "fabrik.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("could not open lock file %s: %w", lockPath, err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another Fabrik instance is already running for this project (lock file: %s)", lockPath)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	// Write our PID for diagnostics (not used for locking — flock handles that).
	lockFile.Truncate(0)
	lockFile.Seek(0, 0)
	fmt.Fprintf(lockFile, "%d\n", os.Getpid())

	// Open the persistent poll log file. Truncated on each startup so the file
	// always reflects the current run only. Non-fatal if the open fails.
	logPath := filepath.Join(e.fabrikDir, ".fabrik", "fabrik.log")
	if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err != nil {
		fmt.Printf("warning: could not open log file %s: %v\n", logPath, err)
	} else {
		e.logFile = lf
		pollLogFile = lf
		defer func() {
			e.logFile = nil
			pollLogFile = nil
			lf.Close()
		}()
	}

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

	// Advisory startup checks: detect URL rewrites first so the HTTPS credential
	// helper warning can be suppressed when HTTPS GitHub URLs are transparently
	// rewritten to SSH by the user's git config.
	httpsToSSH := e.checkURLRewrite()
	e.checkHTTPSCredentials(httpsToSSH)

	// Seed all known Fabrik labels with descriptions and sensible default colors.
	// Non-fatal: a seeding failure must not prevent the engine from polling.
	stageNames := make([]string, len(e.cfg.Stages))
	for i, s := range e.cfg.Stages {
		stageNames[i] = s.Name
	}
	if err := e.client.SeedLabels(e.cfg.Owner, e.cfg.Repo, stageNames, e.cfg.User); err != nil {
		e.logf(0, "warn", "label seeding failed (non-fatal): %v\n", err)
	}

	if e.events == nil {
		fmt.Println("\nFabrik is running. Press Ctrl+C to stop.")
		fmt.Println()
	}

	configuredInterval := time.Duration(e.cfg.PollSeconds) * time.Second
	ticker := time.NewTicker(configuredInterval)
	defer ticker.Stop()

	prevMultiplier := 1
	rateLimitLow := false

	// doPollCycle runs poll(), updates idle/backoff state, emits PollCompletedEvent,
	// and resets the ticker to the effective interval. Returns the error from poll().
	doPollCycle := func() error {
		result, err := e.poll(ctx)
		if err != nil {
			return err
		}

		// Update idle timer.
		if result.Active {
			if !e.idleStart.IsZero() {
				e.logf(0, "poll", "activity detected — idle backoff reset, poll interval restored to %v\n", configuredInterval)
			}
			e.idleStart = time.Time{}
		} else if e.idleStart.IsZero() {
			e.idleStart = time.Now()
		}

		// Update rate-limit state.
		_, graphqlStats := e.client.RateLimitStats()
		if graphqlStats.Limit > 0 {
			ratio := float64(graphqlStats.Remaining) / float64(graphqlStats.Limit)
			wasLow := rateLimitLow
			rateLimitLow = ratio < rateLimitBackoffThreshold
			if rateLimitLow && !wasLow {
				e.logf(0, "warn", "GraphQL rate limit low (%.0f%% remaining) — activating rate-limit backoff\n", ratio*100)
			} else if !rateLimitLow && wasLow {
				e.logf(0, "poll", "GraphQL rate limit recovered (%.0f%% remaining)\n", ratio*100)
			}
		}

		// Compute and apply effective interval.
		var idleDuration time.Duration
		if !e.idleStart.IsZero() {
			idleDuration = time.Since(e.idleStart)
		}
		effectiveInterval := computeEffectiveInterval(configuredInterval, idleDuration, rateLimitLow)

		// Log backoff level transitions.
		mult := idleBackoffMultiplier(idleDuration)
		if mult != prevMultiplier && !e.idleStart.IsZero() {
			if mult == 0 {
				e.logf(0, "poll", "idle backoff: max (%v)\n", effectiveInterval)
			} else if mult > 1 {
				e.logf(0, "poll", "idle backoff: %dx (%v)\n", mult, effectiveInterval)
			}
			prevMultiplier = mult
		}
		if result.Active {
			prevMultiplier = 1
		}

		ticker.Reset(effectiveInterval)

		e.emitStructural(tui.PollCompletedEvent{
			ItemCount:         result.ItemCount,
			Dispatched:        result.Dispatched,
			GraphQLStats:      tui.RateLimitStats{Limit: graphqlStats.Limit, Remaining: graphqlStats.Remaining, Reset: graphqlStats.Reset},
			EffectiveInterval: effectiveInterval,
		})
		return nil
	}

	// Run immediately on start, then on tick
	if err := doPollCycle(); err != nil && ctx.Err() == nil {
		e.logf(0, "warn", "poll error: %v\n", err)
	}

	for {
		if e.wakeCh != nil {
			select {
			case <-ctx.Done():
				e.cleanupLockedIssues()
				e.wg.Wait()
				return nil
			case <-ticker.C:
				if ctx.Err() != nil {
					e.cleanupLockedIssues()
					e.wg.Wait()
					return nil
				}
				if err := doPollCycle(); err != nil {
					e.logf(0, "warn", "poll error: %v\n", err)
				}
			case <-e.wakeCh:
				e.idleStart = time.Time{}
				prevMultiplier = 1
				rateLimitLow = false
				e.logf(0, "poll", "wake requested — polling immediately\n")
				if err := doPollCycle(); err != nil {
					e.logf(0, "warn", "poll error: %v\n", err)
				}
			}
		} else {
			select {
			case <-ctx.Done():
				e.cleanupLockedIssues()
				e.wg.Wait()
				return nil
			case <-ticker.C:
				if ctx.Err() != nil {
					e.cleanupLockedIssues()
					e.wg.Wait()
					return nil
				}
				if err := doPollCycle(); err != nil {
					e.logf(0, "warn", "poll error: %v\n", err)
				}
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

// cleanupClosedIssueLocks removes fabrik:locked:<user> labels from any closed
// issues on the board. This handles stale locks left by prior Fabrik runs where
// an issue was closed while a stage was in-flight. It runs every poll cycle so
// the board stays clean without requiring a manual intervention or restart.
func (e *Engine) cleanupClosedIssueLocks(board *gh.ProjectBoard) {
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)
	for _, item := range board.Items {
		if !item.IsClosed {
			continue
		}
		hasLock := false
		for _, l := range item.Labels {
			if l == lockLabel {
				hasLock = true
				break
			}
		}
		if !hasLock {
			continue
		}
		owner, repo, num := parseIssueKey(issueKey(item, e.defaultRepo()), e.cfg.Owner, e.cfg.Repo)
		if err := e.client.RemoveLabelFromIssue(owner, repo, num, lockLabel); err != nil {
			if !errors.Is(err, gh.ErrNotFound) {
				e.logf(num, "warn", "could not remove lock label from closed issue: %v\n", err)
			}
		} else {
			e.logf(num, "poll", "removed stale lock label from closed issue\n")
		}
	}
}

// archiveDoneCompleteItems archives board items in a cleanup (Done) stage that
// have the stage:<Name>:complete label. Handles both legacy items (pre-archive
// feature) and ongoing cleanup. Uses shallow board data — labels(first:15) is
// sufficient to see stage:Done:complete even on issues with many labels.
// Idempotent: archived items disappear from subsequent board queries.
// archiveGracePeriod is how long a completed Done item stays visible on the
// board before being archived. This gives users time to see what completed
// while they were away, especially in yolo mode.
const archiveGracePeriod = 24 * time.Hour

func (e *Engine) archiveDoneCompleteItems(projectID string, items []gh.ProjectItem) {
	archived := 0
	for _, item := range items {
		st := stages.FindStage(e.cfg.Stages, item.Status)
		if st == nil || !st.CleanupWorktree {
			continue
		}
		completeLabel := fmt.Sprintf("stage:%s:complete", st.Name)
		hasComplete := false
		for _, l := range item.Labels {
			if l == completeLabel {
				hasComplete = true
				break
			}
		}
		if !hasComplete {
			continue
		}
		// Let completed items stay visible for 24 hours so users can see
		// what finished while they were away. If UpdatedAt is unknown (zero),
		// don't archive — we can't tell how old it is.
		if item.UpdatedAt.IsZero() || time.Since(item.UpdatedAt) < archiveGracePeriod {
			continue
		}
		if err := e.client.ArchiveProjectItem(projectID, item.ItemID); err != nil {
			e.logf(item.Number, "warn", "could not archive done item: %v\n", err)
			continue
		}
		archived++
	}
	if archived > 0 {
		e.logf(0, "poll", "archived %d done item(s) from board\n", archived)
	}
}

type pollResult struct {
	Active     bool
	ItemCount  int
	Dispatched int
}

func (e *Engine) poll(ctx context.Context) (pollResult, error) {
	e.emitStructural(tui.PollStartedEvent{Owner: e.cfg.Owner, Repo: e.cfg.Repo, Project: e.cfg.ProjectNum})
	e.logf(0, "poll", "fetching project board %s/%s#%d\n", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)

	board, err := e.client.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		pollStatusClear()
		return pollResult{}, err
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
		if float64(graphqlStats.Remaining)/float64(graphqlStats.Limit) < rateLimitBackoffThreshold {
			e.logf(0, "warn", "GraphQL rate limit is low (%d/%d remaining, %.0f%% threshold) — consider reducing poll frequency\n",
				graphqlStats.Remaining, graphqlStats.Limit, rateLimitBackoffThreshold*100)
		}
	}

	// Update the updatedAt cache only for items that were actually deep-fetched
	// and processed. Caching all board items would cause items that failed the
	// shallow filter (or were skipped for other reasons) to appear "unchanged"
	// on the next poll and never be retried.
	// We defer the actual cache update to after the dispatch loop so that
	// itemNeedsWork sees the OLD timestamps during this poll.
	// The deepFetchCandidates slice is populated below; the defer captures it
	// by reference so it sees the final contents.
	var deepFetchCandidates []gh.ProjectItem
	// Items advanced by the yolo catch-up loop must NOT have their updatedAt
	// re-cached — the advance changes the item's board column but not its
	// updatedAt, so re-caching would make the item look "unchanged" on the
	// next poll and prevent the new stage from running.
	advancedItems := make(map[string]bool)
	defer func() {
		e.mu.Lock()
		for _, item := range deepFetchCandidates {
			iKey := issueKey(item, e.defaultRepo())
			if !item.UpdatedAt.IsZero() && !advancedItems[iKey] {
				e.lastUpdatedAt[iKey] = item.UpdatedAt
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
			deepFetchCandidates = append(deepFetchCandidates, board.Items[i])
			continue
		}
		e.logf(0, "poll", "deep-fetching details for #%d\n", board.Items[i].Number)
		iKey := issueKey(board.Items[i], e.defaultRepo())
		if err := e.client.FetchItemDetails(&board.Items[i]); err != nil {
			e.logf(0, "warn", "could not fetch details for #%d: %v\n", board.Items[i].Number, err)
			e.mu.Lock()
			e.deepFetchFailureTime[iKey] = time.Now()
			e.mu.Unlock()
			// Skip appending to deepFetchCandidates so lastUpdatedAt is NOT updated.
			// The next poll will retry the deep-fetch for this item.
			continue
		}
		e.mu.Lock()
		delete(e.deepFetchFailureTime, iKey)
		e.mu.Unlock()
		deepFetchCandidates = append(deepFetchCandidates, board.Items[i])
		deepFetched++
	}
	if deepFetched > 0 {
		e.logf(0, "poll", "deep-fetched details for %d item(s)\n", deepFetched)
	}

	// Catch-up: auto-advance items that have fabrik:yolo or fabrik:cruise +
	// stage complete but are still sitting in the completed stage's column.
	// Operates only on deepFetchCandidates so the full label set is available.
	for _, item := range deepFetchCandidates {
		if !e.cfg.Yolo && !hasYoloLabel(item) && !hasCruiseLabel(item) {
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
		// cruise and yolo both override auto_advance:false on individual stages.
		isAutoAdvance := hasYoloLabel(item) || hasCruiseLabel(item)
		if !isAutoAdvance && stage.AutoAdvance != nil && !*stage.AutoAdvance {
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
			blocked, timedOut := e.checkReviewGate(board, item, stage)
			if blocked {
				continue // awaiting reviewers; checkReviewGate handled label
			}
			if timedOut {
				e.pauseForReviewTimeout(board, item, stage)
				continue
			}
			// Gate cleared naturally — if reviews with actionable body text were
			// submitted, re-invoke the stage agent to address the feedback before
			// advancing. Reviews with empty bodies (e.g. APPROVED with no comment)
			// have nothing to address; fall through to advance as normal.
			if syntheticComments := e.buildReviewThreadComments(item); len(syntheticComments) > 0 {
				iKey := issueKey(item, e.defaultRepo())
				// Guard: if a goroutine from a previous poll cycle is still
				// running dispatchReviewReinvoke for this item, skip the entire
				// reinvoke path — including cycle-limit checks — to avoid
				// pausing an item while valid work is still in progress. The
				// goroutine clears inFlight when it exits; the next poll will
				// re-evaluate.
				if _, ok := e.inFlight.Load(iKey); ok {
					e.logf(item.Number, "review-reinvoke", "skipping dispatch — review reinvoke already in-flight\n")
					continue
				}
				e.mu.Lock()
				cycleCount := e.reviewCycleCount[iKey]
				maxCycles := e.cfg.MaxReviewCycles
				e.mu.Unlock()
				if cycleCount >= maxCycles {
					e.pauseForReviewCycleLimit(board, item, stage, cycleCount, maxCycles)
				} else {
					e.mu.Lock()
					e.reviewCycleCount[iKey]++
					e.mu.Unlock()
					e.dispatchReviewReinvoke(ctx, board, item, stage)
					advancedItems[issueKey(item, e.defaultRepo())] = true
				}
				continue
			}
			if stage.Name == "Validate" {
				// cruise stops here: skip merge and advancement, leave for human.
				isCruiseOnly := !e.cfg.Yolo && !hasYoloLabel(item) && hasCruiseLabel(item)
				if isCruiseOnly {
					continue
				}
				if err := e.attemptMergeOnValidate(item); err != nil {
					e.logf(item.Number, "warn", "PR not merged during catch-up: %v\n", err)
					continue
				}
			}
			if newComments := e.findNewComments(item); len(newComments) > 0 {
				e.logf(item.Number, "advance", "auto-advance catch-up: skipping advance for stage %q — %d unprocessed comment(s) pending\n", stage.Name, len(newComments))
				continue
			}
			e.logf(item.Number, "advance", "auto-advance catch-up: stage %q already complete, advancing\n", stage.Name)
			if err := e.advanceToNextStage(board, item, stage); err != nil {
				e.logf(item.Number, "warn", "could not advance: %v\n", err)
			}
			// Mark as advanced so the defer doesn't re-cache the old updatedAt.
			// Board column moves don't bump updatedAt, so re-caching would
			// make the item look "unchanged" on the next poll.
			advancedItems[issueKey(item, e.defaultRepo())] = true
		}
	}

	var dispatched int
	// Dispatch only items from deepFetchCandidates — items that passed
	// itemMayNeedWork and (for non-cleanup stages) had FetchItemDetails called to
	// populate the full label set. Iterating board.Items here instead would
	// incorrectly pass shallow-label items (labels(first:5) only) to itemNeedsWork,
	// which could miss stage-complete labels beyond position 5 and re-dispatch
	// already-completed items on every poll after their updatedAt settles.
	// Check for duplicate items in deepFetchCandidates
	seenItems := make(map[string]int)
	for _, item := range deepFetchCandidates {
		k := issueKey(item, e.defaultRepo())
		seenItems[k]++
		if seenItems[k] > 1 {
			debugLog("DUPLICATE-IN-CANDIDATES", map[string]interface{}{
				"key": k, "count": seenItems[k], "number": item.Number,
			})
			e.logf(item.Number, "BUG", "item appears %d times in deepFetchCandidates\n", seenItems[k])
		}
	}

	for _, item := range deepFetchCandidates {
		item := item
		iKeyDbg := issueKey(item, e.defaultRepo())
		debugLog("dispatch-check", map[string]interface{}{
			"number": item.Number, "key": iKeyDbg, "status": item.Status,
		})
		// Full check including comments (populated by deep fetch above).
		if !e.itemNeedsWork(item) {
			debugLog("dispatch-skip-no-work", map[string]interface{}{"number": item.Number})
			continue
		}
		// Skip issues already being processed by a previous poll cycle's worker
		if _, ok := e.inFlight.Load(iKeyDbg); ok {
			debugLog("dispatch-skip-inflight", map[string]interface{}{"number": item.Number})
			continue
		}
		debugLog("dispatch-WILL-DISPATCH", map[string]interface{}{
			"number": item.Number, "key": iKeyDbg,
		})
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

	// Remove stale fabrik:locked labels from closed issues. This handles the case
	// where an issue was closed while a stage was in-flight, leaving the lock label
	// behind. We do this every poll so it also catches locks from prior Fabrik runs.
	e.cleanupClosedIssueLocks(board)

	// Archive any Done+complete items (lazy migration + ongoing cleanup).
	// Uses shallow board data — labels(first:15) is sufficient to see
	// stage:Done:complete. Idempotent: archived items disappear from board
	// results, so this converges to a no-op after legacy items are cleaned up.
	// Auto-archive disabled — too aggressive in practice, removing items
	// before users can see what completed. Re-enable once the timing logic
	// is reworked to track when the Done stage actually completed rather
	// than relying on UpdatedAt.
	// e.archiveDoneCompleteItems(board.ProjectID, board.Items)

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

	return pollResult{
		Active:     dispatched > 0 || deepFetched > 0,
		ItemCount:  len(board.Items),
		Dispatched: dispatched,
	}, nil
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

// checkAndUpgrade selects the upgrade path based on the running version:
//   - dev builds (version starts with "dev"): git pull → go build → re-exec
//   - release builds (all other versions): GitHub Releases API → download → atomic replace → re-exec
func (e *Engine) checkAndUpgrade() {
	if !strings.HasPrefix(e.cfg.Version, "dev") {
		e.checkReleaseUpgrade()
		return
	}

	dir := e.fabrikDir

	// Only auto-upgrade if we're running from a Fabrik source checkout.
	if !isFabrikSourceCheckout(dir) {
		return
	}

	baseBranch := "main"

	// Check local HEAD first — detects local commits that haven't been pushed.
	localRef, err := gitRevParse(dir, "HEAD")
	if err != nil {
		e.logf(0, "upgrade", "could not resolve HEAD: %v\n", err)
		return
	}
	binarySHA := extractBinarySHA(e.cfg.Version)
	needsRebuild := binarySHA != "" && !strings.HasPrefix(localRef, binarySHA)
	if needsRebuild {
		e.logf(0, "upgrade", "binary built from %s but HEAD is %s — rebuilding\n", binarySHA, localRef[:7])
	}

	// Also check remote for new upstream commits.
	if !needsRebuild {
		pollStatus("[upgrade] checking origin/%s ...", baseBranch)

		fetchCmd := exec.Command("git", "fetch", "origin", baseBranch)
		fetchCmd.Dir = dir
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			e.logf(0, "upgrade", "git fetch failed: %v\n%s\n", err, out)
			return
		}

		remoteRef, err := gitRevParse(dir, "origin/"+baseBranch)
		if err != nil {
			e.logf(0, "upgrade", "could not resolve origin/%s: %v\n", baseBranch, err)
			return
		}
		if localRef == remoteRef {
			pollStatusClear()
			return // up to date
		}
		// Only pull if remote is ahead of local. If local is ahead (unpushed
		// commits), we already checked the binary SHA against local HEAD above.
		mergeBaseCmd := exec.Command("git", "merge-base", "--is-ancestor", localRef, remoteRef)
		mergeBaseCmd.Dir = dir
		if err := mergeBaseCmd.Run(); err != nil {
			// localRef is not an ancestor of remoteRef — local is ahead or diverged.
			// Either way, nothing to pull. The binary SHA check above already
			// handled whether a rebuild is needed.
			pollStatusClear()
			return
		}
		needsRebuild = true
		e.logf(0, "upgrade", "new commits on origin/%s — pulling\n", baseBranch)

		pullCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
		pullCmd.Dir = dir
		if out, err := pullCmd.CombinedOutput(); err != nil {
			e.logf(0, "upgrade", "git pull --ff-only failed (local changes?): %v\n%s\n", err, out)
			return
		}
	}

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

// extractBinarySHA extracts the short SHA from a dev version string like
// "dev(abc1234)". Returns "" if the version is not a dev build or has no SHA.
func extractBinarySHA(version string) string {
	if !strings.HasPrefix(version, "dev(") || !strings.HasSuffix(version, ")") {
		return ""
	}
	return version[4 : len(version)-1]
}

// isFabrikSourceCheckout reports whether dir is a git checkout of the fabrik
// source repo (tenaciousvc/fabrik or verveguy/fabrik). Returns false on any
// error (no git, no remote, wrong remote, etc.).
func isFabrikSourceCheckout(dir string) bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	for _, pattern := range []string{"tenaciousvc/fabrik", "verveguy/fabrik"} {
		if strings.Contains(url, pattern) {
			return true
		}
	}
	return false
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

// checkHTTPSCredentials probes whether a git credential helper is configured
// when using HTTPS clone mode. Prints an advisory warning if none is found.
// Skip entirely when SSH mode is active or when HTTPS→SSH URL rewriting is in
// effect for github.com — no credential helper is needed in either case.
// This check is non-interactive and never prompts; it only reads git config.
func (e *Engine) checkHTTPSCredentials(hasSSHRewrite bool) {
	if e.cfg.GitSSH || hasSSHRewrite {
		return
	}
	cmd := exec.Command("git", "config", "credential.helper")
	out, err := cmd.Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		fmt.Printf("[startup] warn: no git credential helper configured; HTTPS cloning may prompt for credentials.\n")
		fmt.Printf("[startup] warn: configure one (e.g. git-credential-osxkeychain) or use --ssh / git_ssh: true in .fabrik/config.yaml.\n")
		fmt.Printf("[startup] note: existing bare clones retain their original remote URL and are not affected by this setting.\n")
	}
}

// checkURLRewrite detects whether git has URL rewriting configured that
// transparently redirects github.com HTTPS URLs to SSH (e.g. via
// url.git@github.com:.insteadOf = https://github.com/ in ~/.gitconfig).
// Returns true when such HTTPS→SSH rewriting is active. Prints an
// informational notice when active — git applies the rewriting transparently,
// so Fabrik's HTTPS clone URLs will automatically use SSH.
func (e *Engine) checkURLRewrite() bool {
	cmd := exec.Command("git", "config", "--get-regexp", `url\..*\.insteadOf`)
	out, _ := cmd.Output() // exit code 1 = no matches, not an error
	// Parse each line: "url.<base>.insteadof <value>"
	// Look specifically for entries that rewrite https://github.com URLs to an
	// SSH base (key contains git@github.com), avoiding false positives from
	// same-protocol or reverse (SSH→HTTPS) rewrites.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(parts[0])     // url.<base>.insteadof
		value := strings.TrimSpace(parts[1]) // the insteadOf value (URL prefix to match)
		if strings.Contains(value, "https://github.com") && strings.Contains(key, "git@github.com") {
			fmt.Printf("[startup] note: git URL rewriting for github.com is active (HTTPS → SSH); Fabrik's HTTPS clone URLs will be transparently redirected to SSH via your git config.\n")
			return true
		}
	}
	return false
}
