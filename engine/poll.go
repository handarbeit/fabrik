package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
	"github.com/handarbeit/fabrik/warnings"
)

const idleUpgradeThreshold = 2 // consecutive idle polls before checking for upgrades

// rateLimitBackoffThreshold is the fraction of GraphQL rate limit remaining
// below which the engine activates poll backoff and logs a warning.
const rateLimitBackoffThreshold = 0.20

// rateLimitHealthyThreshold is the fraction of GraphQL rate limit remaining
// above which the engine clears an active rate-limit backoff. Using a higher
// threshold than rateLimitBackoffThreshold (hysteresis) prevents thrashing on
// busy boards where quota fluctuates near the activation point.
const rateLimitHealthyThreshold = 0.50

// rateLimitMaxBackoffMultiplier caps the backoff interval as a multiple of the
// configured poll interval (e.g. 10× = 10 * PollSeconds).
const rateLimitMaxBackoffMultiplier = 10

// rateLimitNearZeroPercent is the threshold (as a percentage of Limit) below
// which the previous remaining count is considered "near zero" for the
// recovery wake signal. Budget transitions from near-zero to healthy happen
// at the hourly reset boundary — not via gradual organic replenishment — so
// this guard prevents spurious wakes from ordinary hysteresis crossings.
const rateLimitNearZeroPercent = 1

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

// nextRateLimitLow applies two-threshold hysteresis to the rate-limit backoff state.
// Activate when ratio < rateLimitBackoffThreshold (20%) and not already low.
// Clear when ratio > rateLimitHealthyThreshold (50%) and currently low.
// Between the two thresholds, state is unchanged (sticky).
func nextRateLimitLow(current bool, ratio float64) bool {
	if !current && ratio < rateLimitBackoffThreshold {
		return true
	}
	if current && ratio > rateLimitHealthyThreshold {
		return false
	}
	return current
}

// isRateLimitNearZero reports whether remaining is at or near zero relative to
// limit (within rateLimitNearZeroPercent). Returns false when limit is 0.
func isRateLimitNearZero(remaining, limit int) bool {
	return limit > 0 && remaining*100 <= limit*rateLimitNearZeroPercent
}

// effectiveIdleCap returns the idle backoff cap based on webhook stream health.
// When the webhook stream is healthy or starting up, the cap is extended to
// webhookIdleCap (60 min) since the stream covers events that would otherwise
// require frequent polling. Falls back to maxIdleBackoff (5 min) when unhealthy.
func effectiveIdleCap(webhookHealthy bool) time.Duration {
	if webhookHealthy {
		return webhookIdleCap
	}
	return maxIdleBackoff
}

// computeEffectiveInterval returns the effective poll interval considering both
// idle backoff and rate-limit backoff. The result is max(idle, rateLimit).
// The idle component is capped at effectiveIdleCap(webhookHealthy); the rate-limit
// component uses its own cap (rateLimitMaxBackoffMultiplier × configured).
//
// rateLimitRatio is the remaining-to-total GraphQL quota fraction. Pass 1.0 when
// no rate-limit backoff is active; pass the actual fraction when backoff is active.
// The stepwise escalation schedule (activates when ratio < 1.0):
//
//	>=10% remaining: 2× configured  (includes sticky hysteresis zone 20%–50%)
//	>=5% and <10%:   4× configured
//	>=1% and <5%:    6× configured
//	    <1%:        10× configured  (rateLimitMaxBackoffMultiplier)
func computeEffectiveInterval(configuredInterval time.Duration, idleDuration time.Duration, rateLimitRatio float64, webhookHealthy bool) time.Duration {
	cap := effectiveIdleCap(webhookHealthy)

	var idleInterval time.Duration
	mult := idleBackoffMultiplier(idleDuration)
	if mult == 0 {
		idleInterval = cap
	} else {
		idleInterval = configuredInterval * time.Duration(mult)
	}
	if idleInterval > cap {
		idleInterval = cap
	}

	rateLimitInterval := configuredInterval
	if rateLimitRatio < 1.0 {
		var rlMult int
		switch {
		case rateLimitRatio >= 0.10:
			rlMult = 2
		case rateLimitRatio >= 0.05:
			rlMult = 4
		case rateLimitRatio >= 0.01:
			rlMult = 6
		default:
			rlMult = rateLimitMaxBackoffMultiplier
		}
		rateLimitInterval = configuredInterval * time.Duration(rlMult)
		maxRL := configuredInterval * time.Duration(rateLimitMaxBackoffMultiplier)
		if rateLimitInterval > maxRL {
			rateLimitInterval = maxRL
		}
	}

	effective := idleInterval
	if rateLimitInterval > effective {
		effective = rateLimitInterval
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

	// Emit stage-config drift warnings to both stderr (visible at startup) and
	// fabrik.log (durable for post-mortems). Without the log copy, recurrences
	// of "same drift bit us again" are invisible from the persistent log alone.
	if e.logFile != nil {
		stages.WarnStageDrift(e.cfg.Stages, e.cfg.Version, io.MultiWriter(os.Stderr, e.logFile))
	} else {
		stages.WarnStageDrift(e.cfg.Stages, e.cfg.Version, os.Stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	// restartDone is closed after performSighupRestart returns (exec failure path)
	// so the SIGHUP goroutine stays alive for the full drain window.
	restartDone := make(chan struct{})
	registerSighupHandler(ctx, cancel, e, restartDone)
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
			// Annotate all in-flight per-issue contexts with the shutdown reason so
			// the kill log emits "daemon_shutdown" rather than "context_cancel".
			e.issueCtxs.Range(func(_, val any) bool {
				val.(issueCtxEntry).holder.val.Store("daemon_shutdown")
				return true
			})
			cancel()
		case <-ctx.Done():
			return
		}
		// Listen for a second signal during drain and force-exit.
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\nForce-quitting...")
			// Release the terminal before exiting so the shell is not left in
			// alt-screen mode.
			if fn := e.cleanupHook; fn != nil {
				fn()
			}
			os.Exit(1)
		case <-ctx.Done():
		}
	}()

	// Handle TUI stop requests in a dedicated goroutine. Each stop request runs
	// handleStopRequest in its own goroutine so API calls don't block the listener.
	if e.stopCh != nil {
		go func() {
			for {
				select {
				case req, ok := <-e.stopCh:
					if !ok {
						return
					}
					go e.handleStopRequest(ctx, req)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Validate stage names against project board columns before first poll.
	if err := e.checkStageColumnAlignment(ctx); err != nil {
		return err
	}

	// Advisory startup checks: detect URL rewrites first so the HTTPS credential
	// helper warning can be suppressed when HTTPS GitHub URLs are transparently
	// rewritten to SSH by the user's git config.
	httpsToSSH := e.checkURLRewrite()
	e.checkHTTPSCredentials(httpsToSSH)
	if e.cfg.Repo != "" {
		e.checkAllowAutoMerge(e.cfg.Owner, e.cfg.Repo)
	}

	// Seed all known Fabrik labels with descriptions and sensible default colors.
	// Non-fatal: a seeding failure must not prevent the engine from polling.
	// In multi-repo mode (cfg.Repo == ""), skip here; poll() seeds each discovered repo instead.
	stageNames := make([]string, len(e.cfg.Stages))
	for i, s := range e.cfg.Stages {
		stageNames[i] = s.Name
	}
	if e.cfg.Repo != "" {
		if err := e.client.SeedLabels(e.cfg.Owner, e.cfg.Repo, stageNames, e.cfg.User); err != nil {
			e.logf(0, "warn", "label seeding failed (non-fatal): %v\n", err)
		}
		e.mu.Lock()
		e.seededRepos[e.cfg.Owner+"/"+e.cfg.Repo] = true
		e.mu.Unlock()
	}

	// In production wiring readClient is always *CacheImpl; the cast may return
	// (nil, false) when called from tests that use the pass-through GitHub adapter
	// directly via NewWithDeps. Code paths that depend on cacheImpl must check nil.
	cacheImpl, _ := e.readClient.(*boardcache.CacheImpl)

	// Register reactive observers. All returned unsubscribe funcs are collected
	// and called when Run returns. Observers on cacheImpl are gated on cacheImpl != nil;
	// observers on engine.store are always registered.
	{
		var unsubs []func()
		defer func() {
			for _, unsub := range unsubs {
				unsub()
			}
		}()

		// wakeChObserver fires on board-state changes. After store unification, the
		// shared store receives both engine-side mutations (LockChanged) and webhook/
		// reconcile-side mutations (Status/Labels/Comments/LinkedPR). Register once
		// on the shared store — no cacheImpl registration needed or allowed.
		if e.wakeCh != nil {
			wakeObs := newWakeChObserver(e.wakeCh)
			unsubs = append(unsubs, e.store.Subscribe(wakeObs))
		}

		// mayNeedWorkObserver populates e.mayNeedWork when items change. Register
		// once on the shared store; all mutation types flow through it post-unification.
		mwnObs := newMayNeedWorkObserver(&e.mayNeedWorkMu, &e.mayNeedWork)
		unsubs = append(unsubs, e.store.Subscribe(mwnObs))

		// InvocationObserver fires on InvocationRecorded mutations (engine-side).
		invObs := &InvocationObserver{Stages: e.cfg.Stages, Emit: e.emitStructural}
		unsubs = append(unsubs, e.store.Subscribe(invObs))

		// StageChangeObserver fires on StatusChanged mutations. After store unification
		// it registers on the shared store (not cacheImpl) so it sees all status changes.
		stageObs := &StageChangeObserver{Emit: e.emitStructural}
		unsubs = append(unsubs, e.store.Subscribe(stageObs))

		// PushUnblockObserver fires on StateChanged (issue close) and removes
		// fabrik:blocked from dependents whose all blockers are now resolved.
		// StateChanged is not in wakeChFlags so this registration has no effect on
		// poll-wake behaviour. Registered on e.store only (post store-unification).
		pushUnblockObs := &PushUnblockObserver{
			Store:  e.store,
			Remove: func(owner, repo string, n int) { e.removeBlockedIfResolved(owner, repo, n) },
			Logf:   func(format string, args ...any) { e.logf(0, "push-unblock", format, args...) },
		}
		unsubs = append(unsubs, e.store.Subscribe(pushUnblockObs))

		if cacheImpl != nil {
			// WebhookHealthObserver fires tui.WebhookStatusEvent on pause/resume transitions.
			// SubscribePause is a CacheImpl-level signal (stream health), not a Store mutation.
			unsubs = append(unsubs, cacheImpl.SubscribePause(func(paused bool) {
				state := "healthy"
				if paused {
					state = "unhealthy"
				}
				e.emitStructural(tui.WebhookStatusEvent{State: state})
			}))
		}
	}

	// Start webhook manager when enabled. Failures are non-fatal: the engine
	// continues in polling-only mode.
	if e.cfg.Webhooks {
		// Seed the known-repo set so the first subprocess invocation has at least
		// one --repo arg. Multi-repo boards discover additional repos via UpdateRepos
		// after the first poll, at which point the subprocess restarts with the full set.
		var initialRepos map[string]bool
		if e.cfg.Owner != "" && e.cfg.Repo != "" {
			initialRepos = map[string]bool{e.cfg.Owner + "/" + e.cfg.Repo: true}
		}
		var deltaFn func(string, []byte)
		if cacheImpl != nil {
			deltaFn = func(eventType string, payload []byte) {
				cacheImpl.ApplyDelta(eventType, payload)
				e.applyLayer1StatusRefresh(eventType, payload, cacheImpl)
			}
		}
		cleanupFn := func(repos []string) error {
			var firstErr error
			for _, r := range repos {
				owner, repo := parseOwnerRepo(r)
				if owner == "" {
					e.logf(0, "webhook", "skipping malformed repo in cleanup: %q\n", r)
					continue
				}
				if err := e.client.DeleteForwardingHooks(owner, repo); err != nil {
					e.logf(0, "webhook", "orphan hook cleanup failed for %s/%s: %v\n", owner, repo, err)
					if firstErr == nil {
						firstErr = err
					}
				}
			}
			return firstErr
		}
		wm := newWebhookManager(
			e.logf,
			e.wakeCh,
			e.emit,
			initialRepos,
			e.cfg.WebhookEvents,
			deltaFn,
			cleanupFn,
		)
		// Inject MatchEcho so ApplyDelta can clear pending echo entries on incoming webhooks.
		if cacheImpl != nil {
			cacheImpl.SetMatchEchoFn(wm.MatchEcho)
		}
		// Bootstrap the cache before accepting webhook events so no delta is
		// dropped into an empty cache during the startup window.
		// Probe-based bootstrap costs ~250 GraphQL nodes (vs ~2350 for a full
		// shallow fetch). Terminal seeding for closed cleanup-stage items happens
		// via seedTerminalFromProbeItems after bootstrap, preventing the first
		// poll from re-deep-fetching every closed Done item.
		if cacheImpl != nil {
			probeItems, projectID, probeErr := e.client.ProbeProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
			if probeErr != nil {
				e.logf(0, "cache", "startup probe failed — cache will be populated on first poll: %v\n", probeErr)
			} else if projectID != "" && len(probeItems) > 0 {
				cacheImpl.BootstrapFromProbe(probeItems, projectID)
				e.seedTerminalFromProbeItems(probeItems)
			} else if projectID != "" {
				e.logf(0, "cache", "startup probe returned 0 items — deferring to first poll\n")
			}
		}
		if err := wm.Start(ctx, e.cfg.WebhookPort); err == nil {
			e.webhookMgr = wm
			defer wm.Stop()
		}
		// NOTE: the reconcile ticker is intentionally NOT started here. It is the
		// poll-only correctness backstop and must run whether or not the webhook
		// manager started (#955) — it is launched unconditionally below.
	}

	// Reconcile ticker: the poll-only correctness backstop that re-syncs the cache
	// from GitHub on drift — notably fabrik-managed label state, whose divergence
	// (e.g. a store missing fabrik:awaiting-ci while GitHub has it) otherwise
	// strands an item at a gate forever. This MUST run independent of webhooks
	// (#955): webhooks are an optimization, not a requirement, so drift repair may
	// not be gated on the webhook manager starting. e.webhookMgr is nil when
	// webhooks are disabled or failed to start; reconcileLoop skips health-state
	// signaling in that case.
	if cacheImpl != nil {
		go e.reconcileLoop(ctx, cacheImpl, e.webhookMgr)
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
	rateLimitRatio := 1.0
	lastRemainingCount := 0

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
				e.logf(0, "poll", "activity detected — idle backoff reset\n")
			}
			e.idleStart = time.Time{}
		} else if e.idleStart.IsZero() {
			e.idleStart = time.Now()
		}

		// Update rate-limit state using two-threshold hysteresis:
		// activate when ratio drops below 20%; clear only when ratio rises above 50%.
		// Activity detection does NOT reset rate-limit backoff — it is a separate concern.
		_, graphqlStats := e.client.RateLimitStats()
		if graphqlStats.Limit > 0 {
			ratio := float64(graphqlStats.Remaining) / float64(graphqlStats.Limit)
			newRateLimitLow := nextRateLimitLow(rateLimitLow, ratio)
			if newRateLimitLow && !rateLimitLow {
				e.logf(0, "warn", "GraphQL rate limit low (%.0f%% remaining) — activating rate-limit backoff\n", ratio*100)
			} else if !newRateLimitLow && rateLimitLow {
				if isRateLimitNearZero(lastRemainingCount, graphqlStats.Limit) {
					e.logf(0, "info", "GraphQL rate limit recovered (%d/%d remaining) — triggering immediate probe\n", graphqlStats.Remaining, graphqlStats.Limit)
					if e.wakeCh != nil {
						select {
						case e.wakeCh <- struct{}{}:
						default:
						}
					}
				} else {
					e.logf(0, "poll", "GraphQL rate limit recovered (%.0f%% remaining)\n", ratio*100)
				}
				e.emitStructural(tui.RateLimitAlertEvent{Exhausted: false})
			}
			rateLimitLow = newRateLimitLow
			if rateLimitLow {
				rateLimitRatio = ratio
			} else {
				rateLimitRatio = 1.0
			}
			lastRemainingCount = graphqlStats.Remaining
		}

		// Compute and apply effective interval.
		var idleDuration time.Duration
		if !e.idleStart.IsZero() {
			idleDuration = time.Since(e.idleStart)
		}
		webhookHealthy := e.webhookMgr != nil && e.webhookMgr.IsHealthyOrStartingUp()
		if e.webhookMgr != nil && e.webhookMgr.IsDisabled() {
			e.logf(0, "webhook", "poll-only mode active — webhook subscription disabled; restart Fabrik to retry\n")
		}
		effectiveInterval := computeEffectiveInterval(configuredInterval, idleDuration, rateLimitRatio, webhookHealthy)

		// Notify webhook manager of any new repos discovered during this poll.
		if e.webhookMgr != nil && result.SeenRepos != nil {
			e.webhookMgr.UpdateRepos(result.SeenRepos)
		}

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

	// Startup upgrade check: no workers are in flight yet, making this the safest call site.
	// Skip if the context was already cancelled (e.g. SIGINT arrived during startup).
	if e.cfg.AutoUpgrade && ctx.Err() == nil {
		if e.upgradeCheckFn != nil {
			e.upgradeCheckFn()
		} else {
			e.checkAndUpgrade()
		}
	}

	// Run immediately on start, then on tick
	firstPollErr := doPollCycle()
	if firstPollErr != nil && ctx.Err() == nil {
		e.logf(0, "warn", "poll error: %v\n", firstPollErr)
	}

	// One-time startup cleanup: remove stale fabrik:locked:<user> labels from items
	// that have no active Worker in the store (restart case: prior crash left labels).
	// Only runs after a successful first poll cycle so the store is populated.
	// Skipped on poll failure to avoid scanning an empty/partial store.
	if firstPollErr == nil {
		e.runStartupCleanup()
		e.runStartupTransientLabelScan()
		e.runStartupTerminalScan()
		if e.cfg.JanitorIntervalHours > 0 {
			e.runWorktreeJanitor(ctx)
			e.runLogJanitor(ctx)
		}
	}

	// Start background stale-worker detector. Scans for workers whose heartbeat
	// has gone stale and cleans up if the process is confirmed dead via signal 0.
	e.startWorkerDetector(ctx)

	// Start periodic janitor goroutine. On each tick: reaps orphaned worktrees for
	// closed, off-board issues and prunes .fabrik/logs/ by age and total size.
	// Disabled when JanitorIntervalHours == 0.
	if e.cfg.JanitorIntervalHours > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(e.cfg.JanitorIntervalHours) * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					e.runWorktreeJanitor(ctx)
					e.runLogJanitor(ctx)
				}
			}
		}()
	}

	for {
		if e.wakeCh != nil {
			select {
			case <-ctx.Done():
				e.cleanupLockedIssues()
				e.wg.Wait()
				if e.sighupRequested.Load() {
					performSighupRestart(e, lockFile)
					close(restartDone) // reached only on exec failure
				}
				return nil
			case <-ticker.C:
				if ctx.Err() != nil {
					e.cleanupLockedIssues()
					e.wg.Wait()
					if e.sighupRequested.Load() {
						performSighupRestart(e, lockFile)
						close(restartDone) // reached only on exec failure
					}
					return nil
				}
				if err := doPollCycle(); err != nil {
					e.logf(0, "warn", "poll error: %v\n", err)
				}
			case <-e.wakeCh:
				select {
				case <-ticker.C:
				default:
				}
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
				if e.sighupRequested.Load() {
					performSighupRestart(e, lockFile)
					close(restartDone) // reached only on exec failure
				}
				return nil
			case <-ticker.C:
				if ctx.Err() != nil {
					e.cleanupLockedIssues()
					e.wg.Wait()
					if e.sighupRequested.Load() {
						performSighupRestart(e, lockFile)
						close(restartDone) // reached only on exec failure
					}
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
	snaps := e.store.All()
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)

	var locked []itemstate.Snapshot
	for _, snap := range snaps {
		if lock := snap.Lock(); lock != nil && lock.HeldByThis {
			locked = append(locked, snap)
		}
	}

	if len(locked) == 0 {
		return
	}
	e.logf(0, "shutdown", "removing lock labels from %d issue(s)\n", len(locked))
	for _, snap := range locked {
		owner, repo := parseOwnerRepo(snap.Repo())
		num := snap.Number()
		if err := e.client.RemoveLabelFromIssue(owner, repo, num, lockLabel); err != nil {
			e.logf(num, "warn", "could not remove lock label during shutdown: %v\n", err)
		} else {
			e.logf(num, "shutdown", "removed lock label\n")
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, num), lockLabel)
			}
		}
		e.store.Apply(itemstate.LocalLockReleased{Repo: snap.Repo(), Number: num})
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
		if !hasLabel(item.Labels, lockLabel) {
			continue
		}
		owner, repo, num := parseIssueKey(issueKey(item, e.defaultRepo()), e.cfg.Owner, e.cfg.Repo)
		if err := e.client.RemoveLabelFromIssue(owner, repo, num, lockLabel); err != nil {
			if !errors.Is(err, gh.ErrNotFound) {
				e.logf(num, "warn", "could not remove lock label from closed issue: %v\n", err)
			}
		} else {
			e.logf(num, "poll", "removed stale lock label from closed issue\n")
			if c := e.cache(); c != nil {
				c.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), lockLabel)
			}
		}
	}
}

// transientLifecycleLabels are the labels that must be swept from closed issues.
// They are all applied transiently during pipeline execution and have no meaning
// once an issue is closed; leaving them behind causes confusion and may trigger
// unintended catch-up-loop evaluations on reopened issues.
var transientLifecycleLabels = []string{
	"fabrik:awaiting-review",
	"fabrik:awaiting-ci",
	"fabrik:auto-merge-enabled",
	"fabrik:awaiting-input",
	"fabrik:rebase-needed",
	"fabrik:bot-reprompted",
	"fabrik:revalidate",
}

// cleanupClosedIssueTransientLabels removes transient lifecycle labels from any
// closed issues on the board. It runs every poll cycle as a defensive sweep so
// issues do not carry stale operational labels into terminal state (#617).
func (e *Engine) cleanupClosedIssueTransientLabels(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !item.IsClosed {
			continue
		}
		labelSet := make(map[string]struct{}, len(item.Labels))
		for _, l := range item.Labels {
			labelSet[l] = struct{}{}
		}
		owner, repo, num := parseIssueKey(issueKey(item, e.defaultRepo()), e.cfg.Owner, e.cfg.Repo)
		repoKey := owner + "/" + repo // use computed repo for cache key; item.Repo may be empty in fallback paths
		for _, label := range transientLifecycleLabels {
			if _, has := labelSet[label]; !has {
				continue
			}
			if err := e.client.RemoveLabelFromIssue(owner, repo, num, label); err != nil {
				if errors.Is(err, gh.ErrNotFound) {
					// Label already absent on GitHub — desired state achieved; sync cache.
					if c := e.cache(); c != nil {
						c.ApplyLabelRemoved(boardcache.ItemKey(repoKey, num), label)
					}
				} else {
					e.logf(num, "warn", "could not remove transient label %q from closed issue: %v\n", label, err)
				}
			} else {
				e.logf(num, "poll", "removed transient label %q from closed issue\n", label)
				if c := e.cache(); c != nil {
					c.ApplyLabelRemoved(boardcache.ItemKey(repoKey, num), label)
				}
			}
		}
	}
}

type pollResult struct {
	Active     bool
	ItemCount  int
	Dispatched int
	SeenRepos  map[string]bool // all repos observed on the board in this poll
}

func (e *Engine) poll(ctx context.Context) (pollResult, error) {
	e.emitStructural(tui.PollStartedEvent{Owner: e.cfg.Owner, Repo: e.cfg.Repo, Project: e.cfg.ProjectNum})
	if c := e.cache(); c != nil && c.IsBootstrapped() && !c.IsPaused() {
		e.logf(0, "poll", "reading project board from cache: %s/%s#%d\n", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)
	} else {
		e.logf(0, "poll", "fetching project board from GitHub: %s/%s#%d\n", e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum)
	}

	// Layer 2 gate: check project.updatedAt before the board read so the cache
	// holds fresh Status values when poll() processes items this cycle.
	// Only active when the in-memory cache is bootstrapped (cacheImpl != nil and
	// ProjectID is known); skipped on the very first cycle when cache is unset.
	if c := e.cache(); c != nil && c.ProjectID() != "" {
		projectID := c.ProjectID()
		updatedAt, gateErr := e.client.FetchProjectUpdatedAt(projectID)
		if gateErr != nil {
			e.logf(0, "cache", "layer2 gate: FetchProjectUpdatedAt failed: %v; running batch as fallback\n", gateErr)
		}
		if gateErr != nil || updatedAt.After(e.lastProjectUpdatedAt) {
			updates, err := e.client.FetchProjectItemStatusBatch(projectID)
			if err != nil {
				e.logf(0, "cache", "layer2 status sweep failed: %v\n", err)
			} else {
				c.ApplyStatusBatch(updates)
				if gateErr == nil {
					e.lastProjectUpdatedAt = updatedAt
				}
			}
		}
	}

	// Per-poll cache refresh: for a bootstrapped cache use the cheap probe query
	// (no labels, closedByPullRequestsReferences(first:1)) to detect which items
	// changed since the last deep-fetch, and fire FetchItemDetails only for those
	// items. This replaces the previous FetchProjectBoard + Reconcile path and
	// reduces GraphQL cost ~5-10x on idle boards.
	//
	// Virgin caches use ProbeProjectBoard + BootstrapFromProbe (~250 nodes) instead
	// of FetchProjectBoard + Bootstrap (~2350 nodes on a 47-item board). The probe
	// result carries LinkedPRNumber so the subsequent probe cycle does not see
	// spurious linkage-drift. Closed cleanup-stage items are seeded terminal so
	// they are never deep-fetched.
	//
	// Paused caches are skipped; FetchProjectBoard below falls through to GitHub
	// directly for paused caches.
	//
	// Important caveat: project Status changes do NOT flow over webhooks for
	// repo-level projects (and only sometimes for org-level projects with the
	// right permissions). The Layer 2 status sweep above must continue running
	// regardless of webhook state — it is the only delivery path for Status.
	if c := e.cache(); c != nil {
		switch {
		case c.IsPaused():
			// Paused: FetchProjectBoard below falls through to GitHub directly.
		case c.IsBootstrapped() || c.ProjectID() != "":
			// Bootstrapped: use probe-driven refresh (avoids full shallow fetch cost).
			e.runProbeAndDeepFetch(c)
		default:
			// Virgin: probe bootstrap (~250 nodes vs ~2350 for full shallow fetch).
			probeItems, projectID, refreshErr := e.client.ProbeProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
			if refreshErr != nil {
				e.logf(0, "cache", "initial board probe failed (using empty cache): %v\n", refreshErr)
			} else if projectID != "" && len(probeItems) > 0 {
				c.BootstrapFromProbe(probeItems, projectID)
				e.seedTerminalFromProbeItems(probeItems)
			} else if projectID != "" {
				// Probe succeeded but returned 0 items — possible transient indexer hiccup.
				// Leave cache virgin and retry on the next poll rather than bootstrapping
				// with an empty store (which would cause every real item to be treated as
				// "new" and deep-fetched on the next cycle).
				e.logf(0, "cache", "initial board probe returned 0 items — deferring bootstrap to next poll\n")
			}
		}
	}

	board, err := e.readClient.FetchProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		pollStatusClear()
		return pollResult{}, err
	}

	// Fetch status field metadata (for mutations) on first poll
	e.mu.Lock()
	if e.statusField == nil && board.ProjectID != "" {
		sf, err := e.readClient.FetchStatusField(board.ProjectID)
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

	// deepFetchCandidates is populated below; the defer captures it by reference
	// so it sees the final contents.
	var deepFetchCandidates []gh.ProjectItem
	// Items advanced by the yolo catch-up loop must NOT have their CooldownAt
	// re-stamped — the advance changes the item's board column, so the new stage
	// must dispatch on the next poll without waiting for the cooldown.
	advancedItems := make(map[string]bool)
	// priorInQueue captures each item's previous-poll merge-queue membership
	// (LinkedPRState.IsInMergeQueue) BEFORE ItemDeepFetched overwrites the store
	// with the current value. checkAutoMergeConvergence reads it to detect the
	// "left the queue" edge poll-natively (ADR-058 D4 OQ-3): reading "prior" from
	// e.store inside the classifier would yield the already-overwritten current
	// value, silently losing the edge. Keyed by issueKey, mirroring advancedItems.
	priorInQueue := make(map[string]bool)
	defer func() {
		// Refresh CooldownAt["periodic-re-eval"] for all non-advanced, non-cleanup
		// deepFetchCandidates after each full poll cycle. This preserves the #488 fix:
		// items are deep-fetched at most once per cooldown window. The stage:X:complete
		// terminal-only guard is removed here: LastAttemptAt (not CooldownAt) now carries
		// dispatch suppression, so refreshing CooldownAt for ALL non-advanced
		// deepFetchCandidates is safe regardless of completion state (#504 structural fix).
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		for _, item := range deepFetchCandidates {
			iKey := issueKey(item, e.defaultRepo())
			if advancedItems[iKey] {
				continue
			}
			if stage := stages.FindStage(e.cfg.Stages, item.Status); stage != nil && !stage.CleanupWorktree {
				e.store.Apply(itemstate.CooldownRecorded{
					Repo:   itemOwnerRepoString(item, e.defaultRepo()),
					Number: item.Number,
					Reason: "periodic-re-eval",
					Until:  time.Now().Add(cooldown),
				})
			}
		}
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

	// Seed labels on repos discovered for the first time this process run.
	// seededRepos is guarded by e.mu; the poll loop is single-goroutine but
	// lock for future-safety consistent with other Engine maps.
	{
		sn := make([]string, len(e.cfg.Stages))
		for i, s := range e.cfg.Stages {
			sn[i] = s.Name
		}
		for ownerRepo := range seenRepos {
			e.mu.Lock()
			already := e.seededRepos[ownerRepo]
			e.mu.Unlock()
			if already {
				continue
			}
			owner, repo := parseOwnerRepo(ownerRepo)
			if owner == "" {
				e.logf(0, "warn", "label seeding skipped: malformed repo %q\n", ownerRepo)
				e.mu.Lock()
				e.seededRepos[ownerRepo] = true
				e.mu.Unlock()
				continue
			}
			e.checkAllowAutoMerge(owner, repo)
			if err := e.client.SeedLabels(owner, repo, sn, e.cfg.User); err != nil {
				e.logf(0, "warn", "label seeding for %s failed (non-fatal): %v\n", ownerRepo, err)
			}
			e.mu.Lock()
			e.seededRepos[ownerRepo] = true
			e.mu.Unlock()
		}
	}

	// Drain mayNeedWork into a local cycleSet for this poll cycle. Observers fire
	// asynchronously (from any goroutine calling Apply) and write to e.mayNeedWork;
	// we snapshot it here so the dispatch loop works with a consistent view and so
	// that new changes arriving during this cycle are queued for the NEXT cycle.
	cycleSet := func() map[string]bool {
		e.mayNeedWorkMu.Lock()
		defer e.mayNeedWorkMu.Unlock()
		s := e.mayNeedWork
		e.mayNeedWork = make(map[string]bool)
		return s
	}()

	deepFetchCandidates, deepFetched := e.selectDeepFetchCandidates(board, repoFilter, cycleSet, priorInQueue)

	// Catch-up loop: operates only on deepFetchCandidates so the full label set is available.
	//
	// Phase 1 (unconditional): for every non-paused, non-cleanup item with a
	// stage:<X>:complete label OR fabrik:awaiting-ci (on a wait_for_ci stage),
	// run dependency check, review gate, CI gate, and review reinvoke regardless
	// of yolo/cruise/auto_advance. This ensures inline PR review thread comments
	// (Copilot, Gemini, human inline) are addressed on all issues, and that the
	// CI gate is evaluated every poll cycle during CI await.
	//
	// Phase 2 (gated): stage advancement, gated on yolo/cruise/auto_advance.
	for _, item := range deepFetchCandidates {
		// Skip paused items in both phases.
		if hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil || stage.CleanupWorktree || stage.HoldingStage {
			continue
		}
		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		hasComplete := hasLabel(item.Labels, completeLabel)
		hasAwaitingCI := hasLabel(item.Labels, "fabrik:awaiting-ci")
		// Admit items with fabrik:awaiting-ci on a wait_for_ci stage even when
		// stage:X:complete is absent — handleStageComplete now defers the
		// completion label until checkCIGate confirms CI is green (R4).
		isWaitForCI := stage.WaitForCI != nil && *stage.WaitForCI
		if !hasComplete && !(hasAwaitingCI && isWaitForCI) {
			continue
		}

		// Phase 1: run the ordered handler list. Each handler returns true to
		// claim the item (no further handlers run for this item, Phase 2 is
		// skipped) or false to pass through to the next handler. Ordering is
		// structurally enforced by slice position in catchUpPhase1Handlers
		// (ADR-056 D3).
		pctx := &phase1Ctx{
			ctx:           ctx,
			board:         board,
			item:          item,
			stage:         stage,
			hasComplete:   hasComplete,
			advancedItems: advancedItems,
			priorInQueue:  priorInQueue[issueKey(item, e.defaultRepo())],
		}
		claimed := false
		for _, h := range catchUpPhase1Handlers {
			if h.run(e, pctx) {
				claimed = true
				break
			}
		}
		if claimed {
			continue
		}

		// Phase 2: gated stage advancement.
		// Gate: yolo (cfg or label), cruise label, or stage-level auto_advance:true.
		isAutoAdvance := hasYoloLabel(item) || hasCruiseLabel(item)
		if !e.cfg.Yolo && !isAutoAdvance && !(stage.AutoAdvance != nil && *stage.AutoAdvance) {
			continue
		}
		// cruise and yolo labels override auto_advance:false on individual stages;
		// cfg.Yolo alone does not (allows per-stage opt-out to be respected).
		if !isAutoAdvance && stage.AutoAdvance != nil && !*stage.AutoAdvance {
			continue
		}

		if stage.Name == "Validate" {
			// auto-merge is yolo-only — cruise and auto_advance:true stop here.
			yoloActive := e.cfg.Yolo || hasYoloLabel(item)
			if !yoloActive {
				continue
			}
			// Items with fabrik:auto-merge-enabled are already in the GitHub
			// auto-merge convergence flow; checkAutoMergeConvergence (Phase 1)
			// monitors them and advances to Done when the PR merges.
			if hasLabel(item.Labels, "fabrik:auto-merge-enabled") {
				continue
			}
			_, mergeErr := e.attemptMergeOnValidate(ctx, board, item, stage)
			if mergeErr != nil {
				e.logf(item.Number, "warn", "auto-merge enablement failed during catch-up: %v\n", mergeErr)
			}
			// Auto-merge enabled (or failed); Done advancement is handled by
			// runValidatePRTerminalAdvance (ADR-056 D2) — do not advance here.
			continue
		}
		if newComments := e.findNewComments(item); len(newComments) > 0 {
			e.logf(item.Number, "advance", "skipping stage %q — %d unprocessed comment(s) pending\n", stage.Name, len(newComments))
			continue
		}
		if err := e.advanceToNextStage(board, item, stage); err != nil {
			e.logf(item.Number, "warn", "could not advance: %v\n", err)
		}
		// Mark as advanced so the defer doesn't re-cache the old updatedAt.
		// Board column moves don't bump updatedAt, so re-caching would
		// make the item look "unchanged" on the next poll.
		advancedItems[issueKey(item, e.defaultRepo())] = true
	}

	// Single-owner PR terminal advance: the authoritative path for all
	// "Validate-stage PR merged → advance to Done" transitions (ADR-056 D2).
	// Runs regardless of which gate label is present; no label negation required.
	e.runValidatePRTerminalAdvance(board, deepFetchCandidates, advancedItems)

	// No-work-needed settle scan: retries the outstanding Done-move/close for any
	// item carrying fabrik:awaiting-done, independent of item.Status.
	e.settleNoWorkNeededScan(board, deepFetchCandidates)

	// Revalidate scan: operator-facing fabrik:revalidate label re-entry.
	e.settleRevalidateScan(deepFetchCandidates)

	// SHA-invalidation scan: detect force-pushes or external commits that change
	// the linked PR's HEAD SHA after stage:Validate:complete was recorded.
	e.settleSHAInvalidationScan(deepFetchCandidates)

	// Dispatch only items from deepFetchCandidates — items that passed
	// itemMayNeedWork and (for non-cleanup stages) had FetchItemDetails called to
	// populate the full label set. Iterating board.Items here instead would
	// incorrectly pass shallow-label items (labels(first:5) only) to itemNeedsWork,
	// which could miss stage-complete labels beyond position 5 and re-dispatch
	// already-completed items on every poll after their updatedAt settles.
	dispatched := e.dispatchCandidates(ctx, board, deepFetchCandidates)

	// Merge-train batch snapshot: log all items currently in the Queued column.
	// Runs every poll cycle when merge_train: on. No dispatch, no mutation — D1 skeleton only.
	if e.cfg.MergeTrain == "on" {
		e.handleMergeTrainBatch(ctx, board)
	}

	// Merge-train member-issue close settle scan (ADR-061): retries the outstanding
	// landSingleton member-issue CloseIssue call for any item carrying
	// fabrik:awaiting-member-close. Runs unconditionally, independent of merge_train:
	// on/off, so a marker written while merge_train was enabled keeps draining even if
	// the setting is later turned off.
	e.settleMergeTrainMemberCloses(board)

	// Remove stale fabrik:locked labels from closed issues. This handles the case
	// where an issue was closed while a stage was in-flight, leaving the lock label
	// behind. We do this every poll so it also catches locks from prior Fabrik runs.
	e.cleanupClosedIssueLocks(board)
	// Sweep transient lifecycle labels from closed issues every poll cycle (#617).
	e.cleanupClosedIssueTransientLabels(board)

	// Child board-placement settle scan: retries the outstanding project Status
	// placement for any spawned child carrying fabrik:awaiting-placement.
	e.settleChildPlacements(board)

	// Closed-item-at-any-stage advance to Done (ADR-064): a closed issue sitting
	// at any non-Done, non-Holding, non-cleanup, non-gate-checked column never
	// passes itemMayNeedWork/itemNeedsWork's admission guard, so it never reaches
	// deepFetchCandidates and is never dispatched again — its worktree leaks and
	// it never gets archived. Sourced from board.Items directly, same rationale
	// as the child-placement scan above. Gate-checked stages (Validate) are
	// excluded — those closed items remain the exclusive responsibility of
	// runValidatePRTerminalAdvance, to avoid double-advance/racing between the
	// two settle-owners.
	e.settleClosedItemsToDone(board)

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
		for _, snap := range e.store.All() {
			if snap.Worker() != nil {
				k := snap.Repo() + "#" + fmt.Sprint(snap.Number())
				inFlightLabels = append(inFlightLabels, k)
			}
		}

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
		SeenRepos:  seenRepos,
	}, nil
}

// dispatchCandidates checks each deep-fetch candidate against itemNeedsWork and
// the in-flight worker guard, then dispatches a goroutine per admitted item,
// gated on an available e.sem slot. It aborts early (without blocking further)
// if ctx is cancelled while waiting for a slot. Returns the number of items
// dispatched this cycle.
func (e *Engine) dispatchCandidates(ctx context.Context, board *gh.ProjectBoard, deepFetchCandidates []gh.ProjectItem) int {
	var dispatched int

	// Check for duplicate items in deepFetchCandidates.
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
		iKey := issueKey(item, e.defaultRepo())
		itemRepo := itemOwnerRepoString(item, e.defaultRepo())
		debugLog("dispatch-check", map[string]interface{}{
			"number": item.Number, "key": iKey, "status": item.Status,
		})
		// Full check including comments (populated by deep fetch above).
		if !e.itemNeedsWork(item) {
			debugLog("dispatch-skip-no-work", map[string]interface{}{"number": item.Number})
			continue
		}
		// Skip issues already being processed by a previous poll cycle's worker.
		// Use the Store-backed Worker field (set by WorkerEntered before goroutine launch)
		// so this check is consistent with the observer pipeline.
		//
		// Do NOT cancel the in-flight context here. Every stage adds
		// stage:X:in_progress when it starts, which fires a webhook, marks the cache
		// stale, and triggers a new poll while the worker is still running. Cancelling
		// on every re-encounter creates a tight dispatch → label → webhook → poll →
		// cancel → respawn feedback loop that prevents any stage from completing a
		// turn. Genuine "supplant on new event" semantics need to distinguish
		// self-generated label changes from external ones — left as future work.
		if snap, err := e.store.Get(itemRepo, item.Number); err == nil && snap.Worker() != nil {
			debugLog("dispatch-skip-inflight", map[string]interface{}{"number": item.Number})
			continue
		}
		debugLog("dispatch-WILL-DISPATCH", map[string]interface{}{
			"number": item.Number, "key": iKey,
		})
		// Acquire semaphore slot, but abort if the context is cancelled so we
		// don't block indefinitely when all slots are taken at shutdown time.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return dispatched
		}
		// Capture stage name and start time for job tracking.
		var stageName string
		if s := stages.FindStage(e.cfg.Stages, item.Status); s != nil {
			stageName = s.Name
		}
		startTime := time.Now()
		// Apply WorkerEntered synchronously before the goroutine starts so that
		// snap.Worker() != nil is immediately true for any concurrent dispatch check.
		e.store.Apply(itemstate.WorkerEntered{
			Repo:      itemRepo,
			Number:    item.Number,
			StageName: stageName,
			StartedAt: startTime,
		})
		// Create a per-issue context so kill-reason annotation can propagate from
		// the cancellation path (daemon shutdown, supplant) to the kill log.
		// The holder is read in cmd.Cancel to derive the reason string.
		issueHolder := &killReasonHolder{}
		issueCtx, issueCancel := context.WithCancel(context.WithValue(ctx, killReasonCtxKey{}, issueHolder))
		e.issueCtxs.Store(iKey, issueCtxEntry{cancel: issueCancel, holder: issueHolder})
		e.wg.Add(1)
		dispatched++
		go func(issueCtx context.Context, issueCancel context.CancelFunc, iKey string, holder *killReasonHolder) {
			defer e.wg.Done()
			// Remove per-issue context entry on any exit path (success, panic, cancel).
			// Guard with holder pointer equality: if a supplant-cancel raced a new dispatch
			// between WorkerExited (which clears snap.Worker) and this delete, the new entry
			// would have a different holder and must not be removed.
			// issueCancel must also be called to release context resources even if the
			// parent ctx already cancelled (Go context semantics require explicit cancel call).
			defer func() {
				if current, ok := e.issueCtxs.Load(iKey); ok && current.(issueCtxEntry).holder == holder {
					e.issueCtxs.Delete(iKey)
				}
				issueCancel()
			}()
			// WorkerExited must be deferred at the goroutine top level so it fires on
			// every exit path, including processItem early-returns (paused, blocked,
			// awaiting-input, locked-by-other, stage-complete, etc.). The defer inside
			// processItem (item.go, after lock acquired) is reached only after ~14
			// early-return guards; any of them would leak the Worker entry and
			// permanently block re-dispatch via the snap.Worker() != nil guard. Same
			// pattern as the reinvoke dispatchers in reviews.go, ci.go, and
			// merge_gate.go.
			//
			// Ordering: WorkerExited must fire AFTER the semaphore release so the wake
			// it triggers does not race a fresh dispatch into a still-occupied slot.
			// Defers run LIFO; declaring WorkerExited BEFORE the sem-release defer
			// means sem-release runs first on exit, then WorkerExited fires its wake
			// against a freed slot.
			defer e.store.Apply(itemstate.WorkerExited{Repo: itemRepo, Number: item.Number})
			defer func() { <-e.sem }()
			err := e.processItem(issueCtx, board, item)
			if err != nil {
				e.logf(item.Number, "error", "%v\n", err)
			}
		}(issueCtx, issueCancel, iKey, issueHolder)
	}

	return dispatched
}

// selectDeepFetchCandidates runs the deep-fetch pre-filter loop: for each board
// item that passes the shallow admission checks (cycleSet membership, cleanup
// stage, bypass label, expired cooldown, or not-yet-recorded-in-store), it calls
// FetchItemDetails to populate the full label/comment/linked-PR set and appends
// the item to the returned slice. Cleanup-stage items are admitted without a
// deep-fetch (they only need a worktree existence check). priorInQueue is
// populated in place with each item's previous-poll merge-queue membership,
// captured before ItemDeepFetched overwrites the store (ADR-058 D4 OQ-3).
func (e *Engine) selectDeepFetchCandidates(board *gh.ProjectBoard, repoFilter string, cycleSet map[string]bool, priorInQueue map[string]bool) ([]gh.ProjectItem, int) {
	var deepFetchCandidates []gh.ProjectItem
	var deepFetched int
	for i := range board.Items {
		if repoFilter != "" && board.Items[i].Repo != "" && board.Items[i].Repo != repoFilter {
			continue
		}
		// Pre-filter: skip items that haven't changed since the last poll cycle.
		// An item is eligible for deep-fetch evaluation if:
		//   (a) it is in cycleSet (an observer saw a relevant Store change), OR
		//   (b) it is a cleanup stage (checks local filesystem, not board state), OR
		//   (c) it has a bypass label (awaiting-ci, awaiting-review, or rebase-needed need per-poll eval), OR
		//   (d) it has an expired CooldownAt (periodic re-evaluation gate has passed), OR
		//   (e) it is not yet recorded in the engine store (first poll / fresh startup).
		// Items with an active CooldownAt but no other signal are suppressed.
		item := board.Items[i]
		iKey := issueKey(item, e.defaultRepo())
		// Terminal skip: skip items flagged terminal while still in the same cleanup
		// stage — external board activity (label-bot, PR comments, GitHub bookkeeping)
		// bumps updatedAt but Fabrik has nothing left to do for them.
		// item.Status == admitSnap.Status() guards against items that moved between two
		// cleanup stages: in that case we must process normally to update the store.
		if admitSnap, admitErr := e.store.Get(itemOwnerRepoString(item, e.defaultRepo()), item.Number); admitErr == nil {
			if admitSnap.IsTerminal() {
				if pst := stages.FindStage(e.cfg.Stages, item.Status); pst != nil && pst.CleanupWorktree && item.Status == admitSnap.Status() {
					continue // terminal + still in same cleanup stage: skip entirely
				}
				// Status changed (left cleanup or moved to a different cleanup stage) —
				// clear the flag and fall through.
				e.store.Apply(itemstate.TerminalFlagSet{
					Repo:     itemOwnerRepoString(item, e.defaultRepo()),
					Number:   item.Number,
					Terminal: false,
				})
				e.logf(item.Number, "poll", "terminal flag cleared (status drifted to %q)\n", item.Status)
			}
		}
		if !cycleSet[iKey] {
			stage := stages.FindStage(e.cfg.Stages, item.Status)
			isCleanup := stage != nil && stage.CleanupWorktree
			hasAwaitingLabel := hasLabel(item.Labels, "fabrik:awaiting-ci") || hasLabel(item.Labels, "fabrik:rebase-needed") || hasLabel(item.Labels, "fabrik:awaiting-review") || hasLabel(item.Labels, "fabrik:auto-merge-enabled") || hasLabel(item.Labels, "fabrik:revalidate")
			var hasExpiredCooldown, notInStore bool
			if !isCleanup && !hasAwaitingLabel {
				repo := itemOwnerRepoString(item, e.defaultRepo())
				if snap, snapErr := e.store.Get(repo, item.Number); snapErr == nil {
					now := time.Now()
					hasExpiredCooldown = snap.HasExpiredCooldown(now)
					if snap.HasActiveCooldown(now) && !hasExpiredCooldown {
						continue // within cooldown window: no change + no expired window
					}
				} else {
					// Item not yet recorded in the engine store (first poll or fresh startup):
					// let it through so the deep-fetch path can populate the store.
					notInStore = true
				}
			}
			if !isCleanup && !hasAwaitingLabel && !hasExpiredCooldown && !notInStore {
				continue // no state change, no bypass — skip this cycle
			}
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
		if c := e.cache(); c != nil && !c.IsPaused() && c.IsItemCacheFresh(board.Items[i].Repo, board.Items[i].Number, board.Items[i].UpdatedAt) {
			e.logf(0, "poll", "reading details for #%d from cache\n", board.Items[i].Number)
		} else {
			e.logf(0, "poll", "deep-fetching details for #%d from GitHub\n", board.Items[i].Number)
		}
		if err := e.readClient.FetchItemDetails(&board.Items[i]); err != nil {
			e.logf(0, "warn", "could not fetch details for #%d: %v\n", board.Items[i].Number, err)
			e.store.Apply(itemstate.DeepFetchFailed{
				Repo:   itemOwnerRepoString(board.Items[i], e.defaultRepo()),
				Number: board.Items[i].Number,
				At:     time.Now(),
			})
			// Skip appending to deepFetchCandidates.
			// The next poll will retry the deep-fetch for this item.
			continue
		}
		admitRepo := itemOwnerRepoString(board.Items[i], e.defaultRepo())
		admitPreSnap, admitPreErr := e.store.Get(admitRepo, board.Items[i].Number)
		// Capture prior-poll merge-queue membership BEFORE ItemDeepFetched
		// overwrites it (ADR-058 D4 OQ-3 — the "left the queue" edge is otherwise lost).
		priorInQueue[iKey] = admitPreErr == nil && admitPreSnap.LinkedPR() != nil && admitPreSnap.LinkedPR().IsInMergeQueue
		e.store.Apply(itemstate.ItemDeepFetched{
			Repo:       admitRepo,
			Number:     board.Items[i].Number,
			FreshState: board.Items[i],
		})
		// After a successful deep-fetch, check if this item just became terminal.
		if isTerminalPredicate(board.Items[i].Labels, board.Items[i].Status, e.cfg.Stages) {
			if admitPreErr != nil || !admitPreSnap.IsTerminal() {
				e.logf(board.Items[i].Number, "poll", "terminal flag set\n")
			}
			e.store.Apply(itemstate.TerminalFlagSet{Repo: admitRepo, Number: board.Items[i].Number, Terminal: true})
		}
		deepFetchCandidates = append(deepFetchCandidates, board.Items[i])
		deepFetched++
	}
	if deepFetched > 0 {
		e.logf(0, "poll", "deep-fetched details for %d item(s)\n", deepFetched)
	}
	return deepFetchCandidates, deepFetched
}

// queuedRepoGroup is the Queued-column subset for a single owner/repo, preserving
// the board entry order of its members. ADR-059 D6 routes each group to its landing
// engine independently (isMergeQueueEnabled is a per-PR/per-repo signal).
type queuedRepoGroup struct {
	repoKey string // "owner/repo"
	items   []gh.ProjectItem
}

// groupQueuedByRepo collects board items in the holding (Queued) column and groups
// them by owner/repo, preserving first-seen repo order and per-repo entry order. This
// replaces the former flat cross-repo batch (which anchored the whole set on batch[0]'s
// repo and would shove repo B's items into repo A's trial branch — a latent multi-repo
// bug that per-repo grouping also hardens; ADR-059 D-3).
func groupQueuedByRepo(items []gh.ProjectItem, holdingStatus, defaultRepo string) []queuedRepoGroup {
	var order []string
	byRepo := make(map[string][]gh.ProjectItem)
	for _, item := range items {
		if item.Status != holdingStatus {
			continue
		}
		// Never form a train around a closed or paused member. A poisoner that fails the
		// combined Validate even in isolation is ejected and (after MaxMergeTrainEjections)
		// paused, but ejectMember deliberately leaves it in the Queued column. Without this
		// guard it would be re-snapshotted into every subsequent batch — a "poison well" that
		// reds and bisects the train indefinitely and starves clean members from ever landing.
		// A closed issue in Queued (stale board entry) likewise has no PR to land. Paused ==
		// "manual intervention required"; both are excluded until a human resolves and unpauses.
		if item.IsClosed || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		key := itemOwnerRepoString(item, defaultRepo)
		if _, seen := byRepo[key]; !seen {
			order = append(order, key)
		}
		byRepo[key] = append(byRepo[key], item)
	}
	groups := make([]queuedRepoGroup, 0, len(order))
	for _, key := range order {
		groups = append(groups, queuedRepoGroup{repoKey: key, items: byRepo[key]})
	}
	return groups
}

// handleMergeTrainBatch is the ADR-059 D6 "one board column, two landing engines"
// composition point: the single convergence owner (ADR-056 — no parallel scanner) that
// picks the landing engine per repo for the current Queued batch. Each poll it groups
// the Queued snapshot by owner/repo and routes each group by the poll-native
// isMergeQueueEnabled signal (FR-1/FR-3 precedence):
//
//  1. Native merge queue present (MergeQueue != "off" && LinkedPRIsMergeQueueEnabled) →
//     ADR-058 enqueue path (GitHub batches), regardless of merge_train. checkAutoMergeConvergence
//     then drains each enqueued item Queued → Done. Queue always wins (a direct/train merge on a
//     queue-required branch returns HTTP 405).
//  2. Else (merge_train: on, which is the only way items reach Queued) → the ADR-059 internal
//     merge train: one per-repo worker builds a trial branch, runs combined Validate, and lands
//     members Queued → Done.
//
// Both engines drain the same Queued column and advance their members to Done on land; only
// who batches differs.
func (e *Engine) handleMergeTrainBatch(ctx context.Context, board *gh.ProjectBoard) {
	hs := holdingStage(e.cfg)
	if hs == nil {
		return
	}
	for _, g := range groupQueuedByRepo(board.Items, hs.Name, e.defaultRepo()) {
		e.routeQueuedGroup(ctx, g.repoKey, g.items, board.ProjectID)
	}
}

// routeQueuedGroup applies the FR-1 per-repo engine selection to a single repo's Queued
// subset: queue-enabled items take the ADR-058 enqueue path (per item), the remainder form
// one internal-train batch (per repo) dispatched to a single worker. Runs in the poll
// goroutine; the enqueue is a per-item GitHub mutation idempotent via fabrik:auto-merge-enabled.
func (e *Engine) routeQueuedGroup(ctx context.Context, repoKey string, items []gh.ProjectItem, projectID string) {
	var trainItems []gh.ProjectItem
	for _, item := range items {
		// Precedence rule 1 (FR-3): native merge queue present → ADR-058 enqueue path.
		if e.cfg.MergeQueue != "off" && item.LinkedPRIsMergeQueueEnabled {
			// Idempotency: an item already carrying the label is mid-convergence — the
			// convergence monitor owns it. Don't re-enqueue.
			if hasLabel(item.Labels, "fabrik:auto-merge-enabled") {
				continue
			}
			// Signal source is poll-native (FR-1): the linked-PR number and head SHA come from
			// the GraphQL-populated item fields, never a REST re-fetch. On a cache miss, skip
			// this item this poll and retry next — enqueue needs the head SHA for the expected-OID guard.
			if item.LinkedPRNumber == 0 || item.LinkedPRHeadSHA == "" {
				e.logf(item.Number, "merge-train", "queue-enabled item missing poll-native linked-PR state (number=%d, headSHA=%q) — skipping enqueue this poll, will retry\n", item.LinkedPRNumber, item.LinkedPRHeadSHA)
				continue
			}
			owner, repo := itemOwnerRepo(item, e.defaultRepo())
			if _, err := e.enqueueForQueue(owner, repo, item, item.LinkedPRNumber, item.LinkedPRHeadSHA); err != nil {
				e.logf(item.Number, "warn", "enqueue from Queued handler failed: %v — will retry next poll\n", err)
			}
			continue
		}
		// Precedence rule 2 (FR-3): else the internal merge train.
		trainItems = append(trainItems, item)
	}

	if len(trainItems) == 0 {
		return
	}
	// FR-4: cap the internal-train batch to the first N items by entry order (ADR-059 D2),
	// applied PER repo group. Log the truncation explicitly so operators can see it — never silent.
	if maxBatch := e.effectiveMaxBatchSize(); len(trainItems) > maxBatch {
		e.logf(0, "merge-train", "batch capped for %s: %d Queued item(s) exceed max_batch_size=%d — landing first %d by entry order\n", repoKey, len(trainItems), maxBatch, maxBatch)
		trainItems = capBatch(trainItems, maxBatch)
	}
	// Hook 2: pre-dispatch runaway guard check (ADR-059 D8). Handles beyond-cap Queued
	// members that the in-flight worker couldn't reach during Hook 1 (one-poll-cycle gap).
	// Uses the uncapped `items` slice so all Queued members are paused, not just the batch cap.
	if count, tripped := e.isRunawayTripped(repoKey); tripped {
		owner, repo := parseOwnerRepo(repoKey)
		e.logf(0, "merge-train", "runaway guard already tripped for %s (%d trial(s)) — pausing %d Queued member(s) before dispatch\n", repoKey, count, len(items))
		e.fireRunawayGuard(ctx, owner, repo, items, count)
		return
	}
	var parts []string
	for _, item := range trainItems {
		parts = append(parts, fmt.Sprintf("#%d %q", item.Number, item.Title))
	}
	e.logf(0, "merge-train", "batch snapshot for %s: %d item(s) — %s\n", repoKey, len(trainItems), strings.Join(parts, ", "))
	e.dispatchMergeTrainWorker(ctx, trainItems, projectID)
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

// runProbeAndDeepFetch performs the per-poll probe-driven cache refresh for a
// bootstrapped CacheImpl. It calls ProbeProjectBoard to get a minimal per-item
// snapshot (no labels; single linked-PR node) and uses effectiveUpdatedAt to
// detect which items need a full FetchItemDetails call. Items that haven't
// changed since the last deep-fetch skip all GitHub traffic. This replaces the
// previous Reconcile(shallowBoard) path and reduces GraphQL cost ~5-10x on
// idle boards.
//
// Probe mutations must NOT touch the Labels field; use ProbeBoardItemUpdated,
// not ShallowBoardItemUpdated, to preserve webhook-driven label state.
func (e *Engine) runProbeAndDeepFetch(cacheImpl *boardcache.CacheImpl) {
	probeItems, _, err := e.client.ProbeProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		_, graphqlStats := e.client.RateLimitStats()
		if graphqlStats.Limit > 0 && (graphqlStats.Remaining == 0 || float64(graphqlStats.Remaining)/float64(graphqlStats.Limit) < rateLimitBackoffThreshold) {
			retryStr := "retrying soon"
			if !graphqlStats.Reset.IsZero() && graphqlStats.Reset.After(time.Now()) {
				retryStr = fmt.Sprintf("retrying after %s (local)", graphqlStats.Reset.Local().Format("15:04"))
			}
			e.logf(0, "warn", "rate limited — polling suspended, %s: %v\n", retryStr, err)
			e.emitStructural(tui.RateLimitAlertEvent{Exhausted: true, Reset: graphqlStats.Reset})
		} else {
			e.logf(0, "cache", "probe refresh failed (using prior cache state): %v\n", err)
		}
		return
	}

	// Guard: 0 probe items with a populated cache indicates a transient indexer
	// hiccup rather than a genuine board wipe. Skip this cycle; retry on the next
	// poll. (probeProjectBoardOnce already retries 3x, but a degraded response can
	// still slip through.)
	if len(probeItems) == 0 && cacheImpl.IsBootstrapped() {
		e.logf(0, "cache", "probe returned 0 items while cache has data — skipping (transient indexer hiccup)\n")
		return
	}

	newKeys := make(map[string]bool, len(probeItems))
	var deepFetched int

	for _, pi := range probeItems {
		repo := pi.Repo
		if repo == "" {
			repo = e.defaultRepo()
		}
		newKeys[fmt.Sprintf("%s#%d", repo, pi.Number)] = true

		// Stage-membership guard: whether the item's column has a matching Fabrik stage.
		// The guard must come after newKeys[key]=true so unconfigured items are not
		// falsely evicted from the store by the post-loop tombstoning pass (lines below).
		configuredStage := stages.FindStage(e.cfg.Stages, pi.Status) != nil

		snap, snapErr := e.store.Get(repo, pi.Number)
		if snapErr != nil {
			// New item on board — skip entirely if not in a configured stage.
			if !configuredStage {
				continue
			}
			// Seed minimal state into the store.
			minimal := gh.ProjectItem{
				ID:             pi.ContentID,
				ItemID:         pi.ItemID,
				Number:         pi.Number,
				IsPR:           pi.IsPR,
				IsClosed:       pi.IsClosed,
				Status:         pi.Status,
				Repo:           repo,
				UpdatedAt:      pi.EffectiveUpdatedAt,
				LinkedPRNumber: pi.LinkedPRNumber,
			}
			e.store.Apply(itemstate.IssueOpened{Item: minimal})
			// Probe-only terminal short-circuit: closed items in a cleanup stage whose
			// worktree is absent have no remaining Fabrik work; skip the deep-fetch.
			// isProbeOnlyTerminal logs the worktree-presence decision internally (FR-6).
			if e.isProbeOnlyTerminal(minimal) {
				e.store.Apply(itemstate.TerminalFlagSet{Repo: repo, Number: pi.Number, Terminal: true})
				e.logf(pi.Number, "cache", "probe: new item seeded terminal — skipping deep-fetch\n")
				continue
			}
			e.logf(pi.Number, "cache", "probe: new item discovered — deep-fetching\n")
			if fetchErr := e.readClient.FetchItemDetails(&minimal); fetchErr != nil {
				e.logf(pi.Number, "warn", "probe: deep-fetch for new item failed: %v\n", fetchErr)
				e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: pi.Number, At: time.Now()})
			} else {
				deepFetched++
			}
			continue
		}

		// Detect linkage drift: probe found a different linked PR than the cache holds.
		s := snap.State()
		cachedPRNum := 0
		if s.LinkedPR != nil {
			cachedPRNum = s.LinkedPR.Number
		}
		if pi.LinkedPRNumber != cachedPRNum {
			if s.LastDeepFetchAt.IsZero() {
				// Never deep-fetched: no prior deep cache to invalidate.
				// Treat the probe's value as authoritative — write it into LinkedPR.Number
				// and update the prToKey reverse index without firing DeepFetchInvalidated.
				// This suppresses spurious invalidations that occur when the bootstrapped
				// LinkedPR.Number differs from the probe's value (e.g., cold-start with
				// the old FetchProjectBoard path that did not populate LinkedPR.Number).
				// Apply even when pi.LinkedPRNumber==0 to clear a stale prToKey entry
				// if the PR was delinked between bootstrap and the first probe cycle.
				e.store.Apply(itemstate.PRDetailsUpdated{
					Repo:     repo,
					Number:   pi.Number,
					PRNumber: pi.LinkedPRNumber,
				})
			} else {
				// Warm cache (has been deep-fetched): real linkage drift — invalidate.
				e.logf(pi.Number, "cache", "probe: linkage drift (was PR #%d, now PR #%d) — invalidating deep cache\n",
					cachedPRNum, pi.LinkedPRNumber)
				e.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: pi.Number})
			}
		}

		// Terminal skip: if this item was previously identified as terminal and the
		// probe still shows it in the same cleanup stage, skip deep-fetch entirely —
		// external activity on a closed Done item has no bearing on Fabrik's work.
		// Must run BEFORE ProbeBoardItemUpdated so we read the cached Terminal flag;
		// applyProbeItem would clear it on status change before we could react to pi.Status.
		// pi.Status == s.Status guards against items that moved between two cleanup stages:
		// in that case we must apply the probe update so the store reflects the new status.
		if s.Terminal {
			if pst := stages.FindStage(e.cfg.Stages, pi.Status); pst != nil && pst.CleanupWorktree && pi.Status == s.Status {
				continue // still terminal in the same cleanup stage — no deep-fetch needed
			}
			// Status changed (left the cleanup stage or moved to a different one) — clear
			// the flag and fall through to normal probe processing.
			e.store.Apply(itemstate.TerminalFlagSet{Repo: repo, Number: pi.Number, Terminal: false})
			e.logf(pi.Number, "poll", "terminal flag cleared (status drifted to %q)\n", pi.Status)
		}

		// Apply probe state (updates IsClosed, State, IsPR, Status, UpdatedAt;
		// intentionally skips Labels to preserve webhook-driven label state).
		e.store.Apply(itemstate.ProbeBoardItemUpdated{Repo: repo, Number: pi.Number, Item: pi})

		// Unconditionally write the probe's head SHA whenever present — the probe
		// response is always authoritative, including for cache-fresh items that
		// skip the deep-fetch below. This is the primary poll-mode path for keeping
		// HeadSHA populated without relying solely on the REST FetchLinkedPR fallback.
		if pi.LinkedPRHeadSHA != "" && pi.LinkedPRNumber != 0 {
			e.store.Apply(itemstate.PRHeadSHAUpdated{
				Repo:        repo,
				Number:      pi.Number,
				LinkedPRNum: pi.LinkedPRNumber,
				SHA:         pi.LinkedPRHeadSHA,
			})
		}

		// Existing item in unconfigured column: probe state updated above (keeps
		// Status current for TUI and itemMayNeedWork), but deep-fetch is wasted work.
		if !configuredStage {
			continue
		}

		// Deep-fetch when cache is stale relative to effectiveUpdatedAt.
		if cacheImpl.IsItemCacheFresh(repo, pi.Number, pi.EffectiveUpdatedAt) {
			continue
		}
		e.logf(pi.Number, "cache", "probe: stale — deep-fetching\n")
		minimal := gh.ProjectItem{
			ID:        pi.ContentID,
			ItemID:    pi.ItemID,
			Number:    pi.Number,
			IsPR:      pi.IsPR,
			IsClosed:  pi.IsClosed,
			Status:    pi.Status,
			Repo:      repo,
			UpdatedAt: pi.EffectiveUpdatedAt,
		}
		if fetchErr := e.readClient.FetchItemDetails(&minimal); fetchErr != nil {
			e.logf(pi.Number, "warn", "probe: deep-fetch for stale item failed: %v\n", fetchErr)
			e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: pi.Number, At: time.Now()})
			continue
		}
		deepFetched++
		// After a successful deep-fetch, check if this item is now terminal.
		if isTerminalPredicate(minimal.Labels, minimal.Status, e.cfg.Stages) {
			if !s.Terminal {
				e.logf(pi.Number, "poll", "terminal flag set\n")
			}
			e.store.Apply(itemstate.TerminalFlagSet{Repo: repo, Number: pi.Number, Terminal: true})
		}
	}

	// Remove items no longer on the board.
	for _, snap := range e.store.All() {
		key := fmt.Sprintf("%s#%d", snap.Repo(), snap.Number())
		if !newKeys[key] {
			e.logf(snap.Number(), "cache", "probe: item gone from board — removing from store\n")
			e.store.Remove(snap.Repo(), snap.Number())
		}
	}

	if deepFetched > 0 {
		e.logf(0, "cache", "probe: deep-fetched %d item(s)\n", deepFetched)
	}
}

// isTerminalPredicate reports whether an item with the given labels and board
// status qualifies as terminal: it must be in a cleanup (Done) stage, carry the
// stage:Name:complete label, and have no transient lifecycle or lock labels.
// Terminal items have no remaining Fabrik work and may safely skip deep-fetch.
func isTerminalPredicate(labels []string, status string, stagesCfg []*stages.Stage) bool {
	st := stages.FindStage(stagesCfg, status)
	if st == nil || !st.CleanupWorktree {
		return false
	}
	completeLabel := "stage:" + st.Name + ":complete"
	hasComplete := false
	for _, l := range labels {
		if l == completeLabel {
			hasComplete = true
			break
		}
	}
	if !hasComplete {
		return false
	}
	for _, l := range labels {
		for _, tl := range transientLifecycleLabels {
			if l == tl {
				return false
			}
		}
		if strings.HasPrefix(l, "fabrik:locked:") {
			return false
		}
	}
	return true
}

// isProbeOnlyTerminal reports whether an item is terminal based on probe data
// (IsClosed + CleanupWorktree stage) and confirms the on-disk worktree is absent.
// Use this predicate in the new-item branch of runProbeAndDeepFetch and in
// seedTerminalFromProbeItems, where labels have not yet been fetched.
//
// The worktree check prevents stranding cleanup work: if the worktree still
// exists on disk (cleanup_worktree stage not yet run), the item must proceed
// through processItem so the Done stage cleanup can execute.
func (e *Engine) isProbeOnlyTerminal(item gh.ProjectItem) bool {
	if !item.IsClosed {
		return false
	}
	st := stages.FindStage(e.cfg.Stages, item.Status)
	if st == nil || !st.CleanupWorktree {
		return false
	}
	if e.worktreeExistsForItem(item) {
		e.logf(item.Number, "cache", "probe: worktree present — not treating as terminal yet\n")
		return false
	}
	e.logf(item.Number, "cache", "probe: no worktree on disk — treating as terminal\n")
	return true
}

// seedTerminalFromProbeItems applies TerminalFlagSet for probe items that
// qualify as terminal: closed, in a cleanup stage, and worktree absent on disk.
// Must be called after BootstrapFromProbe so items are in the store.
func (e *Engine) seedTerminalFromProbeItems(items []gh.BoardProbeItem) {
	var seeded int
	for _, pi := range items {
		stub := gh.ProjectItem{
			Number:   pi.Number,
			IsClosed: pi.IsClosed,
			Status:   pi.Status,
			Repo:     pi.Repo,
		}
		if e.isProbeOnlyTerminal(stub) {
			e.store.Apply(itemstate.TerminalFlagSet{
				Repo:     pi.Repo,
				Number:   pi.Number,
				Terminal: true,
			})
			seeded++
		}
	}
	if seeded > 0 {
		e.logf(0, "cache", "probe bootstrap: seeded %d terminal items (worktree absent, closed cleanup stage)\n", seeded)
	}
}

// runStartupTransientLabelScan is a one-shot recovery pass that runs after the
// first successful poll. It scans the Store for closed issues that still carry
// transient lifecycle labels — a condition that can occur when an issue closes
// mid-stage during a prior crash.
//
// Label availability note: when the cache was bootstrapped via BootstrapFromProbe
// (the default cold-start path), labels are absent from the Store and this scan
// is a no-op. The accepted gap: stale transient labels on closed terminal items
// will not be cleaned up at startup (very low probability — requires a crash
// between issue close and label removal in the Done stage). Active items are
// deep-fetched on the first probe cycle, which populates their labels normally,
// so those items are covered by the steady-state cleanup path.
// When bootstrapped via the full FetchProjectBoard path (webhook-paused / reconcile),
// the Store contains full label data and this scan is fully effective.
func (e *Engine) runStartupTransientLabelScan() {
	snaps := e.store.All()
	if len(snaps) == 0 {
		return
	}

	// Build a synthetic board containing only closed items that carry at least
	// one transient lifecycle label or a lock label. The cleanup helpers operate
	// on *gh.ProjectBoard items so we pass only the relevant subset.
	transientSet := make(map[string]bool, len(transientLifecycleLabels))
	for _, l := range transientLifecycleLabels {
		transientSet[l] = true
	}
	lockLabel := fmt.Sprintf("fabrik:locked:%s", e.cfg.User)

	var items []gh.ProjectItem
	for _, snap := range snaps {
		if !snap.IsClosed() {
			continue
		}
		labels := snap.Labels()
		hasStale := false
		for _, l := range labels {
			if transientSet[l] || l == lockLabel {
				hasStale = true
				break
			}
		}
		if !hasStale {
			continue
		}
		items = append(items, gh.ProjectItem{
			Number:   snap.Number(),
			Repo:     snap.Repo(),
			IsClosed: true,
			Labels:   labels,
		})
	}
	if len(items) == 0 {
		return
	}
	e.logf(0, "startup", "transient-label scan: %d closed item(s) with stale labels\n", len(items))
	board := &gh.ProjectBoard{Items: items}
	e.cleanupClosedIssueLocks(board)
	e.cleanupClosedIssueTransientLabels(board)
}

// runStartupTerminalScan marks terminal items in the Store after the first
// successful poll using the full label-aware predicate (isTerminalPredicate).
// Must run after runStartupTransientLabelScan so stale transient labels have
// been removed before the predicate is evaluated.
//
// Label availability note: when the cache was bootstrapped via BootstrapFromProbe
// (the default cold-start path), labels are absent from the Store. In that case
// this scan is a no-op — isTerminalPredicate returns false for every item.
// Cold-start terminal seeding is instead handled by seedTerminalFromProbeItems
// (called after BootstrapFromProbe) using IsClosed+CleanupWorktree+worktree-absent.
// When bootstrapped via the full FetchProjectBoard path, labels are present
// and this scan applies the full predicate correctly.
func (e *Engine) runStartupTerminalScan() {
	snaps := e.store.All()
	var marked int
	for _, snap := range snaps {
		if snap.IsTerminal() {
			continue // already set — no-op path
		}
		if isTerminalPredicate(snap.Labels(), snap.Status(), e.cfg.Stages) {
			e.store.Apply(itemstate.TerminalFlagSet{
				Repo:     snap.Repo(),
				Number:   snap.Number(),
				Terminal: true,
			})
			marked++
		}
	}
	if marked > 0 {
		e.logf(0, "startup", "terminal scan: marked %d item(s) as terminal\n", marked)
	}
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
// source repo (handarbeit/fabrik). Returns false on any error (no git, no
// remote, wrong remote, etc.).
func isFabrikSourceCheckout(dir string) bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	return strings.Contains(url, "handarbeit/fabrik")
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
	// Error discarded intentionally: failures are logged by PerformReleaseUpgrade
	// itself via logf, and this caller's contract is non-fatal — the poll loop
	// continues regardless (unlike the foreground `fabrik upgrade` command).
	_ = PerformReleaseUpgrade(e.client, e.cfg.Version, e.cfg.Token, []string{"FABRIK_AUTO_UPGRADED=1"}, logf)
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

// checkAllowAutoMerge queries the GitHub API for the allow_auto_merge setting on
// owner/repo and prints a WARNING if it is disabled. Non-fatal: API errors are
// logged at warn level and processing continues. The check fires at most once per
// repo per process run (guarded by checkedAutoMergeRepos).
func (e *Engine) checkAllowAutoMerge(owner, repo string) {
	key := owner + "/" + repo
	e.mu.Lock()
	already := e.checkedAutoMergeRepos[key]
	e.checkedAutoMergeRepos[key] = true
	e.mu.Unlock()
	if already {
		return
	}
	enabled, err := e.client.FetchAllowAutoMerge(owner, repo)
	if err != nil {
		e.logf(0, "warn", "could not check allow_auto_merge for %s: %v\n", key, err)
		return
	}
	if !enabled {
		e.logf(0, "startup", "WARNING: %s has allow_auto_merge disabled.\n", key)
		e.logf(0, "startup", "WARNING: yolo issues on this repo will reach Validate complete but their PRs will not merge.\n")
		e.logf(0, "startup", "WARNING: Fix: gh api -X PATCH repos/%s -f allow_auto_merge=true\n", key)
		_ = warnings.Record(warnings.Entry{
			Key:       "allow_auto_merge:" + key,
			Type:      "allow_auto_merge",
			Title:     "allow_auto_merge disabled on " + key,
			Detail:    fmt.Sprintf("yolo issues on this repo will reach Validate complete but their PRs will not merge.\n\nFix: gh api -X PATCH repos/%s -f allow_auto_merge=true", key),
			FixAction: "shell_command",
			FixParams: map[string]string{"cmd": fmt.Sprintf("gh api -X PATCH repos/%s -f allow_auto_merge=true", key)},
		})
	} else {
		_ = warnings.Clear("allow_auto_merge:" + key)
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

// reconcileLoop is the poll-only correctness backstop for cache/GitHub divergence.
// It periodically runs LightReconcile (a fresh shallow board fetch + drift compare)
// and, on drift, reconciles the cache — re-syncing shallow fields including the
// fabrik-managed label set. It MUST run whether or not the webhook manager started
// (#955): a webhook-less (or webhook-failed) deployment must still self-heal, so
// this loop is launched unconditionally rather than nested in the webhook-start
// path. wm may be nil (webhooks off/failed); webhook health-state transitions are
// skipped in that case, but drift detection and repair still run.
func (e *Engine) reconcileLoop(ctx context.Context, cacheImpl *boardcache.CacheImpl, wm *webhookManager) {
	reconcileInterval := e.cfg.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = lightReconcileInterval
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			driftCount, driftedKeys, freshBoard, err := cacheImpl.LightReconcile(
				e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType,
			)
			if err != nil {
				e.logf(0, "reconcile", "light reconcile failed (no health state change): %v\n", err)
				continue
			}
			if driftCount == 0 {
				if wm != nil {
					wm.transitionHealthState(WebhookStreamHealthy, "")
				}
				continue
			}
			keyStr := fmt.Sprintf("%v", driftedKeys)
			if len(driftedKeys) > 5 {
				keyStr = fmt.Sprintf("%v … %d more", driftedKeys[:5], len(driftedKeys)-5)
			}
			e.logf(0, "reconcile", "light reconcile: %d item(s) drifted (%s) — reconciling cache\n", driftCount, keyStr)
			if wm != nil {
				wm.transitionHealthState(WebhookStreamUnhealthy, fmt.Sprintf("%d item(s) drifted", driftCount))
			}
			cacheImpl.Pause()
			cacheImpl.Reconcile(freshBoard)
			cacheImpl.Resume()
			if wm != nil {
				wm.transitionHealthState(WebhookStreamHealthy, "drift reconciled")
			}
		}
	}
}

// applyLayer1StatusRefresh handles the Layer 1 opportunistic per-event Status
// refresh. Called from the deltaFn closure after ApplyDelta. For issue and
// issue_comment events, it fetches the current Status from GitHub and updates
// the cache immediately. Two paths:
//   - Fast path: cache has the item's itemID → calls FetchProjectItemStatus.
//   - Fallback path: cache lacks itemID (brand-new issue, issues.opened before
//     projects_v2_item.created) → calls LookupIssueProjectItem to populate
//     both itemID and Status in one query. Skipped when cache.ProjectID() == ""
//     (Bootstrap not yet complete).
//
// All errors are best-effort: logged as warnings and never returned.
func (e *Engine) applyLayer1StatusRefresh(eventType string, payload []byte, cache *boardcache.CacheImpl) {
	if cache.IsPaused() {
		return
	}
	if eventType != "issues" && eventType != "issue_comment" {
		return
	}
	var ev struct {
		Issue struct {
			Number int `json:"number"`
		} `json:"issue"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		e.logf(0, "warn", "layer1 status refresh: failed to parse %s payload: %v\n", eventType, err)
		return
	}
	if ev.Repository.FullName == "" || ev.Issue.Number == 0 {
		return
	}
	key := boardcache.ItemKey(ev.Repository.FullName, ev.Issue.Number)
	itemID, ok := cache.GetItemID(key)
	if !ok {
		// Fallback: issue is in the cache but has no itemID yet (e.g., arrived via
		// issues.opened before a projects_v2_item.created event). Perform a single
		// GraphQL lookup to populate both itemID and Status in one call.
		// If Bootstrap hasn't completed yet, projectID is empty — skip to avoid a
		// useless API call with an empty project ID during the startup window.
		projectID := cache.ProjectID()
		if projectID == "" {
			return
		}
		fetchedItemID, fetchedStatus, err := e.client.LookupIssueProjectItem(projectID, ev.Repository.FullName, ev.Issue.Number)
		if err != nil {
			e.logf(ev.Issue.Number, "warn", "layer1 fallback lookup failed for %s#%d: %v\n", ev.Repository.FullName, ev.Issue.Number, err)
			return
		}
		if fetchedItemID == "" {
			// Issue is not on the project fabrik manages — silently skip.
			return
		}
		cache.RegisterItemID(key, fetchedItemID)
		cache.UpdateItemStatus(key, fetchedStatus)
		return
	}
	status, err := e.client.FetchProjectItemStatus(itemID)
	if err != nil {
		e.logf(ev.Issue.Number, "warn", "layer1 status fetch failed for %s: %v\n", itemID, err)
		return
	}
	cache.UpdateItemStatus(key, status)
}
