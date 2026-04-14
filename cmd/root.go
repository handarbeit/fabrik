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
	DebugOutput       bool
	PluginDir         string
}

func Execute() error {
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
	cfg := &Config{}

	var versionFlag bool
	flag.BoolVar(&versionFlag, "version", false, "Print the fabrik version and exit")

	flag.StringVar(&cfg.Owner, "owner", "", "GitHub repository owner")
	flag.StringVar(&cfg.Repo, "repo", "", "GitHub repository name")
	flag.IntVar(&cfg.ProjectNum, "project", 0, "GitHub project number")
	flag.StringVar(&cfg.User, "user", "", "GitHub username (only process changes by this user)")
	flag.StringVar(&cfg.Token, "token", "", "GitHub token (or set GITHUB_TOKEN env var)")
	flag.StringVar(&cfg.StagesDir, "stages", "./.fabrik/stages", "Directory containing stage YAML configs")
	flag.BoolVar(&cfg.Yolo, "yolo", false, "Auto-advance issues through stages without waiting for human input")
	flag.BoolVar(&cfg.GitSSH, "ssh", false, "Use SSH clone URLs (git@github.com) instead of HTTPS")
	flag.BoolVar(&cfg.AutoUpgrade, "auto-upgrade", false, "When idle, check GitHub Releases for a newer version and self-upgrade")
	var noTUI bool
	flag.BoolVar(&noTUI, "notui", false, "Disable the interactive TUI dashboard (default: enabled when a real terminal is detected)")
	flag.IntVar(&cfg.PollSeconds, "poll", 30, "Polling interval in seconds")
	flag.IntVar(&cfg.MaxConcurrent, "max-concurrent", 5, "Maximum number of concurrent issue workers")
	flag.IntVar(&cfg.MaxRetries, "max-retries", 3, "Max failed stage attempts before pausing the issue (0 = unlimited)")
	flag.BoolVar(&cfg.DebugOutput, "debug-output", false, "Save Claude stage output to .fabrik/debug/ for debugging")
	flag.StringVar(&cfg.PluginDir, "plugin-dir", "", "Path to Fabrik plugin directory (for development; overrides installed plugin)")

	flag.Parse()

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
	if cfg.ReviewWaitTimeout == 0 {
		if v := os.Getenv("FABRIK_REVIEW_WAIT_TIMEOUT"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.ReviewWaitTimeout = n
			} else {
				fmt.Fprintf(os.Stderr, "[warn] FABRIK_REVIEW_WAIT_TIMEOUT=%q is invalid (must be a positive integer of minutes); using default 15\n", v)
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

	if cfg.Owner == "" || cfg.ProjectNum == 0 {
		fmt.Fprintf(os.Stderr, "Usage: fabrik --owner OWNER --project NUM [--repo REPO] [options]\n\n")
		flag.PrintDefaults()
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
		DebugOutput:       cfg.DebugOutput,
		PluginDir:         cfg.PluginDir,
		Stages:            stageCfgs,
		ReadyCh:           testReadyCh,
	})
	if err != nil {
		return err
	}

	// When TUI is enabled (and we have a real terminal), run the bubbletea
	// TUI. Otherwise fall through to plain-text mode.
	if useTUI {
		return runTUI(eng, cfg.PollSeconds, buildProjectInfo(cfg, pc), cfg.PluginDir)
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
func runTUI(eng *engine.Engine, pollSeconds int, info tui.ProjectInfo, pluginDir string) error {
	events := make(chan tui.Event, 256)
	eng.SetEvents(events)

	tuiModel := tui.New(pollSeconds, info, pluginDir)
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
