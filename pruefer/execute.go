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
	"github.com/handarbeit/fabrik/pruefer/events/hookdeck"
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

	// Wire the package-level Logf hook (per cfg.LogFile/useTUI) before
	// anything below logs a line — including githubauth.Reconcile's own
	// first-run/self-heal auth output (setup URLs, guided-install
	// instructions). NewDaemon normally does this, but NewDaemon can't run
	// until Reconcile has produced auth.BotLogin() and clients below, by
	// which point Reconcile's own log lines would already have been emitted
	// through log.go's raw-stderr fallback (bypassing cfg.LogFile
	// entirely) had this call been left at its old, later position.
	// wireLogf is idempotent — NewDaemon's own internal call becomes a
	// no-op once Logf is already set here, so the file isn't opened twice.
	//
	// Deferred immediately, rather than bundled into the later
	// waitRefreshLoops/closeLog defer below: githubauth.Reconcile (called
	// further down) can fail before that later defer is ever registered,
	// and closeLog must still run on that path — otherwise the open log
	// file handle leaks and the package-level Logf hook stays wired for
	// the rest of the process (masked today only by main.go's immediate
	// os.Exit(1) on any Execute error, which skips no-longer-relevant
	// deferred cleanup anyway — but a future caller of Execute that
	// doesn't immediately exit, e.g. a test, would observe the leak).
	closeLog := wireLogf(cfg, useTUI(cfg))
	defer closeLog()

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

	// NewDaemon's own wireLogf call is a no-op here (Logf was already wired
	// above, before Reconcile) — its returned close func is discarded in
	// favor of the one already captured, which is what actually owns the
	// open log file.
	daemon, _ := NewDaemon(cfg, clients, &RealClaudeInvoker{}, CloneForReview, auth.BotLogin())
	// waitRefreshLoops must run before closeLog: the refresh-loop goroutines
	// spawned above are unmanaged by Daemon.Run's own lifecycle (unlike its
	// internal poll/review goroutines, which Run joins before returning), so
	// nothing else guarantees they've stopped calling logf — and therefore
	// stopped touching the package-level Logf hook closeLog tears down —
	// before this deferred close runs. See RunRefreshLoops' doc comment.
	// Deferring it here, after closeLog's own defer above, is what gets
	// that ordering for free: defers run LIFO, so this one (registered
	// later) fires first on return, before closeLog's.
	//
	// waitRefreshLoops() cannot hang here only because both return paths
	// below (Daemon.Run and runTUI) are structurally guaranteed to return
	// solely once ctx is already cancelled — Run via the real OS signal,
	// runTUI via its own self-SIGTERM on quit — which is exactly the ctx
	// the refresh-loop goroutines select on to exit. This is an implicit
	// invariant of those two functions' current return conditions, not
	// something enforced here: a future change that lets either one return
	// early for some other reason (without ctx having been cancelled) would
	// silently turn this into a shutdown hang. If that invariant ever needs
	// to change, wire an explicit deadline into waitRefreshLoops() (or the
	// underlying ctx) instead of assuming it away.
	defer waitRefreshLoops()
	daemon.preAcquiredLock = lockFile

	if cfg.EventSource == EventSourceHookdeck {
		apiKey := os.Getenv(cfg.HookdeckAPIKeyEnv)
		if apiKey == "" {
			return fmt.Errorf("event_source: hookdeck requires %s to be set (see hookdeck.api_key_env)", cfg.HookdeckAPIKeyEnv)
		}
		webhookSecret := os.Getenv(cfg.HookdeckWebhookSecretEnv)
		if webhookSecret == "" {
			return fmt.Errorf("event_source: hookdeck requires %s to be set (see hookdeck.webhook_secret_env)", cfg.HookdeckWebhookSecretEnv)
		}
		// Route hookdeck's package-level log hook through pruefer's own —
		// hookdeck can't import pruefer (pruefer imports hookdeck), so
		// without this its logs would write straight to os.Stderr
		// unconditionally, bypassing whatever pruefer.Logf is ever wired to
		// (e.g. a future TUI-safe sink) and diverging from every other
		// pruefer log line's routing.
		hookdeck.SetLogf(func(format string, args ...any) {
			logf(0, "hookdeck", format, args...)
		})
		daemon.EventSource = hookdeck.NewSource(hookdeck.Config{
			APIKey:        apiKey,
			WebhookSecret: webhookSecret,
			OnHealth:      daemon.HealthHandler(ctx),
		})
		logf(0, "poll", "event_source: hookdeck — Hookdeck API key from $%s, webhook secret from $%s\n",
			cfg.HookdeckAPIKeyEnv, cfg.HookdeckWebhookSecretEnv)
	}

	if useTUI(cfg) {
		return runTUI(ctx, daemon)
	}
	return daemon.Run(ctx)
}
