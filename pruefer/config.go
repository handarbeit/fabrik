// Package pruefer implements the Pruefer V1 daemon: a self-hosted PR review
// bot that watches configured GitHub repositories, reviews pull requests by
// invoking the claude CLI, and submits formal pull_request_review comments.
// It mirrors the architectural shape of Fabrik's engine (poll loop, claude
// CLI invocation, GitHub REST client) but shares no Go imports with engine —
// see adrs/1113-pruefer-v1-architecture.md.
package pruefer

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults, justified in adrs/1113-pruefer-v1-architecture.md.
const (
	DefaultPollInterval   = 120 * time.Second
	DefaultModel          = "sonnet"
	DefaultEffort         = "medium"
	DefaultConcurrencyCap = 3
	DefaultMaxDiffBytes   = 500_000 // 500 KB
	DefaultConfigPath     = ".pruefer/config.yaml"
	DefaultPrivateKeyPath = ".pruefer/app-private-key.pem"
)

// Config holds Pruefer's fully-resolved runtime configuration, after
// applying the flag > env > YAML file > default precedence chain in
// LoadConfig.
type Config struct {
	WatchedRepos    []string // "owner/repo"
	PollInterval    time.Duration
	Model           string
	Effort          string
	ConcurrencyCap  int
	MaxDiffBytes    int64
	// MaxWallTime caps a single claude review invocation's wall-clock
	// duration. Zero means no cap (only the fixed 15-minute inactivity
	// watchdog applies), matching Fabrik's own stage `max_wall_time`
	// convention: absent/zero = uncapped.
	MaxWallTime     time.Duration
	ExcludedAuthors []string
	ExcludedPaths   []string // glob patterns; a PR is skipped only if ALL touched paths match
	ExcludedLabels  []string // a PR is skipped if ANY label matches

	// TUI controls whether Execute launches the bubbletea dashboard. Default
	// true; -notui / PRUEFER_TUI=0 / config.yaml's `tui: false` disable it,
	// mirroring cmd/root.go's --notui/FABRIK_TUI convention. Execute further
	// gates this on a real terminal being detected (see tui_run.go's useTUI).
	TUI bool

	AppID             int64
	AppPrivateKeyPath string
	// AppInstallationID pins Pruefer to a single installation. Zero means
	// auto-discover every installation of the App (see pruefer/auth.go).
	AppInstallationID int64
}

// yamlConfig is the shape of Pruefer's YAML config file. All fields are
// optional; pointer types distinguish "absent" from "explicit zero" for
// numeric fields, mirroring config.ProjectConfig's convention.
type yamlConfig struct {
	WatchedRepos      []string `yaml:"watched_repos"`
	PollIntervalSec   *int     `yaml:"poll_interval_seconds"`
	Model             string   `yaml:"model"`
	Effort            string   `yaml:"effort"`
	ConcurrencyCap    *int     `yaml:"concurrency_cap"`
	MaxDiffBytes      *int64   `yaml:"max_diff_bytes"`
	MaxWallTimeSec    *int     `yaml:"max_wall_time_seconds"`
	ExcludedAuthors   []string `yaml:"excluded_authors"`
	ExcludedPaths     []string `yaml:"excluded_paths"`
	ExcludedLabels    []string `yaml:"excluded_labels"`
	AppID             *int64   `yaml:"github_app_id"`
	AppPrivateKeyPath string   `yaml:"github_app_private_key_path"`
	AppInstallationID *int64   `yaml:"github_app_installation_id"`
	TUI               *bool    `yaml:"tui"`
}

// loadYAMLConfig reads path, returning a zero-value yamlConfig (no error) if
// the file does not exist.
func loadYAMLConfig(path string) (yamlConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return yamlConfig{}, nil
	}
	if err != nil {
		return yamlConfig{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var yc yamlConfig
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return yamlConfig{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return yc, nil
}

// flagValues holds the raw values parsed by LoadConfig's flag.FlagSet.
type flagValues struct {
	repos             string
	pollIntervalSec   int
	model             string
	effort            string
	concurrencyCap    int
	maxDiffBytes      int64
	maxWallTimeSec    int
	excludedAuthors   string
	excludedPaths     string
	excludedLabels    string
	appID             int64
	appPrivateKeyPath string
	appInstallationID int64
	configPath        string
	noTUI             bool
}

// LoadConfig resolves Pruefer's configuration from, in increasing priority:
// defaults, the YAML config file (PRUEFER_CONFIG env var or --config flag,
// default .pruefer/config.yaml), PRUEFER_* environment variables, then
// command-line flags — mirroring cmd/root.go's flag > env > config file >
// default precedence convention. args is the program's argument list
// excluding argv[0] (os.Args[1:] in production; injectable for tests).
func LoadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("pruefer", flag.ContinueOnError)
	var fv flagValues
	fs.StringVar(&fv.repos, "repos", "", "Comma-separated list of owner/repo to watch")
	fs.IntVar(&fv.pollIntervalSec, "poll-interval", 0, "Poll interval in seconds")
	fs.StringVar(&fv.model, "model", "", "Claude model to use for reviews")
	fs.StringVar(&fv.effort, "effort", "", "Claude thinking effort level (low, medium, high, max)")
	fs.IntVar(&fv.concurrencyCap, "concurrency", 0, "Max concurrent claude invocations")
	fs.Int64Var(&fv.maxDiffBytes, "max-diff-bytes", 0, "Skip PRs whose diff exceeds this many bytes")
	fs.IntVar(&fv.maxWallTimeSec, "max-wall-time", 0, "Wall-clock cap in seconds for a single claude review invocation (0 = no cap)")
	fs.StringVar(&fv.excludedAuthors, "excluded-authors", "", "Comma-separated PR authors to skip")
	fs.StringVar(&fv.excludedPaths, "excluded-paths", "", "Comma-separated path globs to skip (all touched paths must match)")
	fs.StringVar(&fv.excludedLabels, "excluded-labels", "", "Comma-separated labels to skip (any match)")
	fs.Int64Var(&fv.appID, "github-app-id", 0, "GitHub App ID")
	fs.StringVar(&fv.appPrivateKeyPath, "github-app-private-key-path", "", "Path to the GitHub App's PEM private key")
	fs.Int64Var(&fv.appInstallationID, "github-app-installation-id", 0, "GitHub App installation ID (0 = auto-discover)")
	fs.StringVar(&fv.configPath, "config", DefaultConfigPath, "Path to Pruefer's YAML config file")
	fs.BoolVar(&fv.noTUI, "notui", false, "Disable the interactive TUI dashboard (default: enabled when a real terminal is detected)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	configPath := fv.configPath
	if !explicit["config"] {
		if v := os.Getenv("PRUEFER_CONFIG"); v != "" {
			configPath = v
		}
	}

	yc, err := loadYAMLConfig(configPath)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		WatchedRepos:      yc.WatchedRepos,
		PollInterval:      DefaultPollInterval,
		Model:             DefaultModel,
		Effort:            DefaultEffort,
		ConcurrencyCap:    DefaultConcurrencyCap,
		MaxDiffBytes:      DefaultMaxDiffBytes,
		ExcludedAuthors:   yc.ExcludedAuthors,
		ExcludedPaths:     yc.ExcludedPaths,
		ExcludedLabels:    yc.ExcludedLabels,
		AppPrivateKeyPath: DefaultPrivateKeyPath,
		TUI:               true,
	}
	if yc.TUI != nil {
		cfg.TUI = *yc.TUI
	}
	if yc.PollIntervalSec != nil {
		cfg.PollInterval = time.Duration(*yc.PollIntervalSec) * time.Second
	}
	if yc.Model != "" {
		cfg.Model = yc.Model
	}
	if yc.Effort != "" {
		cfg.Effort = yc.Effort
	}
	if yc.ConcurrencyCap != nil {
		cfg.ConcurrencyCap = *yc.ConcurrencyCap
	}
	if yc.MaxDiffBytes != nil {
		cfg.MaxDiffBytes = *yc.MaxDiffBytes
	}
	if yc.MaxWallTimeSec != nil {
		cfg.MaxWallTime = time.Duration(*yc.MaxWallTimeSec) * time.Second
	}
	if yc.AppID != nil {
		cfg.AppID = *yc.AppID
	}
	if yc.AppPrivateKeyPath != "" {
		cfg.AppPrivateKeyPath = yc.AppPrivateKeyPath
	}
	if yc.AppInstallationID != nil {
		cfg.AppInstallationID = *yc.AppInstallationID
	}

	applyEnv(&cfg)

	if explicit["repos"] {
		cfg.WatchedRepos = splitCSV(fv.repos)
	}
	if explicit["poll-interval"] {
		cfg.PollInterval = time.Duration(fv.pollIntervalSec) * time.Second
	}
	if explicit["model"] {
		cfg.Model = fv.model
	}
	if explicit["effort"] {
		cfg.Effort = fv.effort
	}
	if explicit["concurrency"] {
		cfg.ConcurrencyCap = fv.concurrencyCap
	}
	if explicit["max-diff-bytes"] {
		cfg.MaxDiffBytes = fv.maxDiffBytes
	}
	if explicit["max-wall-time"] {
		cfg.MaxWallTime = time.Duration(fv.maxWallTimeSec) * time.Second
	}
	if explicit["excluded-authors"] {
		cfg.ExcludedAuthors = splitCSV(fv.excludedAuthors)
	}
	if explicit["excluded-paths"] {
		cfg.ExcludedPaths = splitCSV(fv.excludedPaths)
	}
	if explicit["excluded-labels"] {
		cfg.ExcludedLabels = splitCSV(fv.excludedLabels)
	}
	if explicit["github-app-id"] {
		cfg.AppID = fv.appID
	}
	if explicit["github-app-private-key-path"] {
		cfg.AppPrivateKeyPath = fv.appPrivateKeyPath
	}
	if explicit["github-app-installation-id"] {
		cfg.AppInstallationID = fv.appInstallationID
	}
	if explicit["notui"] {
		cfg.TUI = !fv.noTUI
	}

	return cfg, nil
}

// applyEnv overlays PRUEFER_* environment variables onto cfg, between the
// YAML file and flag layers of the precedence chain.
func applyEnv(cfg *Config) {
	if v := os.Getenv("PRUEFER_REPOS"); v != "" {
		cfg.WatchedRepos = splitCSV(v)
	}
	if v := os.Getenv("PRUEFER_POLL_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PollInterval = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("PRUEFER_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("PRUEFER_EFFORT"); v != "" {
		cfg.Effort = v
	}
	if v := os.Getenv("PRUEFER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ConcurrencyCap = n
		}
	}
	if v := os.Getenv("PRUEFER_MAX_DIFF_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MaxDiffBytes = n
		}
	}
	if v := os.Getenv("PRUEFER_MAX_WALL_TIME"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxWallTime = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("PRUEFER_EXCLUDED_AUTHORS"); v != "" {
		cfg.ExcludedAuthors = splitCSV(v)
	}
	if v := os.Getenv("PRUEFER_EXCLUDED_PATHS"); v != "" {
		cfg.ExcludedPaths = splitCSV(v)
	}
	if v := os.Getenv("PRUEFER_EXCLUDED_LABELS"); v != "" {
		cfg.ExcludedLabels = splitCSV(v)
	}
	if v := os.Getenv("PRUEFER_GITHUB_APP_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.AppID = n
		}
	}
	if v := os.Getenv("PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH"); v != "" {
		cfg.AppPrivateKeyPath = v
	}
	if v := os.Getenv("PRUEFER_GITHUB_APP_INSTALLATION_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.AppInstallationID = n
		}
	}
	if v := os.Getenv("PRUEFER_TUI"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.TUI = b
		}
	}
}

// splitCSV splits a comma-separated string into a trimmed, non-empty slice.
// Returns nil for an empty input.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
