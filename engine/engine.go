package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

type Config struct {
	Owner         string
	Repo          string
	ProjectNum    int
	User          string
	Token         string
	Yolo          bool
	AutoUpgrade   bool
	PollSeconds   int
	MaxConcurrent int
	MaxRetries    int
	DebugOutput   bool
	PluginDir     string
	Stages        []*stages.Stage
	// ReadyCh is closed once Run() has registered signal handlers. Tests use
	// this to avoid sending SIGINT before signal.Notify is installed.
	ReadyCh chan struct{}
}

type Engine struct {
	cfg                Config
	client             GitHubClient
	claude             ClaudeInvoker
	statusField        *gh.StatusField
	worktreeManagers   map[string]*WorktreeManager // key: "owner/repo"; one WM per discovered repo
	mu                 sync.Mutex
	processedSet       map[string]time.Time // key: "owner/repo#N-stageName" or "owner/repo#N-comment-ID"
	lockedIssues       map[string]bool      // key: "owner/repo#N"; issues with fabrik:locked added but not yet released
	totalTokens        TokenUsage           // accumulated token usage since process start
	lastReportedCost   float64              // cost at last [stats] report; skip repeat prints when unchanged
	retryCount         map[string]int       // key: "owner/repo#N-stageName", value: failed attempt count
	pausedDueToRetries map[string]bool      // key: "owner/repo#N-stageName", true if engine paused this issue
	lastUsage          map[string]TokenUsage   // key: issueKey; per-issue token usage from last processItem (for TUI)
	lastCompleted      map[string]bool         // key: issueKey; per-issue stage completion from last processItem (for TUI)
	lastBlocked        map[string]bool         // key: issueKey; per-issue blocked-on-input from last processItem (for TUI)
	lastUpdatedAt      map[string]time.Time    // key: issueKey; tracks last-seen updatedAt per issue
	idleCount          int                  // consecutive idle polls; triggers self-upgrade at threshold
	sem                chan struct{}         // semaphore bounding concurrent workers across poll cycles
	wg                 sync.WaitGroup       // tracks in-flight workers for graceful shutdown
	inFlight           sync.Map             // key: issueKey string, value: bool (isPR)
	events             chan tui.Event        // nil in tests / plain-text mode; TUI goroutine consumes
}

func New(cfg Config) (*Engine, error) {
	// Resolve working directory. If we're in a git repo, use the repo root.
	// If not (job-control directory for multi-repo projects), use cwd and
	// clone the target repo as a bare repo for worktree operations.
	// fabrikDir is the directory containing .fabrik/ (stages, plugin, config).
	// repoDir is the git repo root used for worktree operations.
	// In a git repo, they're the same. In a job-control directory, fabrikDir
	// is cwd and repoDir is the bare clone at .fabrik/repo.git.
	var fabrikDir string
	repoDir, err := gitToplevel()
	if err != nil {
		// Not in a git repo — job-control directory for multi-repo projects.
		fabrikDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolving working directory: %w", err)
		}
		if cloneErr := ensureBareClone(fabrikDir, cfg.Owner, cfg.Repo); cloneErr != nil {
			return nil, fmt.Errorf("cloning target repo: %w", cloneErr)
		}
		repoDir = filepath.Join(fabrikDir, ".fabrik", "repo.git")
	} else {
		fabrikDir = repoDir
	}

	// Default to .fabrik/plugin in the fabrik dir (created by fabrik init).
	// --plugin-dir flag overrides this for development.
	// Path must be absolute since Claude runs in the worktree, not the repo root.
	pluginDir := cfg.PluginDir
	if pluginDir == "" {
		defaultPluginDir := filepath.Join(fabrikDir, ".fabrik", "plugin")
		if fi, err := os.Stat(defaultPluginDir); err == nil && fi.IsDir() {
			pluginDir = defaultPluginDir
		}
	}
	if pluginDir != "" {
		if abs, err := filepath.Abs(pluginDir); err == nil {
			pluginDir = abs
		}
		claudePluginDir = pluginDir
	}
	worktreeRoot := filepath.Join(fabrikDir, ".fabrik", "worktrees")
	eng := &Engine{
		cfg:                cfg,
		client:             gh.NewClient(cfg.Token),
		claude:             &RealClaudeInvoker{DebugOutput: cfg.DebugOutput},
		worktreeManagers:   make(map[string]*WorktreeManager),
		processedSet:       make(map[string]time.Time),
		lockedIssues:       make(map[string]bool),
		lastUsage:          make(map[string]TokenUsage),
		lastCompleted:      make(map[string]bool),
		lastBlocked:        make(map[string]bool),
		lastUpdatedAt:      make(map[string]time.Time),
		retryCount:         make(map[string]int),
		pausedDueToRetries: make(map[string]bool),
		sem:                make(chan struct{}, cfg.MaxConcurrent),
	}

	if cfg.Repo != "" {
		// Single-repo mode: register the configured repo's WM upfront.
		nameWithOwner := cfg.Owner + "/" + cfg.Repo
		rName := cfg.Repo
		wm := NewWorktreeManagerForRepo(repoDir, worktreeRoot, rName)
		eng.worktreeManagers[nameWithOwner] = wm
		wm.logfFn = eng.logf
	}
	// In multi-repo mode (cfg.Repo == ""), WMs are registered lazily in ensureRepoReady.
	return eng, nil
}

// NewWithDeps creates an Engine with explicit dependencies (for testing).
// worktrees is a convenience parameter: if non-nil, it is registered as the WM
// for cfg.Owner+"/"+cfg.Repo (or "_test/_test" when cfg is empty).
func NewWithDeps(cfg Config, client GitHubClient, claude ClaudeInvoker, worktrees *WorktreeManager) *Engine {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	wms := make(map[string]*WorktreeManager)
	eng := &Engine{
		cfg:                cfg,
		client:             client,
		claude:             claude,
		worktreeManagers:   wms,
		processedSet:       make(map[string]time.Time),
		lockedIssues:       make(map[string]bool),
		lastUsage:          make(map[string]TokenUsage),
		lastCompleted:      make(map[string]bool),
		lastBlocked:        make(map[string]bool),
		lastUpdatedAt:      make(map[string]time.Time),
		retryCount:         make(map[string]int),
		pausedDueToRetries: make(map[string]bool),
		sem:                make(chan struct{}, maxConcurrent),
	}
	if worktrees != nil {
		worktrees.logfFn = eng.logf
		key := cfg.Owner + "/" + cfg.Repo
		if key == "/" {
			key = "_test/_test"
		}
		wms[key] = worktrees
	}
	return eng
}

// defaultRepo returns "owner/repo" from cfg, or "" if both are empty.
func (e *Engine) defaultRepo() string {
	if e.cfg.Owner == "" && e.cfg.Repo == "" {
		return ""
	}
	return e.cfg.Owner + "/" + e.cfg.Repo
}

// worktreesFor returns the WorktreeManager for the given "owner/repo" key.
// Panics if no WM is registered for that repo — callers must call ensureRepoReady first.
func (e *Engine) worktreesFor(nameWithOwner string) *WorktreeManager {
	if nameWithOwner == "" {
		nameWithOwner = e.defaultRepo()
	}
	e.mu.Lock()
	wm, ok := e.worktreeManagers[nameWithOwner]
	e.mu.Unlock()
	if !ok {
		panic(fmt.Sprintf("engine: no WorktreeManager registered for repo %q — ensureRepoReady not called", nameWithOwner))
	}
	return wm
}

// primaryWorktrees returns the WorktreeManager for the configured primary repo, or nil.
// Used by operations that don't have a per-issue repo (e.g. auto-upgrade).
func (e *Engine) primaryWorktrees() *WorktreeManager {
	key := e.defaultRepo()
	if key == "" {
		return nil
	}
	e.mu.Lock()
	wm := e.worktreeManagers[key]
	e.mu.Unlock()
	return wm
}

// registerWorktrees adds a WorktreeManager for nameWithOwner to the map.
// Idempotent: if a WM is already registered for this repo, returns the existing one.
func (e *Engine) registerWorktrees(nameWithOwner, baseDir, worktreeRoot string) *WorktreeManager {
	e.mu.Lock()
	defer e.mu.Unlock()
	if wm, ok := e.worktreeManagers[nameWithOwner]; ok {
		return wm
	}
	_, rname := parseOwnerRepo(nameWithOwner)
	wm := NewWorktreeManagerForRepo(baseDir, worktreeRoot, rname)
	wm.logfFn = e.logf
	e.worktreeManagers[nameWithOwner] = wm
	return wm
}

// SetEvents configures the event channel. Must be called before Run().
// When set, direct stdout writes (pollStatus/pollStatusClear) are suppressed
// because the TUI owns the terminal.
func (e *Engine) SetEvents(ch chan tui.Event) {
	e.events = ch
	tuiMode = ch != nil
	claudeLogf = e.logf
	claudeTUI = ch != nil
	// Update logfFn for all registered WorktreeManagers.
	e.mu.Lock()
	for _, wm := range e.worktreeManagers {
		wm.logfFn = e.logf
	}
	e.mu.Unlock()
}

// emit sends an event to the channel without blocking. Dropped if the channel is full.
// Use for high-frequency log events where occasional drops are acceptable.
func (e *Engine) emit(ev tui.Event) {
	if e.events == nil {
		return
	}
	select {
	case e.events <- ev:
	default:
	}
}

// emitStructural sends a structural event (JobStarted, JobCompleted, PollStarted,
// PollCompleted) synchronously. Unlike emit/logf, this blocks if the channel is
// full, but events are never dropped. Use only for low-frequency events that must
// not be lost. Callers are worker goroutines or the poll goroutine; the 256-deep
// buffer ensures blocking is extremely rare in normal operation.
func (e *Engine) emitStructural(ev tui.Event) {
	if e.events == nil {
		return
	}
	e.events <- ev
}

// logf emits a LogEvent to the channel (if configured) or prints directly.
// issueNumber == 0 means a poll-level message; it prints as "[tag]" not "[#0 tag]".
func (e *Engine) logf(issueNumber int, tag, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.events != nil {
		select {
		case e.events <- tui.LogEvent{IssueNumber: issueNumber, Tag: tag, Message: msg}:
		default:
		}
		return
	}
	// Plain-text mode: clear any transient status line before printing.
	pollStatusClear()
	if issueNumber == 0 {
		fmt.Printf("[%s] %s", tag, msg)
	} else {
		fmt.Printf("[#%d %s] %s", issueNumber, tag, msg)
	}
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
