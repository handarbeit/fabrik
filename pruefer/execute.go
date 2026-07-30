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

	cfg, err := LoadConfig(os.Args[1:])
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
		hookdeck.Logf = func(format string, args ...any) {
			logf(0, "hookdeck", format, args...)
		}
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
