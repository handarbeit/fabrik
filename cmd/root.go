package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/stages"
)

type Config struct {
	Owner       string
	Repo        string
	ProjectNum  int
	User        string
	Token       string
	StagesDir     string
	Yolo          bool
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
	flag.IntVar(&cfg.PollSeconds, "poll", 30, "Polling interval in seconds")

	flag.Parse()

	// Token from env if not provided
	if cfg.Token == "" {
		cfg.Token = os.Getenv("GITHUB_TOKEN")
	}

	// Max concurrent from env, default 5
	cfg.MaxConcurrent = 5
	if v := os.Getenv("FABRIK_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConcurrent = n
		}
	}

	if cfg.Owner == "" || cfg.Repo == "" || cfg.ProjectNum == 0 {
		fmt.Fprintf(os.Stderr, "Usage: fabrik --owner OWNER --repo REPO --project NUM [options]\n\n")
		flag.PrintDefaults()
		return fmt.Errorf("missing required flags: --owner, --repo, --project")
	}

	if cfg.Token == "" {
		return fmt.Errorf("GitHub token required: use --token or set GITHUB_TOKEN env var")
	}

	if cfg.User == "" {
		return fmt.Errorf("--user is required (your GitHub username)")
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
	fmt.Printf("  poll:    %ds\n", cfg.PollSeconds)
	fmt.Printf("  workers: %d\n", cfg.MaxConcurrent)

	eng, err := engine.New(engine.Config{
		Owner:         cfg.Owner,
		Repo:          cfg.Repo,
		ProjectNum:    cfg.ProjectNum,
		User:          cfg.User,
		Token:         cfg.Token,
		Yolo:          cfg.Yolo,
		PollSeconds:   cfg.PollSeconds,
		MaxConcurrent: cfg.MaxConcurrent,
		Stages:        stageCfgs,
	})
	if err != nil {
		return err
	}

	return eng.Run()
}
