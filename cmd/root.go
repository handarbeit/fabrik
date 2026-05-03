package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/handarbeit/fabrik/config"
	"github.com/handarbeit/fabrik/engine"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// testReadyCh is set by tests to receive a signal once engine.Run has
// registered its signal handlers. This prevents SIGINT from arriving before
// signal.Notify is installed, which would terminate the process.
var testReadyCh chan struct{}

type Config struct {
	Owner             string
	Repo              string
	ProjectNum        int
	OwnerType         string
	User              string
	Token             string
	StagesDir         string
	Yolo              bool
	AutoUpgrade       bool
	GitSSH            bool
	TUI               bool
	PollSeconds       int
	MaxConcurrent     int
	MaxRetries        int
	ReviewWaitTimeout int // minutes; 0 means use default (15)
	MaxReviewCycles   int // 0 means use default (5)
	CIWaitTimeout     int // minutes; 0 means use default (30)
	MaxCiFixCycles    int // 0 means use default (5)
	MaxRebaseCycles   int // 0 means use default (3)
	ClaudeWaitDelay   int // seconds; 0 means use default (30)
	DebugOutput       bool
	PluginDir         string
	Webhooks          bool
	WebhookPort       int
	WebhookEvents     string // comma-separated; empty means default event set
	BoardCacheMode       string // "in-memory" or "none"; empty = auto (in-memory when webhooks enabled)
	StatusPollSeconds    int    // Layer 2 status-only sweep cadence in seconds; 0 = use default (600)
}

func Execute() error {
	// Install a custom usage function before any subcommand dispatch so it fires
	// for the main daemon path (subcommands return early and never reach flag.Parse).
	// We set both flag.Usage (package-level) and flag.CommandLine.Usage (FlagSet
	// field) so the custom function is invoked regardless of whether CommandLine
	// was replaced by a test harness (resetFlags replaces CommandLine but doesn't
	// install the commandLineUsage hook that normally delegates to flag.Usage).
	customUsage := func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "Fabrik — GitHub Project board orchestration for Claude Code\n\n")
		fmt.Fprintf(out, "Usage: fabrik [flags] | fabrik <subcommand> [args]\n\n")
		fmt.Fprintf(out, "Subcommands:\n")
		fmt.Fprintf(out, "  init [<project-url>]      Initialize .fabrik/ in the current directory\n")
		fmt.Fprintf(out, "  watch <issue-number>      Watch a single issue's Claude session live\n")
		fmt.Fprintf(out, "  resume <issue-number>     Resume an interactive Claude session for an issue\n")
		fmt.Fprintf(out, "  upgrade                   Upgrade the Fabrik binary and plugin skills\n")
		fmt.Fprintf(out, "  refresh-stages            Show (or apply) missing stage YAML keys from embedded defaults\n")
		fmt.Fprintf(out, "  stream-filter             Filter and pretty-print Claude streaming JSON (stdin → stdout)\n\n")
		fmt.Fprintf(out, "Flags:\n")
		flag.CommandLine.PrintDefaults()
	}
	flag.Usage = customUsage
	flag.CommandLine.Usage = customUsage

	// Dispatch subcommands before flag parsing so they get their own flag sets.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		return runInit(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		return runUpgrade(os.Args[2:])
	}
	if len(os.Args) > 1 && (os.Args[1] == "stream-filter" || os.Args[1] == "_stream-filter") {
		RunStreamFilter()
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		return runWatch(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "resume" {
		return runResume(os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "_debug-server" {
		RunDebugServer()
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "refresh-stages" {
		return runRefreshStages(os.Args[2:])
	}
	cfg := &Config{}

	var versionFlag bool
	flag.BoolVar(&versionFlag, "version", false, "Print the fabrik version and exit")

	flag.StringVar(&cfg.Owner, "owner", "", "GitHub repository owner")
	flag.StringVar(&cfg.Repo, "repo", "", "GitHub repository name")
	flag.IntVar(&cfg.ProjectNum, "project", 0, "GitHub project number")
	flag.StringVar(&cfg.User, "user", "", "GitHub username (only process changes by this user)")
	flag.StringVar(&cfg.Token, "token", "", "GitHub token (or set GITHUB_TOKEN env var)")
	flag.StringVar(&cfg.StagesDir, "stages", "./.fabrik/stages", "Directory containing stage YAML configs")
	flag.BoolVar(&cfg.Yolo, "yolo", false, "Auto-advance issues through stages without waiting for human input; also auto-merges the linked PR when Validate completes")
	flag.BoolVar(&cfg.GitSSH, "ssh", false, "Use SSH clone URLs (git@github.com) instead of HTTPS")
	flag.BoolVar(&cfg.AutoUpgrade, "auto-upgrade", false, "When idle, check GitHub Releases for a newer version and self-upgrade; dev builds (built from source) rebuild from origin/main instead")
	var noTUI bool
	flag.BoolVar(&noTUI, "notui", false, "Disable the interactive TUI dashboard (default: enabled when a real terminal is detected)")
	flag.IntVar(&cfg.PollSeconds, "poll", 30, "Polling interval in seconds")
	flag.IntVar(&cfg.MaxConcurrent, "max-concurrent", 5, "Maximum number of concurrent issue workers")
	flag.IntVar(&cfg.MaxRetries, "max-retries", 3, "Max failed stage attempts before pausing the issue (0 = unlimited)")
	flag.IntVar(&cfg.ReviewWaitTimeout, "review-wait-timeout", 0, "Maximum time in minutes to wait for PR reviewers before advancing (0 = use default of 15; also FABRIK_REVIEW_WAIT_TIMEOUT)")
	flag.IntVar(&cfg.MaxReviewCycles, "max-review-cycles", 0, "Maximum number of review-and-fix cycles per issue (0 = use default of 5; also FABRIK_MAX_REVIEW_CYCLES)")
	flag.IntVar(&cfg.CIWaitTimeout, "ci-wait-timeout", 0, "Maximum time in minutes to wait for CI in the merge guard before pausing (0 = use default of 30; also FABRIK_CI_WAIT_TIMEOUT)")
	flag.IntVar(&cfg.MaxCiFixCycles, "max-ci-fix-cycles", 0, "Maximum number of CI-fix cycles per issue before pausing (0 = use default of 5; also FABRIK_MAX_CI_FIX_CYCLES)")
	flag.IntVar(&cfg.MaxRebaseCycles, "max-rebase-cycles", 0, "Maximum number of rebase-reinvoke cycles per issue before pausing (0 = use default of 3; also FABRIK_MAX_REBASE_CYCLES)")
	flag.IntVar(&cfg.ClaudeWaitDelay, "claude-wait-delay", 0, "Seconds to wait after Claude exits before recovering buffered output when grandchildren hold stdout pipe open (0 = use default of 30; also FABRIK_CLAUDE_WAIT_DELAY)")
	flag.BoolVar(&cfg.DebugOutput, "debug-output", false, "Save Claude stage output to .fabrik/debug/ for debugging")
	flag.StringVar(&cfg.PluginDir, "plugin-dir", "", "Path to Fabrik plugin directory (for development; overrides installed plugin)")
	flag.BoolVar(&cfg.Webhooks, "webhooks", false, "Enable webhook-driven event delivery via gh webhook forward (requires gh ≥ 2.32.0; also FABRIK_WEBHOOKS)")
	flag.IntVar(&cfg.WebhookPort, "webhook-port", 0, "Local port for the webhook HTTP listener (0 = OS-assigned; also FABRIK_WEBHOOK_PORT)")
	flag.StringVar(&cfg.WebhookEvents, "webhook-events", "", "Comma-separated list of GitHub event types to subscribe to (default: all supported events; also FABRIK_WEBHOOK_EVENTS)")
	flag.StringVar(&cfg.BoardCacheMode, "board-cache", "", `Board cache mode: "in-memory" (cache board state; requires --webhooks) or "none" (always fetch from GitHub). Default: "in-memory" when --webhooks is enabled, "none" otherwise. Also FABRIK_BOARD_CACHE.`)
	flag.IntVar(&cfg.StatusPollSeconds, "status-poll", 0, "Cadence in seconds for the periodic status-only board sweep (Layer 2; 0 = use default of 600; also FABRIK_STATUS_POLL)")

	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		return err
	}

	// Track which flags were explicitly provided on the command line so that env
	// var fallbacks are only applied when the flag was omitted entirely.  Without
	// this, an explicit --review-wait-timeout=0 (or --max-review-cycles=0) would
	// be indistinguishable from "flag not set" and the env var would override it.
	explicitFlags := make(map[string]bool)
	flag.CommandLine.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })

	if versionFlag {
		fmt.Println(Version)
		return nil
	}

	// Load .env file if present (fatal if .env exists but not in .gitignore)
	if err := config.LoadDotenv(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Load .fabrik/config.yaml (optional; zero value if absent)
	pc, err := config.LoadProjectConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Warn (non-fatal) if config.yaml is gitignored
	config.WarnIfConfigIgnored()

	// Token: flag > FABRIK_TOKEN > GITHUB_TOKEN
	if cfg.Token == "" {
		cfg.Token = config.Token()
	}

	// Allow env vars (from .env or shell) to fill in missing flags,
	// falling back to config.yaml values when still at default.
	if cfg.Owner == "" {
		if v := os.Getenv("FABRIK_OWNER"); v != "" {
			cfg.Owner = v
		} else if pc.Owner != "" {
			cfg.Owner = pc.Owner
		}
	}
	if cfg.Repo == "" {
		if v := os.Getenv("FABRIK_REPO"); v != "" {
			cfg.Repo = v
		} else if pc.Repo != "" {
			cfg.Repo = pc.Repo
		}
	}
	if cfg.ProjectNum == 0 {
		if v := os.Getenv("FABRIK_PROJECT_NUMBER"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("FABRIK_PROJECT_NUMBER=%q is invalid (must be a positive integer)", v)
			}
			cfg.ProjectNum = n
		} else if pc.ProjectNum != nil {
			if *pc.ProjectNum <= 0 {
				return fmt.Errorf("config.yaml project=%d is invalid (must be a positive integer)", *pc.ProjectNum)
			}
			cfg.ProjectNum = *pc.ProjectNum
		}
	}
	if cfg.User == "" {
		if v := os.Getenv("FABRIK_USER"); v != "" {
			cfg.User = v
		} else if pc.User != "" {
			cfg.User = pc.User
		}
	}
	// OwnerType is derived at init time from the project URL; no env var or flag.
	if cfg.OwnerType == "" && pc.OwnerType != "" {
		cfg.OwnerType = pc.OwnerType
	}
	if cfg.StagesDir == "./.fabrik/stages" {
		if v := os.Getenv("FABRIK_STAGES"); v != "" {
			cfg.StagesDir = v
		} else if pc.StagesDir != "" {
			cfg.StagesDir = pc.StagesDir
		}
	}
	if !cfg.Yolo {
		if v := os.Getenv("FABRIK_YOLO"); v != "" {
			lv := strings.ToLower(v)
			cfg.Yolo = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.Yolo {
			cfg.Yolo = true
		}
	}
	if !cfg.GitSSH {
		if v := os.Getenv("FABRIK_GIT_SSH"); v != "" {
			lv := strings.ToLower(v)
			cfg.GitSSH = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.GitSSH {
			cfg.GitSSH = true
		}
	}
	if cfg.PollSeconds == 30 {
		if v := os.Getenv("FABRIK_POLL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.PollSeconds = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_POLL=%q is invalid (must be a positive integer); using default %d\n", v, cfg.PollSeconds)
			}
		} else if pc.Poll != nil {
			if *pc.Poll <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml poll=%d is invalid (must be a positive integer); using default %d\n", *pc.Poll, cfg.PollSeconds)
			} else {
				cfg.PollSeconds = *pc.Poll
			}
		}
	}
	if cfg.MaxConcurrent == 5 {
		if v := os.Getenv("FABRIK_MAX_CONCURRENT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxConcurrent = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_CONCURRENT=%q is invalid (must be a positive integer); using default %d\n", v, cfg.MaxConcurrent)
			}
		} else if pc.MaxConcurrent != nil {
			if *pc.MaxConcurrent <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml max_concurrent=%d is invalid (must be a positive integer); using default %d\n", *pc.MaxConcurrent, cfg.MaxConcurrent)
			} else {
				cfg.MaxConcurrent = *pc.MaxConcurrent
			}
		}
	}
	if cfg.MaxRetries == 3 {
		if v := os.Getenv("FABRIK_MAX_RETRIES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				cfg.MaxRetries = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_RETRIES=%q is invalid (must be a non-negative integer); using default %d\n", v, cfg.MaxRetries)
			}
		} else if pc.MaxRetries != nil {
			if *pc.MaxRetries < 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml max_retries=%d is invalid (must be a non-negative integer); using default %d\n", *pc.MaxRetries, cfg.MaxRetries)
			} else {
				cfg.MaxRetries = *pc.MaxRetries
			}
		}
	}
	if !explicitFlags["review-wait-timeout"] {
		if v := os.Getenv("FABRIK_REVIEW_WAIT_TIMEOUT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.ReviewWaitTimeout = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_REVIEW_WAIT_TIMEOUT=%q is invalid (must be a positive integer of minutes); using default 15\n", v)
			}
		}
	}
	if !explicitFlags["max-review-cycles"] {
		if v := os.Getenv("FABRIK_MAX_REVIEW_CYCLES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxReviewCycles = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_REVIEW_CYCLES=%q is invalid (must be a positive integer); using default 5\n", v)
			}
		}
	}
	if !explicitFlags["ci-wait-timeout"] {
		if v := os.Getenv("FABRIK_CI_WAIT_TIMEOUT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.CIWaitTimeout = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_CI_WAIT_TIMEOUT=%q is invalid (must be a positive integer of minutes); using default 30\n", v)
			}
		}
	}
	if !explicitFlags["max-ci-fix-cycles"] {
		if v := os.Getenv("FABRIK_MAX_CI_FIX_CYCLES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxCiFixCycles = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_CI_FIX_CYCLES=%q is invalid (must be a positive integer); using default 5\n", v)
			}
		}
	}
	if !explicitFlags["max-rebase-cycles"] {
		if v := os.Getenv("FABRIK_MAX_REBASE_CYCLES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxRebaseCycles = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_REBASE_CYCLES=%q is invalid (must be a positive integer); using default 3\n", v)
			}
		}
	}
	if !explicitFlags["claude-wait-delay"] {
		if v := os.Getenv("FABRIK_CLAUDE_WAIT_DELAY"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				if n > 0 {
					cfg.ClaudeWaitDelay = n
				} else if n < 0 {
					fmt.Fprintf(os.Stderr, "[warn] FABRIK_CLAUDE_WAIT_DELAY=%q is invalid (must be 0 or a positive integer of seconds); using default 30\n", v)
				}
				// n == 0: silently use default (same semantics as --claude-wait-delay 0)
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_CLAUDE_WAIT_DELAY=%q is invalid (must be 0 or a positive integer of seconds); using default 30\n", v)
			}
		}
	}
	if !cfg.AutoUpgrade {
		if v := os.Getenv("FABRIK_AUTO_UPGRADE"); v != "" {
			lv := strings.ToLower(v)
			cfg.AutoUpgrade = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.AutoUpgrade {
			cfg.AutoUpgrade = true
		}
	}
	cfg.TUI = true // default on
	if noTUI {
		cfg.TUI = false
	} else if v := os.Getenv("FABRIK_TUI"); v != "" {
		lv := strings.ToLower(v)
		if lv == "false" || lv == "0" || lv == "no" {
			cfg.TUI = false
		}
	} else if pc.TUI != nil && !*pc.TUI {
		cfg.TUI = false
	}
	if !cfg.DebugOutput {
		if v := os.Getenv("FABRIK_DEBUG_OUTPUT"); v != "" {
			lv := strings.ToLower(v)
			cfg.DebugOutput = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.DebugOutput {
			cfg.DebugOutput = true
		}
	}
	if cfg.PluginDir == "" {
		if v := os.Getenv("FABRIK_PLUGIN_DIR"); v != "" {
			cfg.PluginDir = v
		}
	}
	if !explicitFlags["webhooks"] {
		if v := os.Getenv("FABRIK_WEBHOOKS"); v != "" {
			lv := strings.ToLower(v)
			cfg.Webhooks = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.Webhooks {
			cfg.Webhooks = true
		}
	}
	if !explicitFlags["webhook-port"] {
		if v := os.Getenv("FABRIK_WEBHOOK_PORT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				cfg.WebhookPort = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_WEBHOOK_PORT=%q is invalid (must be 0 or a positive integer); using OS-assigned port\n", v)
			}
		} else if pc.WebhookPort != nil {
			if *pc.WebhookPort >= 0 {
				cfg.WebhookPort = *pc.WebhookPort
			} else {
				fmt.Fprintf(os.Stderr, "[warn] webhook_port=%d is invalid (must be 0 or a positive integer); using OS-assigned port\n", *pc.WebhookPort)
			}
		}
	}
	if cfg.WebhookEvents == "" {
		if v := os.Getenv("FABRIK_WEBHOOK_EVENTS"); v != "" {
			cfg.WebhookEvents = v
		}
	}
	if !explicitFlags["board-cache"] {
		if v := os.Getenv("FABRIK_BOARD_CACHE"); v != "" {
			cfg.BoardCacheMode = v
		}
	}
	if !explicitFlags["status-poll"] {
		if v := os.Getenv("FABRIK_STATUS_POLL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.StatusPollSeconds = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_STATUS_POLL=%q is invalid (must be a positive integer); using default 600\n", v)
			}
		} else if pc.StatusPoll != nil {
			if *pc.StatusPoll <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml status_poll=%d is invalid (must be a positive integer); using default 600\n", *pc.StatusPoll)
			} else {
				cfg.StatusPollSeconds = *pc.StatusPoll
			}
		}
	}

	// Apply board-cache default: "in-memory" when webhooks are enabled; "none" otherwise.
	// Explicit --board-cache=in-memory without --webhooks is a configuration error.
	if cfg.BoardCacheMode == "" {
		if cfg.Webhooks {
			cfg.BoardCacheMode = "in-memory"
		} else {
			cfg.BoardCacheMode = "none"
		}
	}
	if cfg.BoardCacheMode == "in-memory" && !cfg.Webhooks {
		return fmt.Errorf("--board-cache=in-memory requires --webhooks (no delta source without webhook events)")
	}
	if cfg.BoardCacheMode != "in-memory" && cfg.BoardCacheMode != "none" {
		return fmt.Errorf("invalid --board-cache value %q: must be \"in-memory\" or \"none\"", cfg.BoardCacheMode)
	}

	if cfg.Owner == "" || cfg.ProjectNum == 0 {
		flag.Usage()
		return fmt.Errorf("missing required config: owner and project (use flags or .env file)")
	}

	if cfg.Token == "" {
		return fmt.Errorf("GitHub token required: use --token, FABRIK_TOKEN, or GITHUB_TOKEN")
	}

	if strings.HasPrefix(cfg.Token, "github_pat_") {
		fmt.Fprintf(os.Stderr, "[warn] Fine-grained personal access tokens (github_pat_...) do not support GitHub Projects v2 GraphQL. Switch to a classic personal access token with 'repo', 'project', and 'workflow' scopes. See: https://github.com/settings/tokens\n")
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

	// Stage drift warnings are emitted from engine.Run() once the persistent
	// log file is open, so they land in fabrik.log as well as stderr.

	// TUI is enabled by default when a real terminal is detected; use --notui to disable.
	useTUI := cfg.TUI &&
		(isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())) &&
		(isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()))

	// In plain-text mode, print the startup banner to stdout. In TUI mode the
	// bubbletea alt-screen replaces stdout immediately, so skip the banner.
	if !useTUI {
		fmt.Printf("Fabrik starting %s\n", Version)
		fmt.Printf("  repo:    %s/%s\n", cfg.Owner, cfg.Repo)
		fmt.Printf("  project: #%d\n", cfg.ProjectNum)
		fmt.Printf("  user:    %s\n", cfg.User)
		fmt.Printf("  stages:  %d loaded\n", len(stageCfgs))
		fmt.Printf("  yolo:    %v\n", cfg.Yolo)
		fmt.Printf("  auto-upgrade: %v\n", cfg.AutoUpgrade)
		fmt.Printf("  poll:    %ds\n", cfg.PollSeconds)
		fmt.Printf("  workers: %d\n", cfg.MaxConcurrent)
		if cfg.MaxRetries == 0 {
			fmt.Printf("  max-retries: unlimited\n")
		} else {
			fmt.Printf("  max-retries: %d\n", cfg.MaxRetries)
		}
	}
	fmt.Printf("  debug-output: %v\n", cfg.DebugOutput)

	// If the process was re-exec'd after an auto-upgrade, refresh embedded plugin
	// skills before entering the poll loop. The env var is unset immediately so
	// the re-exec'd process doesn't loop.
	if os.Getenv("FABRIK_AUTO_UPGRADED") == "1" {
		os.Unsetenv("FABRIK_AUTO_UPGRADED")
		if _, err := fabrikplugin.RefreshPlugin(); err != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: RefreshPlugin failed: %v\n", err)
		}
	}

	// Check whether on-disk plugin files match the embedded versions. This runs
	// only in the main daemon path (all subcommands returned early above).
	// If .fabrik/plugin/ does not exist (no fabrik init), the check is a no-op.
	if err := checkPluginSkills(".fabrik/plugin"); err != nil {
		fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skill check failed: %v\n", err)
	}

	// Parse webhook events from comma-separated string.
	var webhookEvents []string
	if cfg.WebhookEvents != "" {
		for _, ev := range strings.Split(cfg.WebhookEvents, ",") {
			ev = strings.TrimSpace(ev)
			if ev != "" {
				webhookEvents = append(webhookEvents, ev)
			}
		}
	} else if len(pc.WebhookEvents) > 0 {
		webhookEvents = pc.WebhookEvents
	}

	eng, err := engine.New(engine.Config{
		Owner:             cfg.Owner,
		Repo:              cfg.Repo,
		ProjectNum:        cfg.ProjectNum,
		OwnerType:         cfg.OwnerType,
		User:              cfg.User,
		Token:             cfg.Token,
		Version:           Version,
		Yolo:              cfg.Yolo,
		AutoUpgrade:       cfg.AutoUpgrade,
		GitSSH:            cfg.GitSSH,
		PollSeconds:       cfg.PollSeconds,
		MaxConcurrent:     cfg.MaxConcurrent,
		MaxRetries:        cfg.MaxRetries,
		ReviewWaitTimeout: reviewWaitTimeout(cfg.ReviewWaitTimeout),
		MaxReviewCycles:   maxReviewCycles(cfg.MaxReviewCycles),
		CIWaitTimeout:     ciWaitTimeout(cfg.CIWaitTimeout),
		MaxCiFixCycles:    maxCiFixCycles(cfg.MaxCiFixCycles),
		MaxRebaseCycles:   maxRebaseCycles(cfg.MaxRebaseCycles),
		ClaudeWaitDelay:   claudeWaitDelay(cfg.ClaudeWaitDelay),
		DebugOutput:       cfg.DebugOutput,
		PluginDir:         cfg.PluginDir,
		Stages:            stageCfgs,
		Webhooks:          cfg.Webhooks,
		WebhookPort:       cfg.WebhookPort,
		WebhookEvents:     webhookEvents,
		BoardCacheMode:           cfg.BoardCacheMode,
		ProjectStatusPollSeconds: statusPollSeconds(cfg.StatusPollSeconds),
		ReadyCh:                  testReadyCh,
	})
	if err != nil {
		return err
	}

	// When TUI is enabled (and we have a real terminal), run the bubbletea
	// TUI. Otherwise fall through to plain-text mode.
	if useTUI {
		wakeCh := make(chan struct{}, 1)
		eng.SetWakeCh(wakeCh)
		return runTUI(eng, cfg.PollSeconds, buildProjectInfo(cfg, pc), cfg.PluginDir, wakeCh)
	}
	return eng.Run()
}

// reviewWaitTimeout converts a ReviewWaitTimeout config value (minutes) to a
// time.Duration. When minutes is 0 (unset), the default of 15 minutes is used.
func reviewWaitTimeout(minutes int) time.Duration {
	if minutes <= 0 {
		return 15 * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

// maxReviewCycles returns the configured MaxReviewCycles value, defaulting to 5
// when n is 0 (unset).
func maxReviewCycles(n int) int {
	if n <= 0 {
		return 5
	}
	return n
}

// ciWaitTimeout converts a CIWaitTimeout config value (minutes) to a
// time.Duration. When minutes is 0 (unset), the default of 30 minutes is used.
func ciWaitTimeout(minutes int) time.Duration {
	if minutes <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

// maxCiFixCycles returns the configured MaxCiFixCycles value, defaulting to 5
// when n is 0 (unset).
func maxCiFixCycles(n int) int {
	if n <= 0 {
		return 5
	}
	return n
}

// maxRebaseCycles returns the configured MaxRebaseCycles value, defaulting to 3
// when n is 0 (unset). The default is lower than CI/review because rebase
// either works in one shot or needs human intervention on a semantic conflict.
func maxRebaseCycles(n int) int {
	if n <= 0 {
		return 3
	}
	return n
}

// claudeWaitDelay converts a ClaudeWaitDelay config value (seconds) to a
// time.Duration. When seconds is 0 (unset), the default of 30 seconds is used.
func claudeWaitDelay(seconds int) time.Duration {
	if seconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// statusPollSeconds returns the configured ProjectStatusPollSeconds, defaulting
// to 600 (10 min) when n is 0 (unset).
func statusPollSeconds(n int) int {
	if n <= 0 {
		return 600
	}
	return n
}

// buildProjectInfo assembles the TUI footer metadata from the active config.
func buildProjectInfo(cfg *Config, pc config.ProjectConfig) tui.ProjectInfo {
	// Format CWD as home-relative path when possible.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	cwdDisplay := cwd
	if home, herr := os.UserHomeDir(); herr == nil && home != "" && (cwd == home || strings.HasPrefix(cwd, home+string(os.PathSeparator))) {
		cwdDisplay = "~" + cwd[len(home):]
	}

	// Resolve version: explicit config field wins over CWD inference.
	version := pc.Version
	if version == "" {
		version = config.InferVersion(cwd)
	}

	repo := ""
	if cfg.Owner != "" && cfg.Repo != "" {
		repo = cfg.Owner + "/" + cfg.Repo
	}
	return tui.ProjectInfo{
		CWD:           cwdDisplay,
		Repo:          repo,
		Version:       version,
		FabrikVersion: Version,
	}
}

// runTUI wires the event channel, starts the bubbletea program, and runs the
// engine. The engine handles SIGINT itself; bubbletea uses WithoutSignalHandler
// so it doesn't interfere. When the engine exits, the TUI is quit.
func runTUI(eng *engine.Engine, pollSeconds int, info tui.ProjectInfo, pluginDir string, wakeCh chan struct{}) error {
	events := make(chan tui.Event, 256)
	eng.SetEvents(events)

	tuiModel := tui.New(pollSeconds, info, pluginDir, wakeCh)
	p := tea.NewProgram(tuiModel, tea.WithAltScreen(), tea.WithoutSignalHandler())

	// Forward events from the engine's channel into bubbletea.
	go func() {
		for ev := range events {
			p.Send(ev)
		}
	}()

	// Run the engine in a goroutine; quit the TUI when it returns.
	errCh := make(chan error, 1)
	go func() {
		err := eng.Run()
		// Close the events channel so the forwarding goroutine exits.
		close(events)
		errCh <- err
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		p.ReleaseTerminal()
		// TUI failed — signal the engine to stop so its goroutine and the
		// forwarding goroutine both exit cleanly.
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-errCh
		return err
	}
	p.ReleaseTerminal()
	// TUI exited (user pressed q or ctrl+c). If the engine is still running
	// (q doesn't send SIGINT), signal it to stop gracefully.
	select {
	case engineErr := <-errCh:
		return engineErr
	default:
		// Engine still running; send SIGTERM so its signal handler fires.
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		return <-errCh
	}
}
