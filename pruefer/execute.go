package pruefer

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
)

// Execute is Pruefer's entry point: loads .env, resolves configuration,
// bootstraps GitHub App auth, constructs the GitHub client and Daemon, and
// runs until interrupted (SIGINT/SIGTERM). Mirrors cmd.Execute()'s shape
// for the main fabrik binary (see cmd/root.go), scaled down to Pruefer's
// needs — no board/stage machinery, no TUI.
func Execute() error {
	if err := config.LoadDotenv(); err != nil {
		return fmt.Errorf("loading .env: %w", err)
	}

	cfg, err := LoadConfig(os.Args[1:])
	if err != nil {
		return err
	}
	if cfg.AppID == 0 {
		return fmt.Errorf("github_app_id is required (set via config file, PRUEFER_GITHUB_APP_ID, or --github-app-id)")
	}
	if len(cfg.WatchedRepos) == 0 {
		return fmt.Errorf("no watched repos configured (set watched_repos, PRUEFER_REPOS, or --repos)")
	}

	client := gh.NewClient("")
	auth, err := Bootstrap(cfg, client, "")
	if err != nil {
		return fmt.Errorf("bootstrapping GitHub App auth: %w", err)
	}
	logf(0, "auth", "authenticated as %s (installation %d)\n", auth.BotLogin, auth.InstallationID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go auth.RunRefreshLoop(ctx, func(format string, args ...any) {
		logf(0, "auth", format, args...)
	})

	daemon := &Daemon{
		Client:   client,
		Claude:   &RealClaudeInvoker{},
		Clone:    CloneForReview,
		Config:   cfg,
		BotLogin: auth.BotLogin,
	}
	return daemon.Run(ctx)
}
