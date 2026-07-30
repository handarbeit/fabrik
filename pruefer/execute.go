package pruefer

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/handarbeit/fabrik/config"
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
	if len(cfg.WatchedRepos) == 0 {
		return fmt.Errorf("no watched repos configured (set watched_repos, PRUEFER_REPOS, or --repos)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	auth.RunRefreshLoops(ctx, func(installationID int64) func(format string, args ...any) {
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
		clients[owner] = client
	}

	daemon := &Daemon{
		Clients:  clients,
		Claude:   &RealClaudeInvoker{},
		Clone:    CloneForReview,
		Config:   cfg,
		BotLogin: auth.BotLogin(),
	}

	if useTUI(cfg) {
		return runTUI(ctx, daemon)
	}
	return daemon.Run(ctx)
}
