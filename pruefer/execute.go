package pruefer

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// Execute is Pruefer's entry point: loads .env, resolves configuration,
// bootstraps GitHub App auth, constructs the GitHub client and Daemon, and
// runs until interrupted (SIGINT/SIGTERM). Mirrors cmd.Execute()'s shape
// for the main fabrik binary (see cmd/root.go), scaled down to Pruefer's
// needs — no board/stage machinery. The interactive TUI is enabled by
// default when a real terminal is detected (see useTUI); -notui or a
// non-interactive environment (systemd/tmux with no TTY) falls back to
// Pruefer's existing logf-based structured logging with identical review
// behavior.
func Execute() error {
	if err := config.LoadDotenv(); err != nil {
		return fmt.Errorf("loading .env: %w", err)
	}

	cfg, err := LoadConfig(os.Args[1:])
	if err != nil {
		return err
	}
	if cfg.VersionRequested {
		fmt.Println(Version)
		return nil
	}
	// Deliberately no cfg.AppID == 0 hard error here (unlike pre-#1253
	// behavior): a fresh install with no App ID yet is exactly what
	// githubauth.Reconcile's manifest bootstrap flow below handles — see
	// adrs/1253-github-app-manifest-auth-reconciler.md.
	if len(cfg.WatchedRepos) == 0 {
		return fmt.Errorf("no watched repos configured (set watched_repos, PRUEFER_REPOS, or --repos)")
	}

	// Wire the github package's diagnostic logger so pagination-cap warnings
	// (e.g. FetchAppInstallations/FetchInstallationRepositories hitting the
	// 100-result page limit) reach Pruefer's own logf instead of being
	// silently dropped — mirrors engine.go's gh.Logf wiring.
	gh.Logf = func(issueNumber int, tag, format string, args ...any) { logf(0, tag, format, args...) }

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Acquired here, before githubauth.Reconcile, rather than only around
	// Daemon.Run's poll loop as before: Reconcile can spin up the loopback
	// manifest callback server, launch a browser, hit GitHub's
	// manifest-exchange endpoint, and write PEM/app-state files (including
	// the self-heal path that recreates the App when identity validation
	// fails). Two Pruefer processes started concurrently against the same
	// .pruefer directory (e.g. an overlapping systemd restart) could
	// otherwise both race through first-run bootstrap or self-heal at
	// once, with each independently atomic file write interleaving into a
	// mismatched (AppID, private key) pair — exactly the
	// PrivateKeyFingerprint repair-needed case loadOrBootstrapCredentials
	// exists to catch, but avoidable in the first place by never letting
	// two processes attempt it simultaneously. Handed to daemon below via
	// preAcquiredLock so Daemon.Run doesn't try to acquire it a second
	// time (which would deadlock/fail against this same process's own
	// held lock).
	lockFile, err := acquireLock("")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := releaseLock(lockFile); cerr != nil {
			logf(0, "poll", "releasing lock file: %v\n", cerr)
		}
	}()

	auth, err := githubauth.Reconcile(ctx, githubauth.Options{
		AppID:             cfg.AppID,
		AppInstallationID: cfg.AppInstallationID,
		AppPrivateKeyPath: cfg.AppPrivateKeyPath,
		AppStatePath:      cfg.AppStatePath,
		WatchedRepos:      cfg.WatchedRepos,
		NoBrowser:         cfg.NoBrowser,
		Logf:              func(format string, args ...any) { logf(0, "auth", format+"\n", args...) },
	})
	if err != nil {
		return fmt.Errorf("reconciling GitHub App auth: %w", err)
	}
	logf(0, "auth", "authenticated as %s (%d installation(s))\n", auth.BotLogin(), auth.InstallationCount())

	waitRefreshLoops := auth.RunRefreshLoops(ctx, func(installationID int64) func(format string, args ...any) {
		prefix := fmt.Sprintf("installation %d: ", installationID)
		return func(format string, args ...any) {
			logf(0, "auth", prefix+format, args...)
		}
	})

	clients := make(map[string]GitHubLister, len(distinctOwners(cfg.WatchedRepos)))
	for _, owner := range distinctOwners(cfg.WatchedRepos) {
		client, err := auth.ClientForRepo(ctx, owner, "")
		if err != nil {
			logf(0, "warn", "no authorized client for owner %q yet — repos under it will be skipped until reconciled: %v\n", owner, err)
			continue
		}
		// Keyed by lower-cased owner — see daemon.go's distinctOwners and
		// poll()'s d.Clients lookups, which both normalize case the same
		// way for this exact reason.
		clients[strings.ToLower(owner)] = client
	}

	daemon, closeLog := NewDaemon(cfg, clients, &RealClaudeInvoker{}, CloneForReview, auth.BotLogin())
	// waitRefreshLoops must run before closeLog: the refresh-loop goroutines
	// spawned above are unmanaged by Daemon.Run's own lifecycle (unlike its
	// internal poll/review goroutines, which Run joins before returning), so
	// nothing else guarantees they've stopped calling logf — and therefore
	// stopped touching the package-level Logf hook closeLog tears down —
	// before this deferred close runs. See RunRefreshLoops' doc comment.
	defer func() {
		waitRefreshLoops()
		closeLog()
	}()
	daemon.preAcquiredLock = lockFile

	if useTUI(cfg) {
		return runTUI(ctx, daemon)
	}
	return daemon.Run(ctx)
}
