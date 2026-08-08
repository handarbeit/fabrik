package pruefer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
)

// GitHubLister is the subset of *github.Client's methods Daemon needs
// beyond GitHubReviewer: listing open PRs per watched repo.
type GitHubLister interface {
	GitHubReviewer
	ListOpenPRs(owner, repo string) ([]gh.PRDetails, error)
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
// and Logf is left nil (falling back to stderr for every line), mirroring
// engine/poll.go's own non-fatal fabrik.log open-failure handling.
func NewDaemon(cfg Config, clients map[string]GitHubLister, claude ClaudeInvoker, clone CloneFunc, botLogin string) (*Daemon, func() error) {
	d := &Daemon{
		Clients:  clients,
		Claude:   claude,
		Clone:    clone,
		Config:   cfg,
		BotLogin: botLogin,
	}

	closeLog := func() error { return nil }
	if cfg.LogFile != "" {
		fl, err := newFileLogger(cfg.LogFile, logRotateMaxBytes, logRotateBackups, !useTUI(cfg))
		if err != nil {
			fmt.Fprintf(os.Stderr, "pruefer: could not open log file %s: %v (falling back to stderr)\n", cfg.LogFile, err)
		} else {
			Logf = fl.Logf
			closeLog = func() error {
				Logf = nil
				return fl.Close()
			}
		}
	}

	return d, closeLog
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
// Engine.Run) and polls on Config.PollInterval until ctx is cancelled.
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
	sem := make(chan struct{}, d.effectiveConcurrency())
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
			pr := pr
			wg.Add(1)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Done()
				continue
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

// splitOwnerRepo splits "owner/repo" into its two parts. Returns ok=false
// for anything else (missing/extra slash, empty parts).
func splitOwnerRepo(spec string) (owner, repo string, ok bool) {
	parts := strings.Split(spec, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
