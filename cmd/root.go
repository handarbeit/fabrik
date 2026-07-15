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

// testResolvedConfigHook is set by tests to observe the fully-resolved Config
// immediately before engine.New is called. When set, Execute returns nil
// right after invoking it, short-circuiting before the engine is constructed.
var testResolvedConfigHook func(Config)

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
	WorkerStaleMins   int // minutes; 0 means use default (5)
	MaxCiFixCycles    int // 0 means use default (5)
	MaxRebaseCycles      int    // 0 means use default (3)
	MaxEnqueueCycles     int    // 0 means use default (5)
	ConvergenceBudget    string // Go duration string; "" means use default (30m); "0" means disabled
	AutoMergeStrategy    string // MERGE, SQUASH, or REBASE; "" means use default (MERGE)
	MergeQueue           string // auto or off; "" means use default (auto)
	MergeTrain           string // on or off; "" means use default (off)
	MaxBatchSize             int    // 0 means use default (5)
	MaxBisectValidations     int    // 0 means derive default (2·⌈log₂(MaxBatchSize)⌉+1)
	MaxTrainRebaseCycles     int    // 0 means use default (3)
	MaxTrainTrialsPerWindow  int    // 0 means use default (20)
	TrainTrialWindowMinutes  int    // 0 means use default (60)
	ClaudeWaitDelay          int    // seconds; 0 means use default (30)
	PostPushDwell        int    // seconds; 0 means use default (90)
	KillGraceSigInt      string // Go duration string; "" means use default (10s); "0s" skips SIGINT step
	KillGraceSigTerm     string // Go duration string; "" means use default (10s)
	DebugOutput              bool
	SymlinkEnv               bool
	WorktreeBoundaryAudit    bool
	PluginDir                string
	Webhooks          bool
	WebhookPort       int
	WebhookEvents     string // comma-separated; empty means default event set
	StatusPollSeconds    int // Layer 2 status-only sweep cadence in seconds; 0 = use default (15)
	ReconcileInterval    int // seconds; 0 means use default (180 = 3 min); also FABRIK_RECONCILE_INTERVAL
	JanitorIntervalHours int   // hours; 1 = default; 0 disables the janitor
	LogRetentionDays     int   // days; 14 = default; 0 disables age-based log pruning
	LogMaxBytes          int64 // bytes; 2147483648 = default; 0 disables size-cap pruning
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
	flag.IntVar(&cfg.WorkerStaleMins, "worker-stale-timeout", 0, "Minutes before a stale worker heartbeat triggers PID-liveness check (0 = use default of 5; also FABRIK_WORKER_STALE_TIMEOUT)")
	flag.IntVar(&cfg.MaxCiFixCycles, "max-ci-fix-cycles", 0, "Maximum number of CI-fix cycles per issue before pausing (0 = use default of 5; also FABRIK_MAX_CI_FIX_CYCLES)")
	flag.IntVar(&cfg.MaxRebaseCycles, "max-rebase-cycles", 0, "Maximum number of rebase-reinvoke cycles per issue before pausing (0 = use default of 3; also FABRIK_MAX_REBASE_CYCLES)")
	flag.IntVar(&cfg.MaxEnqueueCycles, "max-enqueue-cycles", 0, "Maximum number of merge-queue re-enqueue cycles per issue before pausing (0 = use default of 5; also FABRIK_MAX_ENQUEUE_CYCLES)")
	flag.StringVar(&cfg.ConvergenceBudget, "convergence-budget", "", "Wall-clock budget for post-Validate yolo convergence (Go duration: 30m, 1h; \"0\" disables; also FABRIK_CONVERGENCE_BUDGET)")
	flag.StringVar(&cfg.AutoMergeStrategy, "auto-merge-strategy", "", "Merge method for GitHub auto-merge: MERGE, SQUASH, or REBASE (also FABRIK_AUTO_MERGE_STRATEGY; default MERGE)")
	flag.StringVar(&cfg.MergeQueue, "merge-queue", "", "Merge queue routing for yolo path: auto (enqueue when repo uses merge queue) or off (skip enqueue; direct merge may fail on queue-required repos; also FABRIK_MERGE_QUEUE; default auto)")
	flag.StringVar(&cfg.MergeTrain, "merge-train", "", "Fabrik-internal merge train: on (advance yolo Validate completions to Queued column for batched landing) or off (also FABRIK_MERGE_TRAIN; default off)")
	flag.IntVar(&cfg.MaxBatchSize, "max-batch-size", 0, "Maximum Queued items landed in a single merge-train batch, ordered by entry (0 = use default of 5; smaller = cheaper worst-case bisection, fewer N² savings; also FABRIK_MAX_BATCH_SIZE)")
	flag.IntVar(&cfg.MaxBisectValidations, "max-bisect-validations", 0, "Maximum combined validations per red merge-train batch before degrading to one-at-a-time landing (0 = derive 2·⌈log₂(max-batch-size)⌉+1, ≈7 at the default batch size; also FABRIK_MAX_BISECT_VALIDATIONS)")
	flag.IntVar(&cfg.MaxTrainRebaseCycles, "max-train-rebase-cycles", 0, "Maximum main-moved rebase+revalidate cycles for a merge-train batch before dissolving it back to Queued (0 = use default of 3; also FABRIK_MAX_TRAIN_REBASE_CYCLES)")
	flag.IntVar(&cfg.MaxTrainTrialsPerWindow, "max-train-trials-per-window", 0, "Runaway guard: maximum trial-branch creations with zero successful lands within the window before pausing all Queued members (0 = use default of 20; also FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW)")
	flag.IntVar(&cfg.TrainTrialWindowMinutes, "train-trial-window", 0, "Runaway guard: rolling window in minutes over which max-train-trials-per-window is measured (0 = use default of 60; also FABRIK_TRAIN_TRIAL_WINDOW)")
	flag.IntVar(&cfg.ClaudeWaitDelay, "claude-wait-delay", 0, "Seconds to wait after Claude exits before recovering buffered output when grandchildren hold stdout pipe open (0 = use default of 30; also FABRIK_CLAUDE_WAIT_DELAY)")
	flag.IntVar(&cfg.PostPushDwell, "post-push-dwell", 0, "Seconds to wait after a PR force-push before clearing the CI gate as 'no CI configured' (0 = use default of 90; also FABRIK_POST_PUSH_DWELL)")
	flag.BoolVar(&cfg.DebugOutput, "debug-output", false, "Save Claude stage output to .fabrik/debug/ for debugging")
	flag.BoolVar(&cfg.SymlinkEnv, "symlink-env", false, "Symlink the fabrik-dir .env file into each issue worktree at creation time (also FABRIK_SYMLINK_ENV)")
	flag.BoolVar(&cfg.WorktreeBoundaryAudit, "worktree-boundary-audit", false, "Enable Layer 2 cross-repo ref-mutation audit (default off pending #808 root-cause fix; also FABRIK_WORKTREE_BOUNDARY_AUDIT)")
	flag.StringVar(&cfg.PluginDir, "plugin-dir", "", "Path to Fabrik plugin directory (for development; overrides installed plugin)")
	flag.BoolVar(&cfg.Webhooks, "webhooks", false, "Enable webhook-driven event delivery via gh webhook forward (requires gh ≥ 2.32.0; also FABRIK_WEBHOOKS)")
	flag.IntVar(&cfg.WebhookPort, "webhook-port", 0, "Local port for the webhook HTTP listener (0 = OS-assigned; also FABRIK_WEBHOOK_PORT)")
	flag.StringVar(&cfg.WebhookEvents, "webhook-events", "", "Comma-separated list of GitHub event types to subscribe to (default: all supported events; also FABRIK_WEBHOOK_EVENTS)")
	flag.IntVar(&cfg.StatusPollSeconds, "status-poll", 0, "Retained for config compatibility; the Layer 2 updatedAt gate now runs every poll cycle (~15 s) regardless of this value. Also FABRIK_STATUS_POLL.")
	flag.IntVar(&cfg.ReconcileInterval, "reconcile-interval", 0, "Seconds between periodic light-reconcile webhook-stream-health checks when --webhooks is enabled (0 = use default of 180; also FABRIK_RECONCILE_INTERVAL). Inactive without --webhooks — the per-poll Reconcile is the only freshener in that mode.")
	flag.IntVar(&cfg.JanitorIntervalHours, "janitor-interval", 1, "Worktree janitor scan interval in hours; 0 disables the janitor (also FABRIK_JANITOR_INTERVAL)")
	flag.IntVar(&cfg.LogRetentionDays, "log-retention-days", 14, "Delete log files older than this many days; 0 disables age-based pruning (also FABRIK_LOG_RETENTION_DAYS)")
	flag.Int64Var(&cfg.LogMaxBytes, "log-max-bytes", 2147483648, "Total size cap for .fabrik/logs/ in bytes; oldest files deleted first after age prune; 0 disables size cap (also FABRIK_LOG_MAX_BYTES)")
	flag.StringVar(&cfg.KillGraceSigInt, "kill-grace-sigint", "", "Grace window after SIGINT before SIGTERM in the kill escalation sequence (Go duration: 10s, 0s to skip SIGINT entirely; also FABRIK_KILL_GRACE_SIGINT; default 10s)")
	flag.StringVar(&cfg.KillGraceSigTerm, "kill-grace-sigterm", "", "Grace window after SIGTERM before SIGKILL in the kill escalation sequence (Go duration: 10s; also FABRIK_KILL_GRACE_SIGTERM; default 10s)")

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
	if !explicitFlags["worker-stale-timeout"] {
		if v := os.Getenv("FABRIK_WORKER_STALE_TIMEOUT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.WorkerStaleMins = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_WORKER_STALE_TIMEOUT=%q is invalid (must be a positive integer of minutes); using default 5\n", v)
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
	if !explicitFlags["max-enqueue-cycles"] {
		if v := os.Getenv("FABRIK_MAX_ENQUEUE_CYCLES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxEnqueueCycles = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_ENQUEUE_CYCLES=%q is invalid (must be a positive integer); using default 5\n", v)
			}
		}
	}
	if !explicitFlags["convergence-budget"] {
		if v := os.Getenv("FABRIK_CONVERGENCE_BUDGET"); v != "" {
			cfg.ConvergenceBudget = v // validated in convergenceBudget() helper
		}
	}
	if !explicitFlags["auto-merge-strategy"] {
		if v := os.Getenv("FABRIK_AUTO_MERGE_STRATEGY"); v != "" {
			cfg.AutoMergeStrategy = v // validated in autoMergeStrategy() helper
		}
	}
	if !explicitFlags["merge-queue"] {
		if v := os.Getenv("FABRIK_MERGE_QUEUE"); v != "" {
			cfg.MergeQueue = v // validated in mergeQueueMode() helper
		}
	}
	if !explicitFlags["merge-train"] {
		if v := os.Getenv("FABRIK_MERGE_TRAIN"); v != "" {
			cfg.MergeTrain = v // validated in mergeTrainMode() helper
		} else if pc.MergeTrain != "" {
			cfg.MergeTrain = pc.MergeTrain // validated in mergeTrainMode() helper
		}
	}
	if !explicitFlags["max-batch-size"] {
		if v := os.Getenv("FABRIK_MAX_BATCH_SIZE"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxBatchSize = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_BATCH_SIZE=%q is invalid (must be a positive integer); using default 5\n", v)
			}
		} else if pc.MaxBatchSize != nil {
			if *pc.MaxBatchSize <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml max_batch_size=%d is invalid (must be a positive integer); using default 5\n", *pc.MaxBatchSize)
			} else {
				cfg.MaxBatchSize = *pc.MaxBatchSize
			}
		}
	}
	if !explicitFlags["max-bisect-validations"] {
		if v := os.Getenv("FABRIK_MAX_BISECT_VALIDATIONS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxBisectValidations = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_BISECT_VALIDATIONS=%q is invalid (must be a positive integer); using derived default\n", v)
			}
		} else if pc.MaxBisectValidations != nil {
			if *pc.MaxBisectValidations < 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml max_bisect_validations=%d is invalid (must be a non-negative integer); using derived default\n", *pc.MaxBisectValidations)
			} else {
				cfg.MaxBisectValidations = *pc.MaxBisectValidations
			}
		}
	}
	if !explicitFlags["max-train-rebase-cycles"] {
		if v := os.Getenv("FABRIK_MAX_TRAIN_REBASE_CYCLES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxTrainRebaseCycles = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_TRAIN_REBASE_CYCLES=%q is invalid (must be a positive integer); using default 3\n", v)
			}
		} else if pc.MaxTrainRebaseCycles != nil {
			if *pc.MaxTrainRebaseCycles <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml max_train_rebase_cycles=%d is invalid (must be a positive integer); using default 3\n", *pc.MaxTrainRebaseCycles)
			} else {
				cfg.MaxTrainRebaseCycles = *pc.MaxTrainRebaseCycles
			}
		}
	}
	if !explicitFlags["max-train-trials-per-window"] {
		if v := os.Getenv("FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.MaxTrainTrialsPerWindow = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW=%q is invalid (must be a positive integer); using default 20\n", v)
			}
		} else if pc.MaxTrainTrialsPerWindow != nil {
			if *pc.MaxTrainTrialsPerWindow <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml max_train_trials_per_window=%d is invalid (must be a positive integer); using default 20\n", *pc.MaxTrainTrialsPerWindow)
			} else {
				cfg.MaxTrainTrialsPerWindow = *pc.MaxTrainTrialsPerWindow
			}
		}
	}
	if !explicitFlags["train-trial-window"] {
		if v := os.Getenv("FABRIK_TRAIN_TRIAL_WINDOW"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.TrainTrialWindowMinutes = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_TRAIN_TRIAL_WINDOW=%q is invalid (must be a positive integer of minutes); using default 60\n", v)
			}
		} else if pc.TrainTrialWindow != nil {
			if *pc.TrainTrialWindow <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml train_trial_window=%d is invalid (must be a positive integer of minutes); using default 60\n", *pc.TrainTrialWindow)
			} else {
				cfg.TrainTrialWindowMinutes = *pc.TrainTrialWindow
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
	if !explicitFlags["post-push-dwell"] {
		if v := os.Getenv("FABRIK_POST_PUSH_DWELL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				if n > 0 {
					cfg.PostPushDwell = n
				}
				// n == 0: silently use default (same semantics as --post-push-dwell 0)
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_POST_PUSH_DWELL=%q is invalid (must be 0 or a positive integer of seconds); using default 90\n", v)
			}
		}
	}
	if !explicitFlags["reconcile-interval"] {
		if v := os.Getenv("FABRIK_RECONCILE_INTERVAL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				cfg.ReconcileInterval = n // 0 → use engine default (lightReconcileInterval = 3 min)
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_RECONCILE_INTERVAL=%q is invalid (must be a non-negative integer of seconds; 0 = use default 180); using default 180\n", v)
			}
		}
	}
	if !explicitFlags["janitor-interval"] {
		if v := os.Getenv("FABRIK_JANITOR_INTERVAL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				cfg.JanitorIntervalHours = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_JANITOR_INTERVAL=%q is invalid (must be a non-negative integer of hours; 0 = disable); using default 1\n", v)
			}
		} else if pc.JanitorIntervalHours != nil {
			if *pc.JanitorIntervalHours < 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml janitor_interval_hours=%d is invalid (must be a non-negative integer; 0 = disable); using default 1\n", *pc.JanitorIntervalHours)
			} else {
				cfg.JanitorIntervalHours = *pc.JanitorIntervalHours
			}
		}
	}
	if !explicitFlags["log-retention-days"] {
		if v := os.Getenv("FABRIK_LOG_RETENTION_DAYS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				cfg.LogRetentionDays = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_LOG_RETENTION_DAYS=%q is invalid (must be a non-negative integer of days; 0 = disable); using default 14\n", v)
			}
		} else if pc.LogRetentionDays != nil {
			if *pc.LogRetentionDays < 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml log_retention_days=%d is invalid (must be a non-negative integer; 0 = disable); using default 14\n", *pc.LogRetentionDays)
			} else {
				cfg.LogRetentionDays = *pc.LogRetentionDays
			}
		}
	}
	if !explicitFlags["log-max-bytes"] {
		if v := os.Getenv("FABRIK_LOG_MAX_BYTES"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				cfg.LogMaxBytes = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_LOG_MAX_BYTES=%q is invalid (must be a non-negative integer of bytes; 0 = disable); using default 2147483648\n", v)
			}
		} else if pc.LogMaxBytes != nil {
			if *pc.LogMaxBytes < 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml log_max_bytes=%d is invalid (must be a non-negative integer; 0 = disable); using default 2147483648\n", *pc.LogMaxBytes)
			} else {
				cfg.LogMaxBytes = *pc.LogMaxBytes
			}
		}
	}
	if !explicitFlags["kill-grace-sigint"] {
		if v := os.Getenv("FABRIK_KILL_GRACE_SIGINT"); v != "" {
			cfg.KillGraceSigInt = v // validated in killGraceSigInt() helper
		}
	}
	if !explicitFlags["kill-grace-sigterm"] {
		if v := os.Getenv("FABRIK_KILL_GRACE_SIGTERM"); v != "" {
			cfg.KillGraceSigTerm = v // validated in killGraceSigTerm() helper
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
	if !cfg.SymlinkEnv {
		if v := os.Getenv("FABRIK_SYMLINK_ENV"); v != "" {
			lv := strings.ToLower(v)
			cfg.SymlinkEnv = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.SymlinkEnv {
			cfg.SymlinkEnv = true
		}
	}
	if !explicitFlags["worktree-boundary-audit"] {
		if v := os.Getenv("FABRIK_WORKTREE_BOUNDARY_AUDIT"); v != "" {
			lv := strings.ToLower(v)
			cfg.WorktreeBoundaryAudit = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.WorktreeBoundaryAudit {
			cfg.WorktreeBoundaryAudit = true
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
	if !explicitFlags["status-poll"] {
		if v := os.Getenv("FABRIK_STATUS_POLL"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.StatusPollSeconds = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_STATUS_POLL=%q is invalid (must be a positive integer); using default 15\n", v)
			}
		} else if pc.StatusPoll != nil {
			if *pc.StatusPoll <= 0 {
				fmt.Fprintf(os.Stderr, "[warn] config.yaml status_poll=%d is invalid (must be a positive integer); using default 15\n", *pc.StatusPoll)
			} else {
				cfg.StatusPollSeconds = *pc.StatusPoll
			}
		}
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

	// If the process was re-exec'd after an auto-upgrade, conditionally refresh
	// embedded plugin skills. Three-way comparison: skip refresh when operator
	// has local customizations (disk ≠ installed-version).
	if os.Getenv("FABRIK_AUTO_UPGRADED") == "1" {
		os.Unsetenv("FABRIK_AUTO_UPGRADED")
		customWorkflowOnReexec, upgradeNeededOnReexec, stateErr := fabrikplugin.CheckPluginState(".fabrik/plugin")
		if stateErr != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin state check failed: %v\n", stateErr)
		} else if customWorkflowOnReexec {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skills have local customizations — skipping auto-refresh; run 'fabrik upgrade --force' to overwrite\n")
		} else if upgradeNeededOnReexec {
			if _, err := fabrikplugin.RefreshPlugin(); err != nil {
				fmt.Fprintf(os.Stderr, "[upgrade] warning: RefreshPlugin failed: %v\n", err)
			} else if err := fabrikplugin.WriteInstalledVersion(".fabrik/plugin"); err != nil {
				fmt.Fprintf(os.Stderr, "[upgrade] warning: writing installed version failed: %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "[upgrade] info: plugin baseline seeded; skill refresh deferred to next startup\n")
		}
	}

	// If the process was re-exec'd by a SIGHUP restart, conditionally refresh
	// plugin skills using the same three-way comparison. The env var is unset
	// immediately so Claude child processes do not inherit it.
	if os.Getenv("FABRIK_SIGHUP_RESTART") == "1" {
		os.Unsetenv("FABRIK_SIGHUP_RESTART")
		customWorkflowOnSighup, upgradeNeededOnSighup, stateErr := fabrikplugin.CheckPluginState(".fabrik/plugin")
		if stateErr != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin state check failed after SIGHUP restart: %v\n", stateErr)
		} else if customWorkflowOnSighup {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skills have local customizations — skipping auto-refresh; run 'fabrik upgrade --force' to overwrite\n")
		} else if upgradeNeededOnSighup {
			if _, err := fabrikplugin.RefreshPlugin(); err != nil {
				fmt.Fprintf(os.Stderr, "[upgrade] warning: RefreshPlugin failed after SIGHUP restart: %v\n", err)
			} else if err := fabrikplugin.WriteInstalledVersion(".fabrik/plugin"); err != nil {
				fmt.Fprintf(os.Stderr, "[upgrade] warning: writing installed version failed after SIGHUP restart: %v\n", err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "[upgrade] info: plugin baseline seeded; skill refresh deferred to next startup\n")
		}
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

	if testResolvedConfigHook != nil {
		testResolvedConfigHook(*cfg)
		return nil
	}

	eng, err := engine.New(engine.Config{
		Owner:                    cfg.Owner,
		Repo:                     cfg.Repo,
		ProjectNum:               cfg.ProjectNum,
		OwnerType:                cfg.OwnerType,
		User:                     cfg.User,
		Token:                    cfg.Token,
		Version:                  Version,
		Yolo:                     cfg.Yolo,
		AutoUpgrade:              cfg.AutoUpgrade,
		GitSSH:                   cfg.GitSSH,
		PollSeconds:              cfg.PollSeconds,
		MaxConcurrent:            cfg.MaxConcurrent,
		MaxRetries:               cfg.MaxRetries,
		ReviewWaitTimeout:        reviewWaitTimeout(cfg.ReviewWaitTimeout),
		MaxReviewCycles:          maxReviewCycles(cfg.MaxReviewCycles),
		CIWaitTimeout:            ciWaitTimeout(cfg.CIWaitTimeout),
		PostPushDwell:            postPushDwell(cfg.PostPushDwell),
		WorkerStaleTimeout:       workerStaleTimeout(cfg.WorkerStaleMins),
		MaxCiFixCycles:           maxCiFixCycles(cfg.MaxCiFixCycles),
		MaxRebaseCycles:          maxRebaseCycles(cfg.MaxRebaseCycles),
		MaxEnqueueCycles:         maxEnqueueCycles(cfg.MaxEnqueueCycles),
		ConvergenceBudget:        convergenceBudget(cfg.ConvergenceBudget),
		AutoMergeStrategy:        autoMergeStrategy(cfg.AutoMergeStrategy),
		MergeQueue:               mergeQueueMode(cfg.MergeQueue),
		MergeTrain:               mergeTrainMode(cfg.MergeTrain),
		MaxMergeTrainEjections:   3, // ADR-059 default
		MaxBatchSize:             cfg.MaxBatchSize,                                // 0 = derive default (5) in engine
		MaxBisectValidations:     cfg.MaxBisectValidations,                        // 0 = derive default in engine
		MaxTrainRebaseCycles:     cfg.MaxTrainRebaseCycles,                        // 0 = derive default (3) in engine
		MaxTrainTrialsPerWindow:  cfg.MaxTrainTrialsPerWindow,                     // 0 = derive default (20) in engine
		TrainTrialWindowDuration: trainTrialWindowDuration(cfg.TrainTrialWindowMinutes), // 0 = derive default (60m) in engine
		ClaudeWaitDelay:          claudeWaitDelay(cfg.ClaudeWaitDelay),
		KillGraceSigInt:          killGraceSigInt(cfg.KillGraceSigInt),
		KillGraceSigTerm:         killGraceSigTerm(cfg.KillGraceSigTerm),
		DebugOutput:              cfg.DebugOutput,
		SymlinkEnv:               cfg.SymlinkEnv,
		WorktreeBoundaryAudit:    cfg.WorktreeBoundaryAudit,
		PluginDir:                cfg.PluginDir,
		Stages:                   stageCfgs,
		Webhooks:                 cfg.Webhooks,
		WebhookPort:              cfg.WebhookPort,
		WebhookEvents:            webhookEvents,
		ProjectStatusPollSeconds: statusPollSeconds(cfg.StatusPollSeconds),
		ReconcileInterval:        reconcileIntervalDuration(cfg.ReconcileInterval),
		JanitorIntervalHours:     cfg.JanitorIntervalHours,
		LogRetentionDays:         cfg.LogRetentionDays,
		LogMaxBytes:              cfg.LogMaxBytes,
		ReadyCh:                  testReadyCh,
	})
	if err != nil {
		return err
	}

	// Evaluate plugin skill staleness and customization state after engine.New()
	// (so env vars are unset and the inheritance window is closed).
	// Guard with os.Stat so projects that haven't run `fabrik init` (no
	// .fabrik/plugin/ dir) always report stale count 0.
	var skillsStaleCount int
	var customWorkflow bool
	if _, statErr := os.Stat(".fabrik/plugin"); statErr == nil {
		cw, upgradeNeeded, cwErr := fabrikplugin.CheckPluginState(".fabrik/plugin")
		if cwErr != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin state check failed: %v\n", cwErr)
		} else {
			customWorkflow = cw
			if upgradeNeeded {
				if diffing, diffErr := diffingPluginFiles(".fabrik/plugin"); diffErr != nil {
					fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skill check failed: %v\n", diffErr)
				} else {
					skillsStaleCount = len(diffing)
				}
			}
		}
	}

	// When TUI is enabled (and we have a real terminal), run the bubbletea
	// TUI. Otherwise fall through to plain-text mode.
	if useTUI {
		wakeCh := make(chan struct{}, 1)
		eng.SetWakeCh(wakeCh)
		stopCh := make(chan tui.StopRequest, 1)
		eng.SetStopCh(stopCh)
		return runTUI(eng, cfg.PollSeconds, buildProjectInfo(cfg, pc), cfg.PluginDir, wakeCh, stopCh, skillsStaleCount, customWorkflow)
	}
	if customWorkflow {
		fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skills have local customizations — skipping auto-refresh; run 'fabrik upgrade --force' to overwrite\n")
	} else if skillsStaleCount > 0 {
		if refreshErr := checkPluginSkillsWithReader(".fabrik/plugin", false, nil); refreshErr != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skill refresh failed: %v\n", refreshErr)
		}
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

// workerStaleTimeout converts a WorkerStaleMins config value (minutes) to a
// time.Duration. When minutes is 0 (unset), the default of 5 minutes is used.
func workerStaleTimeout(minutes int) time.Duration {
	if minutes <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

// trainTrialWindowDuration converts a TrainTrialWindowMinutes config value (minutes) to a
// time.Duration for the runaway guard rolling window (ADR-059 D8). When minutes is 0
// (unset), returns 0 so the engine applies its own default of 60 minutes.
func trainTrialWindowDuration(minutes int) time.Duration {
	if minutes <= 0 {
		return 0
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

// maxEnqueueCycles returns the configured MaxEnqueueCycles value, defaulting to 5
// when n is 0 (unset). Bounds merge-queue re-enqueue trips so a queue-thrash loop
// (enqueue → eject → re-enqueue → eject) pauses for a human (ADR-058 D4 FR-3).
func maxEnqueueCycles(n int) int {
	if n <= 0 {
		return 5
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

// postPushDwell converts a PostPushDwell config value (seconds) to a
// time.Duration. When seconds is 0 (unset), the default of 90 seconds is used.
func postPushDwell(seconds int) time.Duration {
	if seconds <= 0 {
		return 90 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// statusPollSeconds returns the configured ProjectStatusPollSeconds, defaulting
// to 15 s when n is 0 (unset). The gate now runs every poll cycle so the default
// matches the main poll cadence rather than the former goroutine ticker period.
func statusPollSeconds(n int) int {
	if n <= 0 {
		return 15
	}
	return n
}

// reconcileIntervalDuration converts a ReconcileInterval config value (seconds)
// to a time.Duration. When seconds is 0 (unset), returns 0 so the engine falls
// back to its lightReconcileInterval default (3 minutes).
func reconcileIntervalDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// killGraceSigInt parses a kill-grace-sigint string (Go duration syntax) into a
// time.Duration. An empty string returns the default of 10 seconds. "0s" returns
// zero (meaning: skip the SIGINT step in the kill escalation sequence). Negative
// values and invalid syntax log a warning and return the default.
func killGraceSigInt(s string) time.Duration {
	if s == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_KILL_GRACE_SIGINT=%q is invalid (Go duration syntax required, e.g. 10s, 0s); using default 10s\n", s)
		return 10 * time.Second
	}
	if d < 0 {
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_KILL_GRACE_SIGINT=%q is negative; using default 10s\n", s)
		return 10 * time.Second
	}
	return d
}

// killGraceSigTerm parses a kill-grace-sigterm string (Go duration syntax) into a
// time.Duration. An empty string returns the default of 10 seconds. Negative values
// and invalid syntax log a warning and return the default.
func killGraceSigTerm(s string) time.Duration {
	if s == "" {
		return 10 * time.Second
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_KILL_GRACE_SIGTERM=%q is invalid (Go duration syntax required, e.g. 10s); using default 10s\n", s)
		return 10 * time.Second
	}
	if d < 0 {
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_KILL_GRACE_SIGTERM=%q is negative; using default 10s\n", s)
		return 10 * time.Second
	}
	return d
}

// convergenceBudget parses a convergence budget string (Go duration syntax) into
// a time.Duration. An empty string returns the default of 30 minutes. "0" or "0s"
// disables the bounded budget (returns 0). Invalid values log a warning and return
// the default.
func convergenceBudget(s string) time.Duration {
	if s == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_CONVERGENCE_BUDGET=%q is invalid (Go duration syntax required, e.g. 30m, 1h); using default 30m\n", s)
		return 30 * time.Minute
	}
	if d < 0 {
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_CONVERGENCE_BUDGET=%q is negative; using default 30m\n", s)
		return 30 * time.Minute
	}
	return d
}

// autoMergeStrategy validates and returns the merge strategy string for
// enablePullRequestAutoMerge. Accepts "MERGE", "SQUASH", or "REBASE" (case
// insensitive). Returns "MERGE" for empty input or unrecognised values (with a
// warning).
func autoMergeStrategy(s string) string {
	if s == "" {
		return "MERGE"
	}
	upper := strings.ToUpper(s)
	switch upper {
	case "MERGE", "SQUASH", "REBASE":
		return upper
	default:
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_AUTO_MERGE_STRATEGY=%q is invalid (must be MERGE, SQUASH, or REBASE); using default MERGE\n", s)
		return "MERGE"
	}
}

// mergeQueueMode normalizes the --merge-queue / FABRIK_MERGE_QUEUE value.
// Valid values are "auto" and "off" (case-insensitive). An empty string defaults to "auto".
// Unrecognized values produce a warning and fall back to "auto".
func mergeQueueMode(s string) string {
	if s == "" {
		return "auto"
	}
	lower := strings.ToLower(s)
	switch lower {
	case "auto", "off":
		return lower
	default:
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_MERGE_QUEUE=%q is invalid (must be auto or off); using default auto\n", s)
		return "auto"
	}
}

// mergeTrainMode normalizes the --merge-train / FABRIK_MERGE_TRAIN value.
// Valid values are "on" and "off" (case-insensitive). An empty string defaults to "off".
// Unrecognized values produce a warning and fall back to "off".
func mergeTrainMode(s string) string {
	if s == "" {
		return "off"
	}
	lower := strings.ToLower(s)
	switch lower {
	case "on", "off":
		return lower
	default:
		fmt.Fprintf(os.Stderr, "[warn] FABRIK_MERGE_TRAIN=%q is invalid (must be on or off); using default off\n", s)
		return "off"
	}
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
// customWorkflow is true when operator customizations are detected in .fabrik/plugin/.
func runTUI(eng *engine.Engine, pollSeconds int, info tui.ProjectInfo, pluginDir string, wakeCh chan struct{}, stopCh chan tui.StopRequest, skillsStaleCount int, customWorkflow bool) error {
	events := make(chan tui.Event, 256)
	eng.SetEvents(events)

	tuiModel := tui.New(pollSeconds, info, pluginDir, wakeCh, stopCh, skillsStaleCount, customWorkflow)
	p := tea.NewProgram(tuiModel, tea.WithAltScreen(), tea.WithoutSignalHandler())
	// Register terminal cleanup so force-quit paths (SIGHUP re-exec, second
	// SIGTERM/SIGHUP) release alt-screen before replacing or exiting the process.
	// Must be registered before eng.Run() starts the signal handlers.
	eng.SetCleanupHook(func() { _ = p.ReleaseTerminal() })

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

	finalModel, err := p.Run()
	if err != nil {
		p.ReleaseTerminal()
		// TUI failed — signal the engine to stop so its goroutine and the
		// forwarding goroutine both exit cleanly.
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-errCh
		return err
	}
	p.ReleaseTerminal()

	// Print any pending reconcile prompt the user requested before quitting.
	if fm, ok := finalModel.(tui.Model); ok {
		if prompt := fm.PendingReconcilePrompt(); prompt != "" {
			fmt.Fprintf(os.Stderr, "\nReconciliation prompt (paste into Claude Code):\n\n%s\n", prompt)
		}
	}

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
