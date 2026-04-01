package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/handarbeit/fabrik/config"
	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/stages"
)

// testReadyCh is set by tests to receive a signal once engine.Run has
// registered its signal handlers. This prevents SIGINT from arriving before
// signal.Notify is installed, which would terminate the process.
var testReadyCh chan struct{}

type Config struct {
	Owner         string
	Repo          string
	ProjectNum    int
	User          string
	Token         string
	StagesDir     string
	Yolo          bool
	AutoUpgrade   bool
	PollSeconds   int
	MaxConcurrent int
}

func Execute() error {
	cfg := &Config{}

	flag.StringVar(&cfg.Owner, "owner", "", "GitHub repository owner")
	flag.StringVar(&cfg.Repo, "repo", "", "GitHub repository name")
	flag.IntVar(&cfg.ProjectNum, "project", 0, "GitHub project number")
	flag.StringVar(&cfg.User, "user", "", "GitHub username (only process changes by this user)")
	flag.StringVar(&cfg.Token, "token", "", "GitHub token (or set GITHUB_TOKEN env var)")
	flag.StringVar(&cfg.StagesDir, "stages", "./stages", "Directory containing stage YAML configs")
	flag.BoolVar(&cfg.Yolo, "yolo", false, "Auto-advance issues through stages without waiting for human input")
	flag.BoolVar(&cfg.AutoUpgrade, "auto-upgrade", false, "When idle, check for new commits on origin/main and self-upgrade (for fabrik developing itself)")
	flag.IntVar(&cfg.PollSeconds, "poll", 30, "Polling interval in seconds")

	flag.Parse()

	// Load .env file if present (fatal if .env exists but not in .gitignore)
	if err := config.LoadDotenv(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Token: flag > FABRIK_TOKEN > GITHUB_TOKEN
	if cfg.Token == "" {
		cfg.Token = config.Token()
	}

	// Allow env vars (from .env or shell) to fill in missing flags
	if cfg.Owner == "" {
		cfg.Owner = os.Getenv("FABRIK_OWNER")
	}
	if cfg.Repo == "" {
		cfg.Repo = os.Getenv("FABRIK_REPO")
	}
	if cfg.ProjectNum == 0 {
		if v := os.Getenv("FABRIK_PROJECT_NUMBER"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("FABRIK_PROJECT_NUMBER=%q is invalid (must be a positive integer)", v)
			}
			cfg.ProjectNum = n
		}
	}
	if cfg.User == "" {
		cfg.User = os.Getenv("FABRIK_USER")
	}
	if cfg.StagesDir == "./stages" {
		if v := os.Getenv("FABRIK_STAGES"); v != "" {
			cfg.StagesDir = v
		}
	}
	if !cfg.Yolo {
		if v := os.Getenv("FABRIK_YOLO"); v != "" {
			lv := strings.ToLower(v)
			cfg.Yolo = lv == "true" || lv == "1" || lv == "yes"
		}
	}
	if cfg.PollSeconds == 30 {
		if v := os.Getenv("FABRIK_POLL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.PollSeconds = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_POLL=%q is invalid (must be a positive integer); using default %d\n", v, cfg.PollSeconds)
			}
		}
	}

	// Max concurrent from env, default 5
	cfg.MaxConcurrent = 5
	if v := os.Getenv("FABRIK_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConcurrent = n
		} else {
			fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_CONCURRENT=%q is invalid (must be a positive integer); using default %d\n", v, cfg.MaxConcurrent)
		}
	}

	if cfg.Owner == "" || cfg.Repo == "" || cfg.ProjectNum == 0 {
		fmt.Fprintf(os.Stderr, "Usage: fabrik --owner OWNER --repo REPO --project NUM [options]\n\n")
		flag.PrintDefaults()
		return fmt.Errorf("missing required config: owner, repo, project (use flags or .env file)")
	}

	if cfg.Token == "" {
		return fmt.Errorf("GitHub token required: use --token, FABRIK_TOKEN, or GITHUB_TOKEN")
	}

	if cfg.User == "" {
		return fmt.Errorf("user is required: use --user flag or FABRIK_USER in .env")
	}

	// Load stage configurations
	stageCfgs, err := stages.LoadAll(cfg.StagesDir)
	if err != nil {
		return fmt.Errorf("loading stages from %s: %w", cfg.StagesDir, err)
	}

	if len(stageCfgs) == 0 {
		return fmt.Errorf("no stage configurations found in %s", cfg.StagesDir)
	}

	fmt.Printf("Fabrik starting\n")
	fmt.Printf("  repo:    %s/%s\n", cfg.Owner, cfg.Repo)
	fmt.Printf("  project: #%d\n", cfg.ProjectNum)
	fmt.Printf("  user:    %s\n", cfg.User)
	fmt.Printf("  stages:  %d loaded\n", len(stageCfgs))
	fmt.Printf("  yolo:    %v\n", cfg.Yolo)
	fmt.Printf("  auto-upgrade: %v\n", cfg.AutoUpgrade)
	fmt.Printf("  poll:    %ds\n", cfg.PollSeconds)
	fmt.Printf("  workers: %d\n", cfg.MaxConcurrent)

	eng, err := engine.New(engine.Config{
		Owner:         cfg.Owner,
		Repo:          cfg.Repo,
		ProjectNum:    cfg.ProjectNum,
		User:          cfg.User,
		Token:         cfg.Token,
		Yolo:          cfg.Yolo,
		AutoUpgrade:   cfg.AutoUpgrade,
		PollSeconds:   cfg.PollSeconds,
		MaxConcurrent: cfg.MaxConcurrent,
		Stages:        stageCfgs,
		ReadyCh:       testReadyCh,
	})
	if err != nil {
		return err
	}

	return eng.Run()
}
