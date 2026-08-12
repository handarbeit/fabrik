package pruefer

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer/events"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

// GitHubLister is the subset of *github.Client's methods Daemon needs
// beyond GitHubReviewer: listing open PRs per watched repo, and fetching a
// single PR's authoritative current state (used by the event-triggered
// review path — see eventsink.go — which must never trust webhook payload
// contents as PR state).
type GitHubLister interface {
	GitHubReviewer
	ListOpenPRs(owner, repo string) ([]gh.PRDetails, error)
	FetchPRDetails(owner, repo string, prNumber int) (*gh.PRDetails, error)
}

// RateLimitReporter is an optional interface implemented by *github.Client:
// type-asserted against Daemon.Client once per poll cycle so the TUI can
// surface REST API rate-limit state. Deliberately separate from
// GitHubLister/GitHubReviewer — adding this method there would force every
// test fake to implement it; fakes that don't simply emit no
// RateLimitSnapshotEvent, which is correct (there is no rate-limit data to
// show).
type RateLimitReporter interface {
	RateLimitStats() (rest, graphql gh.RateLimitStats)
}

// Daemon polls Pruefer's configured repos and dispatches eligible PRs to
// ReviewPR, mirroring engine/poll.go's Run()→poll(ctx) shape but with no
// board/stage concept: each cycle just lists open PRs per watched repo and
// hands them to ReviewPR, which does its own eligibility filtering.
type Daemon struct {
	// Clients maps each watched-repo owner to the GitHubLister whose token
	// is scoped to that owner's App installation — installation tokens are
	// strictly owner-scoped, so using the wrong one is a 403, not a soft
	// failure (see AuthSet/BootstrapMulti in auth.go). Every owner present
	// in Config.WatchedRepos must have an entry.
	Clients  map[string]GitHubLister
	Claude   ClaudeInvoker
	Clone    CloneFunc
	Config   Config
	BotLogin string

	// FabrikDir is the directory containing .pruefer/pruefer.lock. Defaults
	// to "." (cwd) when empty.
	//
	// For the dev-build self-upgrade path (checkAndUpgrade → devBuildDir in
	// upgrade.go) to work, FabrikDir must also be the root of the Fabrik
	// source checkout — devBuildDir resolves <FabrikDir>/cmd/pruefer as the
	// package to rebuild. This is not enforced: Execute() never sets this
	// field explicitly, relying on the "" → cwd default, so it's on the
	// operator to run Pruefer with its working directory at the checkout
	// root for dev-mode auto-upgrade to work. A mismatch fails closed —
	// selfupgrade.IsSourceCheckout simply returns false — rather than erroring.
	FabrikDir string

	// Emit, when non-nil, receives TUI observability events for poll cycles,
	// in-flight reviews, and outcomes. nil (the default) is a true no-op —
	// mirroring engine.Engine's events-channel-nil idiom — so headless
	// (-notui) operation incurs zero overhead and is never coupled to
	// ReviewPR's decision logic: every emit call here wraps ReviewPR from
	// the outside, never alters its inputs, return value, or control flow.
	Emit func(ptui.Event)

	// EventSource, when non-nil, switches Run into event-driven mode: it is
	// started alongside a low-frequency reconciliation fallback loop,
	// instead of poll() being the sole/primary driver. nil (the default,
	// matching event_source: poll) leaves Run's poll-only behavior exactly
	// as before this field existed. See runEventDriven.
	EventSource events.EventSource

	// sem is the concurrency-capped semaphore shared by every review
	// dispatch path — poll()'s per-cycle fan-out and event-triggered
	// ReviewFromEvent (eventsink.go) alike — so Pruefer's one claude
	// invocation budget is never exceeded regardless of which path
	// triggered a given review. Lazily built by semaphore() since Daemon
	// values are constructed as struct literals (execute.go, tests), not
	// via a constructor that could size it upfront.
	semOnce sync.Once
	sem     chan struct{}

	// prLocks is a small fixed-size set of stripe mutexes serializing
	// concurrent review dispatch for the same PR across poll- and
	// event-triggered paths — see prLock and runReview. A zero Daemon
	// (struct literal) has a usable zero-value [prLockStripes]sync.Mutex,
	// so no lazy-init is needed here unlike sem.
	prLocks [prLockStripes]sync.Mutex

	// pollInFlight coalesces concurrent installation-event-triggered
	// reconciliation polls — see triggerReconciliationPoll. A zero Daemon
	// has a usable zero-value atomic.Bool, so no lazy-init is needed here
	// either.
	pollInFlight atomic.Bool

	// dropMu guards dropCounts — unlike source.go's counters (single-writer,
	// off one WebSocket read loop), drops recorded here can originate from
	// eventsink.go's uncapped per-event goroutines (ReviewFromEvent) as well
	// as hookdeck.Source's OnDrop callback (invoked from its own read loop),
	// so concurrent writers are the normal case, not an edge case.
	dropMu     sync.Mutex
	dropCounts map[events.DropReason]int
}

// prLockStripes bounds prLocks to a small fixed size instead of an
// unbounded per-PR map that would grow for the daemon's entire lifetime.
// Unrelated PRs occasionally hashing to the same stripe only costs
// incidental contention (they still fully serialize against each other),
// never a correctness gap — the property runReview needs is "no two
// reviews of the *same* PR run concurrently," which holds regardless.
const prLockStripes = 64

// prLock returns the stripe mutex for owner/repo/prNumber, selected by
// hashing the PR's identity. See prLocks and runReview.
func (d *Daemon) prLock(owner, repo string, prNumber int) *sync.Mutex {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s/%s#%d", owner, repo, prNumber)
	return &d.prLocks[h.Sum32()%prLockStripes]
}

// semaphore returns the daemon's shared concurrency-capped semaphore,
// building it on first use.
func (d *Daemon) semaphore() chan struct{} {
	d.semOnce.Do(func() {
		d.sem = make(chan struct{}, d.effectiveConcurrency())
	})
	return d.sem
}

// emit is a nil-checked convenience wrapper around d.Emit.
func (d *Daemon) emit(ev ptui.Event) {
	if d.Emit != nil {
		d.Emit(ev)
	}
}

// NewDaemon is the sole production construction path for Daemon: it builds
// the Daemon and, when cfg.LogFile is non-empty, opens a rotating file
// logger and assigns it to the package-level pruefer.Logf hook so every
// logf call in this package routes to a timestamped, mutex-serialized log
// file instead of stderr (issue #1428, R1/R2). The returned close function
// closes the log file and clears Logf; callers should defer it.
//
// Deliberately NOT called from Daemon's own methods (Run/poll) — daemon_test.go
// builds Daemon{} literals directly and calls Run/poll without going through
// NewDaemon, relying on Logf staying nil in that path (R1: "Logf staying nil
// in tests must remain true"). Execute is the only production caller.
//
// In TUI mode (per useTUI(cfg)), stderr output would corrupt the bubbletea
// display, so the file is the sole destination; in plain daemon mode,
// logging is additive — lines are teed to both stderr and the file (R5).
//
// A log-file open failure is non-fatal: it's logged as a warning to stderr
// and, outside TUI mode, Logf is left nil (falling back to stderr for every
// line), mirroring engine/poll.go's own non-fatal fabrik.log open-failure
// handling. In TUI mode, an open failure or an explicitly empty cfg.LogFile
// both leave nothing routing pruefer/log.go's raw fmt.Fprintf(os.Stderr, ...)
// fallback out of the way of the bubbletea display, so Logf is instead
// assigned a discard function — the same corruption R5 wires the file-only
// path to avoid, just for the case where there is no file to write to.
func NewDaemon(cfg Config, clients map[string]GitHubLister, claude ClaudeInvoker, clone CloneFunc, botLogin string) (*Daemon, func() error) {
	d := &Daemon{
		Clients:  clients,
		Claude:   claude,
		Clone:    clone,
		Config:   cfg,
		BotLogin: botLogin,
	}

	return d, wireLogf(cfg, useTUI(cfg))
}

// wireLogf assigns the package-level Logf hook according to cfg.LogFile and
// tui (the caller's already-computed useTUI(cfg) result — split out as a
// parameter purely so this decision table is unit-testable without a real
// terminal, which useTUI itself requires). Returns the close function
// NewDaemon should defer.
func wireLogf(cfg Config, tui bool) func() error {
	discardLog := func(int, string, string, ...any) {}
	closeLog := func() error { return nil }

	switch {
	case cfg.LogFile != "":
		fl, err := newFileLogger(cfg.LogFile, logRotateMaxBytes, logRotateBackups, !tui)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pruefer: could not open log file %s: %v (falling back to stderr)\n", cfg.LogFile, err)
			if tui {
				Logf = discardLog
				closeLog = func() error { Logf = nil; return nil }
			}
		} else {
			Logf = fl.Logf
			closeLog = func() error {
				Logf = nil
				return fl.Close()
			}
		}
	case tui:
		// File logging explicitly disabled (log_file "") but TUI is active.
		Logf = discardLog
		closeLog = func() error { Logf = nil; return nil }
	}

	return closeLog
}

func (d *Daemon) lockPath() string {
	dir := d.FabrikDir
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, ".pruefer", "pruefer.lock")
}

// Run acquires an exclusive file lock (preventing two Pruefer instances from
// double-polling the same watched repos, mirroring engine/poll.go's
// Engine.Run), then dispatches to runPollOnly (Config.PollInterval-driven,
// the default, byte-for-byte unchanged from before EventSource existed) or
// runEventDriven (when EventSource is set — event_source: hookdeck),
// running until ctx is cancelled either way.
func (d *Daemon) Run(ctx context.Context) (err error) {
	lockPath := d.lockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return fmt.Errorf("creating lock dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}
	// A failed Close on a writable handle can mean a lost write (though the
	// lock file's own contents are never written to — this guards against
	// the general case). Surface it as the function's error when nothing
	// else already failed; otherwise log it so it isn't silently dropped.
	defer func() {
		if cerr := lockFile.Close(); cerr != nil {
			if err == nil {
				err = fmt.Errorf("closing lock file %s: %w", lockPath, cerr)
			} else {
				logf(0, "poll", "closing lock file %s after prior error: %v\n", lockPath, cerr)
			}
		}
	}()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another pruefer instance is already running (lock file: %s)", lockPath)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	if d.EventSource != nil {
		return d.runEventDriven(ctx)
	}
	return d.runPollOnly(ctx)
}

// runPollOnly is Run's original (pre-EventSource) poll loop, unchanged:
// poll every Config.PollInterval until ctx is cancelled. This is
// event_source: poll's entire behavior — the default, and what makes that
// default "byte-for-byte unchanged" from before this issue.
func (d *Daemon) runPollOnly(ctx context.Context) error {
	interval := d.Config.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	logf(0, "poll", "pruefer starting: watching %d repo(s), poll interval %s, concurrency %d\n",
		len(d.Config.WatchedRepos), interval, d.effectiveConcurrency())

	// lastUpgradeCheck is local to Run, not a Daemon field: Run is the sole
	// sequential caller (both TUI and headless modes call d.Run), so there's
	// no concurrent access to guard against. Zero value means the first
	// iteration always checks.
	var lastUpgradeCheck time.Time

	for {
		d.poll(ctx)

		// The upgrade check runs after poll() returns — poll() already calls
		// wg.Wait() before returning, so the in-flight review set is
		// guaranteed empty here. This is the poll-boundary safety guarantee:
		// re-exec'ing mid-review would orphan an ephemeral clone and a
		// running claude subprocess with no review posted (see ADR-1197).
		//
		// The 30-minute throttle is a rate/cost control, not a safety
		// requirement — it exists to bound git-fetch/GitHub-Releases-API
		// chatter (Pruefer's own poll interval defaults to 120s, which would
		// otherwise mean checking upstream roughly every 2 minutes).
		//
		// ctx.Err() == nil guards against racing a shutdown signal: neither
		// selfupgrade.CheckAndRebuildDev nor PerformReleaseUpgrade take a
		// context, and a successful upgrade ends in syscall.Exec — once
		// started, nothing can abort it. Without this guard, a SIGINT/SIGTERM
		// arriving while d.poll(ctx) is still finishing could be immediately
		// followed by a full git-fetch/build/re-exec or download/replace/
		// re-exec cycle instead of the loop reaching the <-ctx.Done() case
		// below, silently discarding the shutdown request. Mirrors
		// engine/poll.go's equivalent guard on its own checkAndUpgrade call
		// sites.
		if d.Config.AutoUpgrade && ctx.Err() == nil && time.Since(lastUpgradeCheck) >= upgradeCheckInterval {
			lastUpgradeCheck = time.Now()
			d.checkAndUpgrade()
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func (d *Daemon) reconciliationFallbackInterval() time.Duration {
	if d.Config.ReconciliationFallbackInterval > 0 {
		return d.Config.ReconciliationFallbackInterval
	}
	return DefaultReconciliationFallbackInterval
}

// runEventDriven runs Pruefer in event_source: hookdeck mode: EventSource
// delivers events to a daemonEventSink in the background, while poll() is
// demoted to a low-frequency safety net — an optional pass at startup, then
// one every reconciliationFallbackInterval() for the lifetime of the run.
// EventSource.Run is trusted to retry transport failures internally and
// never return except on ctx cancellation (see events.EventSource's
// contract), but this loop tolerates a misbehaving implementation that
// returns early anyway: the fallback ticker keeps polling regardless, so a
// dead event source degrades to poll-only rather than crashing the daemon.
func (d *Daemon) runEventDriven(ctx context.Context) error {
	fallback := d.reconciliationFallbackInterval()
	logf(0, "poll", "pruefer starting: watching %d repo(s) in event-driven mode, reconciliation fallback %s, concurrency %d\n",
		len(d.Config.WatchedRepos), fallback, d.effectiveConcurrency())

	if d.Config.ReconciliationStartup {
		logf(0, "poll", "running startup reconciliation poll\n")
		d.poll(ctx)
	}

	sourceDone := make(chan error, 1)
	go func() { sourceDone <- d.EventSource.Run(ctx, &daemonEventSink{daemon: d}) }()

	ticker := time.NewTicker(fallback)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.poll(ctx)
		case err := <-sourceDone:
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				logf(0, "warn", "event source exited unexpectedly: %v — continuing on poll-fallback only\n", err)
			} else {
				logf(0, "warn", "event source returned before ctx cancellation — continuing on poll-fallback only\n")
			}
			// Disable this case: the goroutine has exited and sourceDone
			// will never receive again, so avoid busy-looping on it.
			sourceDone = nil
		}
	}
}

// HealthHandler returns a callback suitable for wiring into an
// EventSource's transport-health hook (e.g. hookdeck.Config.OnHealth),
// bound to ctx — execute.go constructs the EventSource (and this callback)
// before Run/runEventDriven starts, so ctx is threaded through explicitly
// rather than stored on Daemon. A transition into events.HealthConnected
// that follows a prior connection (a reconnect) triggers a reconciliation
// poll: events may have been missed while disconnected, so this catches up
// via poll rather than trusting the transport's own replay alone. The very
// first connect is not treated as a reconnect — ReconciliationStartup
// already covers startup. The poll is coalesced via triggerReconciliationPoll
// (shared with installation events) rather than a bare goroutine — a
// flapping connection can achieve HealthConnected repeatedly in quick
// succession, and without coalescing each reconnect would spawn its own
// independent full ListOpenPRs sweep across every watched repo, exactly
// when the network is already unreliable.
func (d *Daemon) HealthHandler(ctx context.Context) func(events.HealthEvent) {
	var connectedBefore atomic.Bool
	return func(ev events.HealthEvent) {
		if ev.State != events.HealthConnected {
			return
		}
		if !connectedBefore.Swap(true) {
			return
		}
		logf(0, "poll", "event source reconnected — running a reconciliation poll\n")
		d.triggerReconciliationPoll(ctx)
	}
}

// recordDrop increments reason's cumulative count and emits a TUI
// DropEvent carrying the new total — see events.DropReason and ADR-1563.
// The sole entry point for drop accounting: hookdeck.Source's OnDrop
// callback (wired in execute.go) and eventsink.go's own four drop points
// both call this directly, since eventsink.go already lives in this
// package and needs no callback indirection.
//
// DropEvent.Total carries the cumulative count, not a delta: the TUI's own
// d.Emit wrapper (tui_run.go) drops messages under channel backpressure
// rather than blocking the daemon, so a delta-based counter would
// permanently under-count on any dropped TUI message — a total-based one
// self-heals on the next delivered event. dropCounts itself, guarded by
// dropMu, is never lossy regardless of what the TUI momentarily displays.
func (d *Daemon) recordDrop(reason events.DropReason) {
	d.dropMu.Lock()
	if d.dropCounts == nil {
		d.dropCounts = make(map[events.DropReason]int)
	}
	d.dropCounts[reason]++
	total := d.dropCounts[reason]
	d.dropMu.Unlock()
	d.emit(ptui.DropEvent{Reason: string(reason), Total: total, At: time.Now()})
}

// DropCounts returns a locked snapshot of every recorded drop reason's
// cumulative count. Exported for tests; production code has no need to
// read this back (recordDrop's TUI emission is the only consumer).
func (d *Daemon) DropCounts() map[events.DropReason]int {
	d.dropMu.Lock()
	defer d.dropMu.Unlock()
	out := make(map[events.DropReason]int, len(d.dropCounts))
	for k, v := range d.dropCounts {
		out[k] = v
	}
	return out
}

// SignatureDriftHandler returns a callback suitable for wiring into an
// EventSource's signature-drift hook (e.g. hookdeck.Config.OnSignatureDrift)
// — see ADR-1563. active=true means signature verification has just failed
// on SignatureDriftThreshold consecutive deliveries with no interleaved
// success: not a transient blip, but a misconfigured webhook secret or a
// wire-format change in whatever transport EventSource speaks. This is
// deliberately transport-agnostic (no "hookdeck" in the message) — the same
// convention HealthHandler above already follows — since Daemon has no
// business knowing which EventSource implementation is in play.
//
// Escalation here is a loud log line plus a TUI banner, not a mode switch:
// poll-fallback already always runs alongside event-driven mode
// (runEventDriven, ADR-1254 Decision 3) regardless of signature drift, so
// there is nothing to switch — only something to say plainly. The log
// line's "continuing on poll-fallback only" phrasing deliberately echoes
// runEventDriven's own sourceDone-exit message above, for a consistent
// operator-facing vocabulary across both kinds of event-source failure.
func (d *Daemon) SignatureDriftHandler() func(bool) {
	return func(active bool) {
		if active {
			logf(0, "warn", "event source signature verification is failing on every delivery — check the configured webhook secret, or the event source's wire protocol may have changed; continuing on poll-fallback only until this clears\n")
		} else {
			logf(0, "poll", "event source signature verification recovered\n")
		}
		d.emit(ptui.SignatureDriftEvent{Active: active, At: time.Now()})
	}
}

func (d *Daemon) effectiveConcurrency() int {
	if d.Config.ConcurrencyCap > 0 {
		return d.Config.ConcurrencyCap
	}
	return DefaultConcurrencyCap
}

// triggerReconciliationPoll starts one poll cycle in its own goroutine,
// coalescing concurrent requests: a burst of installation/repo-selection
// events (e.g. an app install across many repos, each a distinct delivery
// so dedupe doesn't collapse them), or a flapping Hookdeck connection
// achieving HealthConnected repeatedly in quick succession (see
// HealthHandler), would otherwise spawn one redundant full ListOpenPRs
// sweep per trigger. Only one poll runs at a time; a trigger that arrives
// while one is already in flight is dropped — the in-flight poll already
// re-derives current GitHub state, and any further change is still covered
// by the next fallback-interval tick or the poll-fallback safety net.
func (d *Daemon) triggerReconciliationPoll(ctx context.Context) {
	if !d.pollInFlight.CompareAndSwap(false, true) {
		logf(0, "poll", "reconciliation poll already in flight — coalescing this trigger\n")
		return
	}
	go func() {
		defer d.pollInFlight.Store(false)
		d.poll(ctx)
	}()
}

// poll runs one polling cycle across every watched repo, dispatching
// eligible PRs to ReviewPR through a concurrency-capped semaphore shared
// across the whole cycle (not per-repo) — Pruefer's claude capacity is one
// subscription shared across every watched repo, not one per repo.
func (d *Daemon) poll(ctx context.Context) {
	var wg sync.WaitGroup

	for _, repoSpec := range d.Config.WatchedRepos {
		owner, repo, ok := splitOwnerRepo(repoSpec)
		if !ok {
			logf(0, "warn", "skipping malformed watched repo %q (want owner/repo)\n", repoSpec)
			continue
		}
		client, ok := d.Clients[owner]
		if !ok {
			// Should not happen: BootstrapMulti validates every watched
			// owner has a resolved installation before the daemon starts.
			// Defensive skip rather than a nil-pointer panic if it ever
			// does (e.g. a hand-built Daemon in a future caller).
			logf(0, "warn", "no client for owner %q (repo %s) — skipping this repo this cycle\n", owner, repoSpec)
			continue
		}
		repoName := owner + "/" + repo
		pollAt := time.Now()
		prs, err := client.ListOpenPRs(owner, repo)
		if err != nil {
			logf(0, "warn", "listing open PRs for %s/%s: %v — skipping this repo this cycle\n", owner, repo, err)
			d.emit(ptui.RepoPollEvent{Repo: repoName, At: pollAt, Err: err.Error()})
			continue
		}
		d.emit(ptui.RepoPollEvent{Repo: repoName, At: pollAt, PRCount: len(prs)})
		for _, pr := range prs {
			d.reviewOne(ctx, &wg, client, owner, repo, pr)
		}
	}
	wg.Wait()

	for _, owner := range distinctOwners(d.Config.WatchedRepos) {
		client, ok := d.Clients[owner]
		if !ok {
			continue
		}
		if reporter, ok := client.(RateLimitReporter); ok {
			rest, _ := reporter.RateLimitStats()
			d.emit(ptui.RateLimitSnapshotEvent{Owner: owner, Stats: rest})
		}
	}
}

// reviewOne dispatches a single PR to ReviewPR through the daemon's shared
// concurrency-capped semaphore. Calls wg.Add(1) itself and guarantees a
// matching wg.Done, whether the PR is actually dispatched or ctx is
// cancelled first — callers must not call wg.Add themselves.
// Shared by poll() (fan-out per cycle) and ReviewFromEvent (event-triggered,
// see eventsink.go) so both draw from one concurrency budget, satisfying
// the issue's "hand off to the existing concurrency-capped review dispatch"
// requirement for real rather than just in shape.
func (d *Daemon) reviewOne(ctx context.Context, wg *sync.WaitGroup, client GitHubLister, owner, repo string, pr gh.PRDetails) {
	sem := d.semaphore()
	wg.Add(1)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		wg.Done()
		return
	}
	go func() {
		defer wg.Done()
		defer func() { <-sem }()
		d.runReview(ctx, client, owner, repo, pr)
	}()
}

// runReview acquires the PR's stripe lock (prLock), blocking until any
// review already in flight for the same PR finishes, then executes
// ReviewPR. Used by poll()'s reviewOne: poll cycles are infrequent (default
// 2m), so a poll-triggered dispatch blocking briefly on an event-triggered
// review already in progress for the same PR is not a resource-exhaustion
// concern the way a burst of event-triggered dispatches would be — see
// ReviewFromEvent's non-blocking claim instead, which drops rather than
// blocks for exactly that reason.
//
// Without this lock at all, ReviewPR's "check existing reviews, then
// submit" sequence (review.go's FetchPRReviews/alreadyReviewedAtHead check
// against a snapshot taken at call start) is a TOCTOU race: before
// event-triggered dispatch existed, only one poll loop ever ran, so a given
// PR was reviewed by at most one caller at a time. Now poll()'s per-cycle
// fan-out and ReviewFromEvent's per-event dispatch can both pick up the
// same PR at the same head SHA concurrently, both see no existing bot
// review, and both submit — a duplicate review beyond what ReviewPR's own
// SHA-idempotency is supposed to prevent. The lock makes the second
// caller's ReviewPR call observe the first caller's just-submitted review
// and skip, restoring the single-flight-per-PR property the SHA-idempotency
// guarantee assumes.
func (d *Daemon) runReview(ctx context.Context, client GitHubLister, owner, repo string, pr gh.PRDetails) {
	mu := d.prLock(owner, repo, pr.Number)
	mu.Lock()
	defer mu.Unlock()
	d.executeReview(ctx, client, owner, repo, pr)
}

// executeReview runs ReviewPR and emits the same TUI events poll()'s former
// inline goroutine did. Caller must already hold both a concurrency-budget
// semaphore slot and the PR's stripe lock (prLock) — executeReview touches
// neither itself; see runReview (blocking claim, used by poll()) and
// ReviewFromEvent (non-blocking claim, used by event-triggered dispatch)
// for the two ways callers establish that precondition.
func (d *Daemon) executeReview(ctx context.Context, client GitHubLister, owner, repo string, pr gh.PRDetails) {
	repoName := owner + "/" + repo
	startedAt := time.Now()
	d.emit(ptui.ReviewStartedEvent{Repo: repoName, PRNumber: pr.Number, Title: pr.Title, StartedAt: startedAt})
	outcome := ReviewPR(ctx, client, d.Claude, d.Clone, d.Config, d.BotLogin, owner, repo, pr)
	if outcome.Err != nil {
		logf(pr.Number, "warn", "reviewing %s/%s#%d: %v\n", owner, repo, pr.Number, outcome.Err)
	}
	errText := ""
	if outcome.Err != nil {
		errText = outcome.Err.Error()
	}
	d.emit(ptui.ReviewCompletedEvent{
		Repo: repoName, PRNumber: pr.Number, Title: pr.Title,
		Reviewed: outcome.Reviewed, Skipped: outcome.Skipped, Reason: string(outcome.Reason),
		Err: errText, NumTurns: outcome.NumTurns, CostUSD: outcome.CostUSD,
		Duration: time.Since(startedAt), CompletedAt: time.Now(),
	})
}

// isWatchedRepo reports whether owner/repo is one of Config.WatchedRepos.
// poll() never needs this check — it only ever iterates WatchedRepos in the
// first place — but event-triggered dispatch (ReviewFromEvent) must check
// explicitly: Daemon.Clients is owner-keyed, not repo-keyed, and an owner's
// installation token can plausibly cover repos beyond what Pruefer is
// configured to watch. In particular, the Hookdeck adapter creates its
// session with webhook_ids: [] (see hookdeck/client.go's createSession) —
// "every connection visible to this API key," not scoped to watched repos —
// so a webhook event for an unwatched repo under a watched owner is a
// plausible real delivery, not just a hypothetical one.
func (d *Daemon) isWatchedRepo(owner, repo string) bool {
	target := owner + "/" + repo
	for _, spec := range d.Config.WatchedRepos {
		if spec == target {
			return true
		}
	}
	return false
}

// splitOwnerRepo splits "owner/repo" into its two parts. Returns ok=false
// for anything else (missing/extra slash, empty parts).
func splitOwnerRepo(spec string) (owner, repo string, ok bool) {
	parts := strings.Split(spec, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
