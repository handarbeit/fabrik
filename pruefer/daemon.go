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
)

// GitHubLister is the subset of *github.Client's methods Daemon needs
// beyond GitHubReviewer: listing open PRs per watched repo.
type GitHubLister interface {
	GitHubReviewer
	ListOpenPRs(owner, repo string) ([]gh.PRDetails, error)
}

// Daemon polls Pruefer's configured repos and dispatches eligible PRs to
// ReviewPR, mirroring engine/poll.go's Run()→poll(ctx) shape but with no
// board/stage concept: each cycle just lists open PRs per watched repo and
// hands them to ReviewPR, which does its own eligibility filtering.
type Daemon struct {
	Client   GitHubLister
	Claude   ClaudeInvoker
	Clone    CloneFunc
	Config   Config
	BotLogin string

	// FabrikDir is the directory containing .pruefer/pruefer.lock. Defaults
	// to "." (cwd) when empty.
	FabrikDir string
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
func (d *Daemon) Run(ctx context.Context) error {
	lockPath := d.lockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return fmt.Errorf("creating lock dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening lock file %s: %w", lockPath, err)
	}
	defer lockFile.Close()
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
		prs, err := d.Client.ListOpenPRs(owner, repo)
		if err != nil {
			logf(0, "warn", "listing open PRs for %s/%s: %v — skipping this repo this cycle\n", owner, repo, err)
			continue
		}
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
				outcome := ReviewPR(ctx, d.Client, d.Claude, d.Clone, d.Config, d.BotLogin, owner, repo, pr)
				if outcome.Err != nil {
					logf(pr.Number, "warn", "reviewing %s/%s#%d: %v\n", owner, repo, pr.Number, outcome.Err)
				}
			}()
		}
	}
	wg.Wait()
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
