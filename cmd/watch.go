package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/handarbeit/fabrik/config"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/watch"
)

// runWatch is the entry point for the `fabrik watch <issue-number>` subcommand.
func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	owner := fs.String("owner", "", "GitHub repository owner (or FABRIK_OWNER)")
	repo := fs.String("repo", "", "GitHub repository name (or FABRIK_REPO)")
	token := fs.String("token", "", "GitHub token (or FABRIK_TOKEN / GITHUB_TOKEN)")
	pluginDir := fs.String("plugin-dir", "", "Path to Fabrik plugin directory (or FABRIK_PLUGIN_DIR)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: fabrik watch <issue-number>")
	}
	issueNumber, err := strconv.Atoi(fs.Arg(0))
	if err != nil || issueNumber <= 0 {
		return fmt.Errorf("issue-number must be a positive integer, got %q", fs.Arg(0))
	}

	// Load .env and project config (best-effort; non-fatal if absent).
	_ = config.LoadDotenv()
	pc, _ := config.LoadProjectConfig()

	// Resolve owner from: flag > env > config.yaml
	if *owner == "" {
		if v := os.Getenv("FABRIK_OWNER"); v != "" {
			*owner = v
		} else if pc.Owner != "" {
			*owner = pc.Owner
		}
	}
	// Resolve repo from: flag > env > config.yaml
	if *repo == "" {
		if v := os.Getenv("FABRIK_REPO"); v != "" {
			*repo = v
		} else if pc.Repo != "" {
			*repo = pc.Repo
		}
	}
	// Resolve token
	if *token == "" {
		*token = config.Token()
	}
	// Resolve plugin-dir from: flag > env
	if *pluginDir == "" {
		if v := os.Getenv("FABRIK_PLUGIN_DIR"); v != "" {
			*pluginDir = v
		}
	}

	// Resolve poll interval (seconds): env > config.yaml > default 30s
	pollSeconds := 30
	if v := os.Getenv("FABRIK_POLL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pollSeconds = n
		}
	} else if pc.Poll != nil && *pc.Poll > 0 {
		pollSeconds = *pc.Poll
	}

	// Build GitHub client (may be nil if no token, degrading gracefully).
	var client *gh.Client
	if *token != "" {
		client = gh.NewClient(*token)
	} else {
		fmt.Fprintf(os.Stderr, "[warn] no GitHub token available; PR/CI status will not be shown\n")
	}

	if *owner == "" || *repo == "" {
		fmt.Fprintf(os.Stderr, "[warn] --owner/--repo not set; GitHub metadata will not be fetched\n")
	}

	opts := watch.GitHubOptions{
		Owner:        *owner,
		Repo:         *repo,
		Client:       client,
		PollInterval: time.Duration(pollSeconds) * time.Second,
		PluginDir:    *pluginDir,
	}

	model := watch.NewModel(issueNumber, opts)
	return watch.Run(model)
}
