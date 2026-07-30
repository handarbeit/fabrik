package pruefer

import (
	"context"
	"fmt"
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

	for {
		d.poll(ctx)
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
// already covers startup.
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
		go d.poll(ctx)
	}
}

func (d *Daemon) effectiveConcurrency() int {
	if d.Config.ConcurrencyCap > 0 {
		return d.Config.ConcurrencyCap
	}
	return DefaultConcurrencyCap
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
// concurrency-capped semaphore, emitting the same TUI events poll()'s
// former inline goroutine did. Calls wg.Add(1) itself and guarantees a
// matching wg.Done, whether the PR is actually dispatched or ctx is
// cancelled first — callers must not call wg.Add themselves.
// Shared by poll() (fan-out per cycle) and ReviewFromEvent (event-triggered,
// see eventsink.go) so both draw from one concurrency budget, satisfying
// the issue's "hand off to the existing concurrency-capped review dispatch"
// requirement for real rather than just in shape.
func (d *Daemon) reviewOne(ctx context.Context, wg *sync.WaitGroup, client GitHubLister, owner, repo string, pr gh.PRDetails) {
	repoName := owner + "/" + repo
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
	}()
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
