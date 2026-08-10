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
	// failure (see internal/githubauth.Reconcile). An owner present in
	// Config.WatchedRepos may still be missing an entry — e.g. the App
	// hasn't been installed on that account yet. Reconcile runs once, at
	// startup, only — this map is never refreshed afterward — so a missing
	// entry does not resolve itself; the operator must install the App
	// (Reconcile logs the guided-install URL) and then restart Pruefer.
	// See the poll() nil-check below.
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

	// preAcquiredLock, when set, is an already-open, already-flocked lock
	// file Run should use instead of acquiring its own — see Execute's
	// call site, which must hold this same lock across
	// githubauth.Reconcile (not just the poll loop) so two concurrently
	// started Pruefer processes can't both race through App
	// bootstrap/self-heal at once. nil (the default, and what every
	// existing test constructs) preserves Run's original
	// self-contained acquire-then-release behavior exactly.
	preAcquiredLock *os.File
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

// acquireLock opens (creating if needed) and non-blockingly, exclusively
// flocks .pruefer/pruefer.lock under fabrikDir ("" defaults to "."),
// returning the open, locked file for the caller to release via
// releaseLock when done. Extracted so Execute can hold the same lock across
// githubauth.Reconcile (which mutates on-disk credentials and can talk to
// GitHub) and not just Daemon.Run's poll loop — see Daemon.preAcquiredLock
// and this function's call sites in both places.
func acquireLock(fabrikDir string) (*os.File, error) {
	dir := fabrikDir
	if dir == "" {
		dir = "."
	}
	lockPath := filepath.Join(dir, ".pruefer", "pruefer.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, fmt.Errorf("creating lock dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if closeErr := lockFile.Close(); closeErr != nil {
			return nil, fmt.Errorf("another pruefer instance is already running (lock file: %s), and closing our own handle to it also failed: %v", lockPath, closeErr)
		}
		return nil, fmt.Errorf("another pruefer instance is already running (lock file: %s)", lockPath)
	}
	return lockFile, nil
}

// releaseLock unlocks and closes a *os.File returned by acquireLock.
func releaseLock(lockFile *os.File) error {
	syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	return lockFile.Close()
}

// Run acquires an exclusive file lock (preventing two Pruefer instances from
// double-polling the same watched repos, mirroring engine/poll.go's
// Engine.Run) — unless d.preAcquiredLock is already set, in which case Run
// uses that instead of acquiring (or releasing) its own — and polls on
// Config.PollInterval until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) (err error) {
	lockFile := d.preAcquiredLock
	if lockFile == nil {
		lockFile, err = acquireLock(d.FabrikDir)
		if err != nil {
			return err
		}
		// A failed Close on a writable handle can mean a lost write (though
		// the lock file's own contents are never written to — this guards
		// against the general case). Surface it as the function's error
		// when nothing else already failed; otherwise log it so it isn't
		// silently dropped.
		defer func() {
			if cerr := releaseLock(lockFile); cerr != nil {
				if err == nil {
					err = fmt.Errorf("releasing lock file %s: %w", d.lockPath(), cerr)
				} else {
					logf(0, "poll", "releasing lock file %s after prior error: %v\n", d.lockPath(), cerr)
				}
			}
		}()
	}

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
		client, ok := d.Clients[strings.ToLower(owner)]
		if !ok {
			// Expected when an owner has no resolved App installation (see
			// internal/githubauth.Reconcile, which already logged a guided-
			// install URL for this owner at startup). Reconcile does not
			// re-run after startup, so this is not transient: it recurs
			// every poll cycle until the operator installs the App and
			// restarts Pruefer. Skip rather than panic.
			logf(0, "warn", "no client for owner %q (repo %s) — install the GitHub App on %q, then restart Pruefer to pick it up; skipping this repo until then\n", owner, repoSpec, owner)
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
		client, ok := d.Clients[strings.ToLower(owner)]
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

// distinctOwners returns the distinct owners of every well-formed
// "owner/repo" entry in watchedRepos, in first-seen order. Malformed entries
// are skipped here — poll() already logs and skips them independently.
//
// Dedup is case-insensitive (keyed on the lower-cased owner, keeping the
// first-seen literal casing in the result), mirroring
// internal/githubauth's distinctOwnersLogging: GitHub org/user logins are
// case-insensitive, so "MyOrg/repo1" and "myorg/repo2" name the same
// account. d.Clients (built by execute.go from this same function) is
// itself keyed by lower-cased owner for the same reason — an exact-case
// dedup here would produce a second "distinct" owner with no corresponding
// d.Clients entry, silently dropping that repo from every poll cycle.
func distinctOwners(watchedRepos []string) []string {
	seen := make(map[string]bool)
	var owners []string
	for _, spec := range watchedRepos {
		owner, _, ok := splitOwnerRepo(spec)
		if !ok {
			continue
		}
		key := strings.ToLower(owner)
		if seen[key] {
			continue
		}
		seen[key] = true
		owners = append(owners, owner)
	}
	return owners
}
