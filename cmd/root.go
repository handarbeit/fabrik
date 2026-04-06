package cmd

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
	"github.com/handarbeit/fabrik/config"
	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

// testReadyCh is set by tests to receive a signal once engine.Run has
// registered its signal handlers. This prevents SIGINT from arriving before
// signal.Notify is installed, which would terminate the process.
var testReadyCh chan struct{}

type Config struct {
	Owner         string
	Repo          string
	ProjectNum    int
	OwnerType     string
	User          string
	Token         string
	StagesDir     string
	Yolo          bool
	AutoUpgrade   bool
	TUI           bool
	PollSeconds   int
	MaxConcurrent int
	MaxRetries    int
	DebugOutput   bool
	PluginDir     string
	Terminal      string
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
	flag.BoolVar(&cfg.AutoUpgrade, "auto-upgrade", false, "When idle, check for new commits on origin/main and self-upgrade (for fabrik developing itself)")
	flag.BoolVar(&cfg.TUI, "tui", false, "Enable the interactive TUI dashboard (default: plain-text log output)")
	flag.IntVar(&cfg.PollSeconds, "poll", 30, "Polling interval in seconds")
	flag.IntVar(&cfg.MaxConcurrent, "max-concurrent", 5, "Maximum number of concurrent issue workers")
	flag.IntVar(&cfg.MaxRetries, "max-retries", 3, "Max failed stage attempts before pausing the issue (0 = unlimited)")
	flag.BoolVar(&cfg.DebugOutput, "debug-output", false, "Save Claude stage output to .fabrik/debug/ for debugging")
	flag.StringVar(&cfg.PluginDir, "plugin-dir", "", "Path to Fabrik plugin directory (for development; overrides installed plugin)")
	flag.StringVar(&cfg.Terminal, "terminal", "", "Terminal emulator to use for log viewer (terminal, iterm2, ghostty, kitty, alacritty, warp)")

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
	if !cfg.AutoUpgrade {
		if v := os.Getenv("FABRIK_AUTO_UPGRADE"); v != "" {
			lv := strings.ToLower(v)
			cfg.AutoUpgrade = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.AutoUpgrade {
			cfg.AutoUpgrade = true
		}
	}
	if !cfg.TUI {
		if v := os.Getenv("FABRIK_TUI"); v != "" {
			lv := strings.ToLower(v)
			cfg.TUI = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.TUI {
			cfg.TUI = true
		}
	}
	if !cfg.DebugOutput {
		if v := os.Getenv("FABRIK_DEBUG_OUTPUT"); v != "" {
			lv := strings.ToLower(v)
			cfg.DebugOutput = lv == "true" || lv == "1" || lv == "yes"
		} else if pc.DebugOutput {
			cfg.DebugOutput = true
		}
	}
	if cfg.Terminal == "" {
		if v := os.Getenv("FABRIK_TERMINAL"); v != "" {
			cfg.Terminal = v
		} else if pc.Terminal != "" {
			cfg.Terminal = pc.Terminal
		} else {
			cfg.Terminal = detectTerminalFromEnv()
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

	// TUI requires --tui flag AND a real terminal on both stdin and stdout.
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
		if _, err := refreshPlugin(); err != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: refreshPlugin failed: %v\n", err)
		}
	}

	eng, err := engine.New(engine.Config{
		Owner:         cfg.Owner,
		Repo:          cfg.Repo,
		ProjectNum:    cfg.ProjectNum,
		OwnerType:     cfg.OwnerType,
		User:          cfg.User,
		Token:         cfg.Token,
		Version:       Version,
		Yolo:          cfg.Yolo,
		AutoUpgrade:   cfg.AutoUpgrade,
		PollSeconds:   cfg.PollSeconds,
		MaxConcurrent: cfg.MaxConcurrent,
		MaxRetries:    cfg.MaxRetries,
		DebugOutput:   cfg.DebugOutput,
		PluginDir:     cfg.PluginDir,
		Stages:        stageCfgs,
		ReadyCh:       testReadyCh,
	})
	if err != nil {
		return err
	}

	// When --tui is enabled (and we have a real terminal), run the bubbletea
	// TUI. Otherwise fall through to plain-text mode.
	if useTUI {
		return runTUI(eng, cfg.PollSeconds, buildProjectInfo(cfg, pc), cfg.Terminal, cfg.PluginDir)
	}
	return eng.Run()
}

// detectTerminalFromEnv maps the TERM_PROGRAM environment variable to a Fabrik
// terminal identifier. Returns "" if TERM_PROGRAM is not set or not recognized,
// causing openTerminalCmd to fall back to the platform default.
func detectTerminalFromEnv() string {
	switch os.Getenv("TERM_PROGRAM") {
	case "Apple_Terminal":
		return "terminal"
	case "iTerm.app":
		return "iterm2"
	case "ghostty":
		return "ghostty"
	case "WarpTerminal":
		return "warp"
	case "alacritty":
		return "alacritty"
	default:
		return ""
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

	return tui.ProjectInfo{
		CWD:           cwdDisplay,
		Repo:          cfg.Owner + "/" + cfg.Repo,
		Version:       version,
		FabrikVersion: Version,
	}
}

// runTUI wires the event channel, starts the bubbletea program, and runs the
// engine. The engine handles SIGINT itself; bubbletea uses WithoutSignalHandler
// so it doesn't interfere. When the engine exits, the TUI is quit.
func runTUI(eng *engine.Engine, pollSeconds int, info tui.ProjectInfo, terminal string, pluginDir string) error {
	events := make(chan tui.Event, 256)
	eng.SetEvents(events)

	tuiModel := tui.New(pollSeconds, info, terminal, pluginDir)
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
		// TUI failed — signal the engine to stop so its goroutine and the
		// forwarding goroutine both exit cleanly.
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-errCh
		return err
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
