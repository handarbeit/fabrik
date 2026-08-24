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
	"reflect"
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
	DefaultLogPath        = ".pruefer/pruefer.log"
)

// Defaults for the event-driven (Hookdeck) ingestion surface, justified in
// adrs/1254-*.md. EventSourcePoll is the default: with it selected,
// Pruefer's behavior is byte-for-byte unchanged from before this surface
// existed.
const (
	EventSourcePoll     = "poll"
	EventSourceHookdeck = "hookdeck"

	DefaultEventSource                    = EventSourcePoll
	DefaultHookdeckAPIKeyEnv              = "HOOKDECK_API_KEY"
	DefaultHookdeckWebhookSecretEnv       = "PRUEFER_GITHUB_WEBHOOK_SECRET"
	DefaultReconciliationStartup          = true
	DefaultReconciliationFallbackInterval = 2 * time.Minute
)

// Config holds Pruefer's fully-resolved runtime configuration, after
// applying the flag > env > YAML file > default precedence chain in
// LoadConfig.
//
// Every field carries a `reload:"live|restart|skip"` struct tag — the single
// source of truth applyConfigReload uses both to decide which changed
// fields it may write into a running Daemon on SIGHUP (see
// pruefer/execute.go's handleReload) and to render the R6 diff summary.
// TestConfig_AllFieldsClassified asserts every field has one, so adding a
// field without a tag fails the build's tests rather than silently
// defaulting to some behavior. See adrs/1640-pruefer-config-reload.md.
type Config struct {
	// WatchedRepos is tagged "live" but is never applied through the
	// generic reflection loop in applyConfigReload — it's diffed and
	// applied specially (diffRepos) so R6's summary can name individual
	// repos added/removed rather than a single before/after slice dump.
	WatchedRepos   []string      `reload:"live"` // "owner/repo"
	PollInterval   time.Duration `reload:"live"`
	Model          string        `reload:"live"`
	Effort         string        `reload:"live"`
	ConcurrencyCap int           `reload:"live"`
	MaxDiffBytes   int64         `reload:"live"`
	// MaxWallTime caps a single claude review invocation's wall-clock
	// duration. Zero means no cap (only the fixed 15-minute inactivity
	// watchdog applies), matching Fabrik's own stage `max_wall_time`
	// convention: absent/zero = uncapped.
	MaxWallTime     time.Duration `reload:"live"`
	ExcludedAuthors []string      `reload:"live"`
	ExcludedPaths   []string      `reload:"live"` // glob patterns, applied per file before max_diff_bytes is measured (#1462); a PR is skipped whole only if ALL touched paths match
	ExcludedLabels  []string      `reload:"live"` // a PR is skipped if ANY label matches

	// RequestChangesThreshold gates Pruefer's severity-based REQUEST_CHANGES
	// escalation (see decideEvent in pruefer/review.go). The zero value
	// ("") means the feature is off: every review submits COMMENT,
	// byte-for-byte the pre-#1251 behavior. Setting it to one of the four
	// recognized Severity tiers turns the gate on at that threshold —
	// LoadConfig fails loud on any other non-empty value (a mistyped
	// config value is an operator error worth catching at startup, unlike
	// a per-finding severity from Claude, which fails closed instead; see
	// severityRank's doc comment).
	RequestChangesThreshold Severity `reload:"live"`

	// TUI controls whether Execute launches the bubbletea dashboard. Default
	// true; -notui / PRUEFER_TUI=0 / config.yaml's `tui: false` disable it,
	// mirroring cmd/root.go's --notui/FABRIK_TUI convention. Execute further
	// gates this on a real terminal being detected (see tui_run.go's useTUI).
	// Restart-only: the bubbletea program is started once, synchronously, in
	// Execute before Daemon.Run is ever called — there is no live surface to
	// toggle it against.
	TUI bool `reload:"restart"`

	// LogFile is the path Pruefer's daemon log lines are written to, resolved
	// relative to the process cwd (the same way DefaultConfigPath and
	// AppPrivateKeyPath are resolved). Defaults to DefaultLogPath. An
	// explicitly empty value (YAML `log_file: ""`, PRUEFER_LOG_FILE=, or
	// --log-file "") disables file logging entirely. Restart-only: the log
	// file is opened once by NewDaemon and wired into the package-level Logf
	// hook; reopening it live is out of scope for this issue.
	LogFile string `reload:"restart"`

	AppID             int64  `reload:"restart"`
	AppPrivateKeyPath string `reload:"restart"`
	// AppInstallationID is a legacy pin/escape hatch: when set (non-zero),
	// Pruefer skips owner-derived discovery entirely and routes every
	// watched repo, regardless of owner, through this one installation's
	// token — preserving pre-multi-installation behavior exactly. Zero (the
	// default) means resolve one installation per distinct owner present in
	// WatchedRepos instead (see BootstrapMulti in pruefer/auth.go).
	// Restart-only: it governs which App identity BootstrapMulti resolves at
	// startup, not something a reload can safely re-derive.
	AppInstallationID int64 `reload:"restart"`

	// AutoUpgrade enables self-upgrade checks at the poll boundary (see
	// Daemon.checkAndUpgrade in pruefer/upgrade.go). Default false, mirroring
	// cmd/root.go's -auto-upgrade default — an operator opts in deliberately
	// rather than the daemon silently replacing its own binary. See
	// ADR-1197. Live: runPollOnly re-reads it every loop iteration.
	AutoUpgrade bool `reload:"live"`

	// VersionRequested is set when --version is passed. Execute checks this
	// before validating required config (AppID/WatchedRepos) so `pruefer
	// --version` works without a fully configured environment. Never a real
	// runtime setting to reload — handleReload rejects a reload candidate
	// that sets this rather than classifying it live or restart.
	VersionRequested bool `reload:"skip"`

	// EventSource selects Pruefer's event ingestion mode: EventSourcePoll
	// (default — behavior is byte-for-byte unchanged from before this
	// field existed) or EventSourceHookdeck (event-driven, with poll
	// demoted to a low-frequency reconciliation fallback — see the
	// Reconciliation* fields below). See adrs/1254-*.md. Restart-only:
	// switching modes means constructing (or tearing down) a whole
	// events.EventSource, including its Hookdeck WebSocket session and
	// dedupe ring (R3) — out of scope for a live reload.
	EventSource string `reload:"restart"`

	// HookdeckAPIKeyEnv names the environment variable holding the
	// Hookdeck API key. Only consulted when EventSource ==
	// EventSourceHookdeck. Restart-only: read once in Execute to resolve a
	// concrete secret value baked into the already-constructed
	// hookdeck.Config; changing which env var to read requires
	// reconstructing the EventSource.
	HookdeckAPIKeyEnv string `reload:"restart"`
	// HookdeckWebhookSecretEnv names the environment variable holding the
	// GitHub App's webhook secret, used to verify every forwarded
	// delivery's signature — required regardless of Hookdeck's own
	// transport auth. Only consulted when EventSource ==
	// EventSourceHookdeck. Restart-only, for the same reason as
	// HookdeckAPIKeyEnv above.
	HookdeckWebhookSecretEnv string `reload:"restart"`

	// ReconciliationStartup controls whether an event-driven run performs
	// a full poll reconciliation pass at startup, before event delivery
	// begins. Only consulted when EventSource == EventSourceHookdeck.
	// Restart-only: read exactly once, synchronously, before
	// runEventDriven's loop begins — by the time any SIGHUP could be
	// handled concurrently with a running daemon, that read has already
	// happened, so a reload can structurally never affect it.
	ReconciliationStartup bool `reload:"restart"`
	// ReconciliationFallbackInterval is the low-frequency poll interval
	// used as a safety net in event-driven mode — a separate field from
	// PollInterval, which remains poll-only mode's interval, unaffected by
	// this one. Only consulted when EventSource == EventSourceHookdeck.
	// Live: runEventDriven calls ticker.Reset when it detects a change.
	ReconciliationFallbackInterval time.Duration `reload:"live"`
}

// yamlConfig is the shape of Pruefer's YAML config file. All fields are
// optional; pointer types distinguish "absent" from "explicit zero" for
// numeric fields, mirroring config.ProjectConfig's convention.
type yamlConfig struct {
	WatchedRepos            []string `yaml:"watched_repos"`
	PollIntervalSec         *int     `yaml:"poll_interval_seconds"`
	Model                   string   `yaml:"model"`
	Effort                  string   `yaml:"effort"`
	ConcurrencyCap          *int     `yaml:"concurrency_cap"`
	MaxDiffBytes            *int64   `yaml:"max_diff_bytes"`
	MaxWallTimeSec          *int     `yaml:"max_wall_time_seconds"`
	ExcludedAuthors         []string `yaml:"excluded_authors"`
	ExcludedPaths           []string `yaml:"excluded_paths"`
	ExcludedLabels          []string `yaml:"excluded_labels"`
	RequestChangesThreshold string   `yaml:"request_changes_threshold"`
	AppID                   *int64   `yaml:"github_app_id"`
	AppPrivateKeyPath       string   `yaml:"github_app_private_key_path"`
	AppInstallationID       *int64   `yaml:"github_app_installation_id"`
	TUI                     *bool    `yaml:"tui"`
	LogFile                 *string  `yaml:"log_file"`
	AutoUpgrade             *bool    `yaml:"auto_upgrade"`

	EventSource string `yaml:"event_source"`
	Hookdeck    *struct {
		APIKeyEnv        string `yaml:"api_key_env"`
		WebhookSecretEnv string `yaml:"webhook_secret_env"`
	} `yaml:"hookdeck"`
	Reconciliation *struct {
		Startup *bool `yaml:"startup"`
		// FallbackInterval is a Go duration string (e.g. "2m"), matching
		// the issue's literal config example — unlike this file's other
		// duration fields, which use a "_seconds" int convention.
		FallbackInterval string `yaml:"fallback_interval"`
	} `yaml:"reconciliation"`
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
	repos                   string
	pollIntervalSec         int
	model                   string
	effort                  string
	concurrencyCap          int
	maxDiffBytes            int64
	maxWallTimeSec          int
	excludedAuthors         string
	excludedPaths           string
	excludedLabels          string
	requestChangesThreshold string
	appID                   int64
	appPrivateKeyPath       string
	appInstallationID       int64
	configPath              string
	noTUI                   bool
	logFile                 string
	autoUpgrade             bool
	versionRequested        bool

	eventSource                    string
	hookdeckAPIKeyEnv              string
	hookdeckWebhookSecretEnv       string
	reconciliationStartup          bool
	reconciliationFallbackInterval string
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
	fs.StringVar(&fv.excludedPaths, "excluded-paths", "", "Comma-separated path globs, filtered per file before max_diff_bytes is measured; a PR is skipped whole only if all touched paths match")
	fs.StringVar(&fv.excludedLabels, "excluded-labels", "", "Comma-separated labels to skip (any match)")
	fs.StringVar(&fv.requestChangesThreshold, "request-changes-threshold", "", "Severity tier (low, medium, high, critical) at or above which Pruefer submits REQUEST_CHANGES instead of COMMENT; empty disables severity-gated REQUEST_CHANGES entirely")
	fs.Int64Var(&fv.appID, "github-app-id", 0, "GitHub App ID")
	fs.StringVar(&fv.appPrivateKeyPath, "github-app-private-key-path", "", "Path to the GitHub App's PEM private key")
	fs.Int64Var(&fv.appInstallationID, "github-app-installation-id", 0, "GitHub App installation ID (0 = auto-discover)")
	fs.StringVar(&fv.configPath, "config", DefaultConfigPath, "Path to Pruefer's YAML config file")
	fs.BoolVar(&fv.noTUI, "notui", false, "Disable the interactive TUI dashboard (default: enabled when a real terminal is detected)")
	fs.StringVar(&fv.logFile, "log-file", "", "Path to write daemon log lines to (empty disables file logging; default .pruefer/pruefer.log)")
	fs.BoolVar(&fv.autoUpgrade, "auto-upgrade", false, "At each poll boundary (never mid-review), check GitHub Releases for a newer version and self-upgrade; dev builds (built from the fabrik source checkout) rebuild from origin/main instead")
	fs.BoolVar(&fv.versionRequested, "version", false, "Print the pruefer version and exit")
	fs.StringVar(&fv.eventSource, "event-source", "", "Event ingestion mode: poll (default) or hookdeck")
	fs.StringVar(&fv.hookdeckAPIKeyEnv, "hookdeck-api-key-env", "", "Environment variable name holding the Hookdeck API key")
	fs.StringVar(&fv.hookdeckWebhookSecretEnv, "hookdeck-webhook-secret-env", "", "Environment variable name holding the GitHub App's webhook secret")
	fs.BoolVar(&fv.reconciliationStartup, "reconciliation-startup", true, "Run a full poll reconciliation pass at startup in event-driven mode")
	fs.StringVar(&fv.reconciliationFallbackInterval, "reconciliation-fallback-interval", "", "Low-frequency poll interval used as a safety net in event-driven mode (Go duration, e.g. 2m)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if fv.versionRequested {
		return Config{VersionRequested: true}, nil
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
		LogFile:           DefaultLogPath,
		AutoUpgrade:       false,

		EventSource:                    DefaultEventSource,
		HookdeckAPIKeyEnv:              DefaultHookdeckAPIKeyEnv,
		HookdeckWebhookSecretEnv:       DefaultHookdeckWebhookSecretEnv,
		ReconciliationStartup:          DefaultReconciliationStartup,
		ReconciliationFallbackInterval: DefaultReconciliationFallbackInterval,
	}
	if yc.RequestChangesThreshold != "" {
		cfg.RequestChangesThreshold = Severity(yc.RequestChangesThreshold)
	}
	if yc.TUI != nil {
		cfg.TUI = *yc.TUI
	}
	if yc.AutoUpgrade != nil {
		cfg.AutoUpgrade = *yc.AutoUpgrade
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
	if yc.LogFile != nil {
		cfg.LogFile = *yc.LogFile
	}
	if yc.EventSource != "" {
		cfg.EventSource = yc.EventSource
	}
	if yc.Hookdeck != nil {
		if yc.Hookdeck.APIKeyEnv != "" {
			cfg.HookdeckAPIKeyEnv = yc.Hookdeck.APIKeyEnv
		}
		if yc.Hookdeck.WebhookSecretEnv != "" {
			cfg.HookdeckWebhookSecretEnv = yc.Hookdeck.WebhookSecretEnv
		}
	}
	if yc.Reconciliation != nil {
		if yc.Reconciliation.Startup != nil {
			cfg.ReconciliationStartup = *yc.Reconciliation.Startup
		}
		if yc.Reconciliation.FallbackInterval != "" {
			d, err := time.ParseDuration(yc.Reconciliation.FallbackInterval)
			if err != nil {
				return Config{}, fmt.Errorf("parsing reconciliation.fallback_interval %q: %w", yc.Reconciliation.FallbackInterval, err)
			}
			cfg.ReconciliationFallbackInterval = d
		}
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
	if explicit["request-changes-threshold"] {
		cfg.RequestChangesThreshold = Severity(fv.requestChangesThreshold)
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
	if explicit["log-file"] {
		cfg.LogFile = fv.logFile
	}
	if explicit["auto-upgrade"] {
		cfg.AutoUpgrade = fv.autoUpgrade
	}
	if explicit["event-source"] {
		cfg.EventSource = fv.eventSource
	}
	if explicit["hookdeck-api-key-env"] {
		if fv.hookdeckAPIKeyEnv == "" {
			return Config{}, fmt.Errorf("-hookdeck-api-key-env cannot be empty")
		}
		cfg.HookdeckAPIKeyEnv = fv.hookdeckAPIKeyEnv
	}
	if explicit["hookdeck-webhook-secret-env"] {
		if fv.hookdeckWebhookSecretEnv == "" {
			return Config{}, fmt.Errorf("-hookdeck-webhook-secret-env cannot be empty")
		}
		cfg.HookdeckWebhookSecretEnv = fv.hookdeckWebhookSecretEnv
	}
	if explicit["reconciliation-startup"] {
		cfg.ReconciliationStartup = fv.reconciliationStartup
	}
	if explicit["reconciliation-fallback-interval"] {
		d, err := time.ParseDuration(fv.reconciliationFallbackInterval)
		if err != nil {
			return Config{}, fmt.Errorf("parsing -reconciliation-fallback-interval %q: %w", fv.reconciliationFallbackInterval, err)
		}
		cfg.ReconciliationFallbackInterval = d
	}

	if cfg.EventSource != EventSourcePoll && cfg.EventSource != EventSourceHookdeck {
		return Config{}, fmt.Errorf("invalid event_source %q: must be %q or %q", cfg.EventSource, EventSourcePoll, EventSourceHookdeck)
	}

	if cfg.RequestChangesThreshold != "" && !validSeverity(cfg.RequestChangesThreshold) {
		return Config{}, fmt.Errorf("request_changes_threshold: %q is not a recognized severity tier (must be one of low, medium, high, critical, or empty to disable)", cfg.RequestChangesThreshold)
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
	if v := os.Getenv("PRUEFER_REQUEST_CHANGES_THRESHOLD"); v != "" {
		cfg.RequestChangesThreshold = Severity(v)
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
	// Unlike every other PRUEFER_* var above, an explicitly empty
	// PRUEFER_LOG_FILE must be distinguished from "unset" (R2: empty
	// disables file logging entirely). os.LookupEnv, not the v != "" idiom
	// used above, is what makes that distinction possible.
	if v, ok := os.LookupEnv("PRUEFER_LOG_FILE"); ok {
		cfg.LogFile = v
	}
	if v := os.Getenv("PRUEFER_AUTO_UPGRADE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AutoUpgrade = b
		}
	}
	if v := os.Getenv("PRUEFER_EVENT_SOURCE"); v != "" {
		cfg.EventSource = v
	}
	if v := os.Getenv("PRUEFER_HOOKDECK_API_KEY_ENV"); v != "" {
		cfg.HookdeckAPIKeyEnv = v
	}
	if v := os.Getenv("PRUEFER_HOOKDECK_WEBHOOK_SECRET_ENV"); v != "" {
		cfg.HookdeckWebhookSecretEnv = v
	}
	if v := os.Getenv("PRUEFER_RECONCILIATION_STARTUP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.ReconciliationStartup = b
		}
	}
	if v := os.Getenv("PRUEFER_RECONCILIATION_FALLBACK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.ReconciliationFallbackInterval = d
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

// reloadClass is the recognized set of values a Config field's `reload`
// struct tag may carry. Anything else (including an absent tag) is treated
// conservatively by applyConfigReload as restart-only — a field is only
// ever live-applied on an explicit, correctly-spelled "live".
type reloadClass string

const (
	reloadLive    reloadClass = "live"
	reloadRestart reloadClass = "restart"
	reloadSkip    reloadClass = "skip"
)

// FieldChange records one Config field's old and new value, formatted for
// the R6 log summary.
type FieldChange struct {
	Field string
	Old   string
	New   string
}

// ReloadDiff summarizes what a reload attempt changed (R6). ReposAdded and
// ReposRemoved report WatchedRepos at repo granularity rather than as a
// single before/after field dump; FieldsChanged covers every other changed
// field that was actually applied; RestartOnlyChanged covers every changed
// field that was reported but deliberately left untouched (R2/AC4).
type ReloadDiff struct {
	ReposAdded         []string
	ReposRemoved       []string
	FieldsChanged      []FieldChange
	RestartOnlyChanged []FieldChange
}

// Empty reports whether the reload produced no observable change at all —
// used by logReloadSummary to log a single "no changes" line instead of an
// empty diff.
func (d ReloadDiff) Empty() bool {
	return len(d.ReposAdded) == 0 && len(d.ReposRemoved) == 0 &&
		len(d.FieldsChanged) == 0 && len(d.RestartOnlyChanged) == 0
}

// diffRepos reports which entries of newRepos are absent from oldRepos
// (added) and which entries of oldRepos are absent from newRepos (removed).
// Order-insensitive: reordering watched_repos with no membership change
// reports no diff.
func diffRepos(oldRepos, newRepos []string) (added, removed []string) {
	oldSet := make(map[string]bool, len(oldRepos))
	for _, r := range oldRepos {
		oldSet[r] = true
	}
	newSet := make(map[string]bool, len(newRepos))
	for _, r := range newRepos {
		newSet[r] = true
	}
	for _, r := range newRepos {
		if !oldSet[r] {
			added = append(added, r)
		}
	}
	for _, r := range oldRepos {
		if !newSet[r] {
			removed = append(removed, r)
		}
	}
	return added, removed
}

// applyConfigReload computes merged — old with every changed field tagged
// "live" overwritten by cand's value — and a ReloadDiff describing what
// changed. A field tagged "restart" (or carrying no recognized tag at all)
// that changed value is reported in RestartOnlyChanged but merged keeps
// old's value for it: AC4 ("reported clearly... never silently ignored")
// is true of the log, and "not applied" (AC4) is literally true of merged,
// not just of runtime behavior. cand is assumed already fully parsed and
// validated by LoadConfig — this function performs no validation of its
// own, only classification and merging.
func applyConfigReload(old, cand Config) (Config, ReloadDiff) {
	merged := old
	var diff ReloadDiff

	diff.ReposAdded, diff.ReposRemoved = diffRepos(old.WatchedRepos, cand.WatchedRepos)
	if len(diff.ReposAdded) > 0 || len(diff.ReposRemoved) > 0 {
		merged.WatchedRepos = cand.WatchedRepos
	}

	t := reflect.TypeOf(Config{})
	oldV := reflect.ValueOf(old)
	candV := reflect.ValueOf(cand)
	mergedV := reflect.ValueOf(&merged).Elem()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Name == "WatchedRepos" {
			continue // handled specially above via diffRepos
		}

		oldF := oldV.Field(i)
		candF := candV.Field(i)
		if reflect.DeepEqual(oldF.Interface(), candF.Interface()) {
			continue
		}

		change := FieldChange{
			Field: field.Name,
			Old:   fmt.Sprint(oldF.Interface()),
			New:   fmt.Sprint(candF.Interface()),
		}

		switch reloadClass(field.Tag.Get("reload")) {
		case reloadLive:
			mergedV.Field(i).Set(candF)
			diff.FieldsChanged = append(diff.FieldsChanged, change)
		case reloadSkip:
			// VersionRequested only: never a real runtime value to reload
			// (LoadConfig would already have exited via --version before
			// handleReload's candidate could reach here) — never reported,
			// never applied.
		default:
			// "restart", an unrecognized tag value, or a missing tag are
			// all treated identically and conservatively: report, never
			// apply. See reloadClass's doc comment.
			diff.RestartOnlyChanged = append(diff.RestartOnlyChanged, change)
		}
	}

	return merged, diff
}

// logReloadSummary emits R6's diff-style summary of one reload attempt to
// .pruefer/pruefer.log (via logf), naming every repo added/removed and
// every other field that changed — including restart-only fields that were
// reported but not applied.
func logReloadSummary(diff ReloadDiff) {
	if diff.Empty() {
		logf(0, "reload", "config reloaded: no changes\n")
		return
	}
	if len(diff.ReposAdded) > 0 {
		logf(0, "reload", "watched_repos added: %s\n", strings.Join(diff.ReposAdded, ", "))
	}
	if len(diff.ReposRemoved) > 0 {
		logf(0, "reload", "watched_repos removed: %s\n", strings.Join(diff.ReposRemoved, ", "))
	}
	for _, c := range diff.FieldsChanged {
		logf(0, "reload", "%s changed: %s -> %s\n", c.Field, c.Old, c.New)
	}
	for _, c := range diff.RestartOnlyChanged {
		logf(0, "reload", "%s changed (%s -> %s) but requires a restart to take effect — not applied\n", c.Field, c.Old, c.New)
	}
}
