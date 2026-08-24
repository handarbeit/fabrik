package pruefer

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/handarbeit/fabrik/config"
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
	if cfg.AppID == 0 {
		return fmt.Errorf("github_app_id is required (set via config file, PRUEFER_GITHUB_APP_ID, or --github-app-id)")
	}
	if len(cfg.WatchedRepos) == 0 {
		return fmt.Errorf("no watched repos configured (set watched_repos, PRUEFER_REPOS, or --repos)")
	}

	authSet, err := BootstrapMulti(cfg, "")
	if err != nil {
		return fmt.Errorf("bootstrapping GitHub App auth: %w", err)
	}
	owners := make([]string, 0, len(authSet.Clients))
	for owner := range authSet.Clients {
		owners = append(owners, owner)
	}
	logf(0, "auth", "authenticated as %s (%d installation(s), owners: %s)\n", authSet.BotLogin, authSet.InstallationCount(), strings.Join(owners, ", "))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authSet.RunRefreshLoops(ctx, func(installationID int64) func(format string, args ...any) {
		prefix := fmt.Sprintf("installation %d: ", installationID)
		return func(format string, args ...any) {
			logf(0, "auth", prefix+format, args...)
		}
	})

	clients := make(map[string]GitHubLister, len(authSet.Clients))
	for owner, client := range authSet.Clients {
		clients[owner] = client
	}

	daemon, closeLog := NewDaemon(cfg, clients, &RealClaudeInvoker{}, CloneForReview, authSet.BotLogin)
	defer closeLog()

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

	// SIGHUP triggers a config reload (#1640) — deliberately a separate
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
				handleReload(ctx, args, authSet, daemon)
			}
		}
	}()

	if useTUI(cfg) {
		return runTUI(ctx, daemon)
	}
	return daemon.Run(ctx)
}

// handleReload processes one SIGHUP (#1640): re-reads config via
// LoadConfig(args), merges it against daemon's currently running config
// (applyConfigReload — R2/R4), mints a token for any newly-watched owner
// (R5, all-or-nothing across the whole reload — see AuthSet.mintOwnerAuth's
// doc comment), and only once every step has succeeded does it apply the
// merged config (and any newly-minted clients) to daemon and log a
// diff-style summary (R6).
//
// A failure at any step — a malformed/invalid config file, or a newly
// watched owner with no matching App installation — leaves daemon and
// authSet completely untouched (R4/AC3/AC5): nothing is committed until
// every owner in the batch has minted successfully. Nothing here cancels a
// running review, drops the Hookdeck EventSource, or resets the dedupe
// ring (R3) — those are untouched by construction, since this function
// never rebuilds or reaches into any of them. This includes a removed
// owner's installation token: its refresh loop is only ever stopped once
// Daemon.drainThenStopAuth (spawned below) has confirmed no review is still
// in flight for that owner, so an in-flight review's remaining GitHub calls
// never lose a valid token out from under them.
func handleReload(ctx context.Context, args []string, authSet *AuthSet, daemon *Daemon) {
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
		// the full flag/env/YAML chain every call (R2's documented
		// caveat), so this is defensive, not expected to fire in practice.
		logf(0, "reload", "config reload failed: --version is not a reloadable configuration — keeping current config\n")
		return
	}

	old := daemon.config()
	merged, diff := applyConfigReload(old, cand)

	newOwners := newlyWatchedOwners(old, merged)
	type mintedOwner struct {
		owner       string
		auth        *Auth
		mintedFresh bool
	}
	minted := make([]mintedOwner, 0, len(newOwners))
	for _, owner := range newOwners {
		a, fresh, err := authSet.mintOwnerAuth(merged, owner)
		if err != nil {
			logf(0, "reload", "config reload failed: %v — keeping current config\n", err)
			return
		}
		minted = append(minted, mintedOwner{owner: owner, auth: a, mintedFresh: fresh})
	}

	addedClients := make(map[string]GitHubLister, len(minted))
	for _, m := range minted {
		authSet.CommitOwner(m.owner, m.auth, m.mintedFresh)
		addedClients[m.owner] = authSet.Clients[m.owner]
		if m.mintedFresh {
			installationID := m.auth.InstallationID
			prefix := fmt.Sprintf("installation %d: ", installationID)
			m.auth.startRefreshLoop(ctx, func(format string, args ...any) {
				logf(0, "auth", prefix+format, args...)
			})
		}
	}

	// Owners no longer present in watched_repos are pruned only now, after
	// every newly-watched owner in this same batch has already minted
	// successfully — an earlier prune (e.g. right after computing merged)
	// would violate R4/AC3's atomicity if a later owner's mint then failed
	// and aborted the whole reload above: the removed owner's client would
	// already be gone even though the reload as a whole never took effect.
	// pruneOwners only detaches each removed (non-pinned) owner's *Auth from
	// AuthSet's own bookkeeping — it deliberately does NOT stop its refresh
	// goroutine yet. That happens below, once drainThenStopAuth confirms no
	// review is still in flight for the owner being removed.
	removedOwners := noLongerWatchedOwners(old, merged)
	detached := authSet.pruneOwners(removedOwners)

	daemon.ApplyReload(merged, addedClients, removedOwners)
	logReloadSummary(diff)

	for _, d := range detached {
		go daemon.drainThenStopAuth(ctx, d.owner, d.auth)
	}
}
