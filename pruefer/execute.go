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

	// Captured once so a later SIGHUP-triggered reload (handleReload) can
	// re-run LoadConfig against the same argument list, rather than
	// re-reading a possibly-stale os.Args.
	args := os.Args[1:]

	cfg, err := LoadConfig(args)
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
			APIKey:           apiKey,
			WebhookSecret:    webhookSecret,
			OnHealth:         daemon.HealthHandler(ctx),
			OnDrop:           daemon.recordDrop,
			OnSignatureDrift: daemon.SignatureDriftHandler(),
		})
		logf(0, "poll", "event_source: hookdeck — Hookdeck API key from $%s, webhook secret from $%s\n",
			cfg.HookdeckAPIKeyEnv, cfg.HookdeckWebhookSecretEnv)
	}

	// SIGHUP triggers a config reload (ADR-1640) — deliberately a separate
	// signal channel from the SIGINT/SIGTERM ctx above, not folded into it:
	// that context's cancellation means shutdown, and a reload must never
	// be mistaken for one. See handleReload's doc comment for what a
	// reload does and does not touch.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hupCh:
				logf(0, "reload", "SIGHUP received — reloading config\n")
				handleReload(ctx, args, auth, daemon)
			}
		}
	}()

	if useTUI(cfg) {
		return runTUI(ctx, daemon)
	}
	return daemon.Run(ctx)
}

// handleReload processes one SIGHUP (ADR-1640): re-reads config via
// LoadConfig(args), merges it against daemon's currently running config
// (applyConfigReload), mints a token for any newly-watched owner
// (MintOwnerAuth, all-or-nothing across the whole reload — see that
// method's doc comment), and only once every step has succeeded does it
// apply the merged config (and any newly-minted clients) to daemon and log
// a diff-style summary.
//
// A failure at any step — a malformed/invalid config file, or a newly
// watched owner with no matching App installation — leaves daemon and
// reconciler completely untouched: nothing is committed until every owner
// in the batch has minted successfully. Nothing here cancels a running
// review, drops the Hookdeck EventSource, or resets its dedupe ring — those
// are untouched by construction, since this function never rebuilds or
// reaches into any of them. This includes a removed owner's installation
// token: its refresh loop is only ever stopped once
// Daemon.drainThenStopAuth (spawned below) has confirmed no review is still
// in flight for that owner, so an in-flight review's remaining GitHub calls
// never lose a valid token out from under them.
func handleReload(ctx context.Context, args []string, reconciler *githubauth.Reconciler, daemon *Daemon) {
	cand, err := LoadConfig(args)
	if err != nil {
		logf(0, "reload", "config reload failed: %v — keeping current config\n", err)
		return
	}
	if cand.VersionRequested {
		// --version on the original invocation would already have made
		// Execute print the version and return before a daemon (and thus
		// this handler) ever existed. Reaching here means the *current*
		// process wasn't a --version invocation, but re-parsing the same
		// args produced VersionRequested anyway — LoadConfig re-resolves
		// the full flag/env/YAML chain every call, so this is defensive,
		// not expected to fire in practice.
		logf(0, "reload", "config reload failed: --version is not a reloadable configuration — keeping current config\n")
		return
	}

	old := daemon.config()
	merged, diff := applyConfigReload(old, cand)

	newOwners := newlyWatchedOwners(old, merged)
	type mintedOwner struct {
		owner       string
		auth        *githubauth.Auth
		mintedFresh bool
	}
	minted := make([]mintedOwner, 0, len(newOwners))
	for _, owner := range newOwners {
		reloadLogf := func(format string, args ...any) { logf(0, "reload", format+"\n", args...) }
		a, fresh, err := reconciler.MintOwnerAuth(owner, merged.WatchedRepos, reloadLogf)
		if err != nil {
			logf(0, "reload", "config reload failed: %v — keeping current config\n", err)
			return
		}
		minted = append(minted, mintedOwner{owner: owner, auth: a, mintedFresh: fresh})
	}

	addedClients := make(map[string]GitHubLister, len(minted))
	for _, m := range minted {
		installationID := m.auth.InstallationID
		prefix := fmt.Sprintf("installation %d: ", installationID)
		client := reconciler.CommitOwnerAuth(ctx, m.owner, m.auth, m.mintedFresh, func(format string, args ...any) {
			logf(0, "auth", prefix+format, args...)
		})
		addedClients[strings.ToLower(m.owner)] = client
	}

	// Owners no longer present in watched_repos are pruned only now, after
	// every newly-watched owner in this same batch has already minted
	// successfully — an earlier prune (e.g. right after computing merged)
	// would violate atomicity if a later owner's mint then failed and
	// aborted the whole reload above: the removed owner's client would
	// already be gone even though the reload as a whole never took effect.
	// RemoveOwners only detaches each removed (non-pinned) owner's *Auth
	// from the Reconciler's own bookkeeping — it deliberately does NOT stop
	// its refresh goroutine yet. That happens below, once
	// drainThenStopAuth confirms no review is still in flight for the
	// owner being removed.
	removedOwners := noLongerWatchedOwners(old, merged)
	detached := reconciler.RemoveOwners(removedOwners)

	removedLowered := make([]string, len(removedOwners))
	for i, o := range removedOwners {
		removedLowered[i] = strings.ToLower(o)
	}
	daemon.ApplyReload(merged, addedClients, removedLowered)
	logReloadSummary(diff)

	for _, d := range detached {
		go daemon.drainThenStopAuth(ctx, d.Owner, d.Auth)
	}
}
