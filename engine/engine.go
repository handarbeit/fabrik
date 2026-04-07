package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

type Config struct {
	Owner             string
	Repo              string
	ProjectNum        int
	OwnerType         string
	User              string
	Token             string
	Version           string
	Yolo              bool
	AutoUpgrade       bool
	PollSeconds       int
	MaxConcurrent     int
	MaxRetries        int
	ReviewWaitTimeout time.Duration // How long to wait for PR reviewers before auto-advancing anyway (default 15m)
	DebugOutput       bool
	PluginDir         string
	Stages            []*stages.Stage
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
	jobControlMode     bool                        // true when running from a non-git directory (multi-repo)
	jobControlDir      string                      // fabrikDir when jobControlMode is true
	mu                 sync.Mutex
	processedSet       map[string]time.Time  // key: "owner/repo#N-stageName" or "owner/repo#N-comment-ID"
	lockedIssues       map[string]bool       // key: "owner/repo#N"; issues with fabrik:locked added but not yet released
	totalTokens        TokenUsage            // accumulated token usage since process start
	lastReportedCost   float64               // cost at last [stats] report; skip repeat prints when unchanged
	retryCount         map[string]int        // key: "owner/repo#N-stageName", value: failed attempt count
	pausedDueToRetries map[string]bool       // key: "owner/repo#N-stageName", true if engine paused this issue
	lastUsage          map[string]TokenUsage // key: issueKey; per-issue token usage from last processItem (for TUI)
	lastCompleted      map[string]bool       // key: issueKey; per-issue stage completion from last processItem (for TUI)
	lastBlocked        map[string]bool       // key: issueKey; per-issue blocked-on-input from last processItem (for TUI)
	lastUpdatedAt        map[string]time.Time // key: issueKey; tracks last-seen updatedAt per issue
	deepFetchFailureTime map[string]time.Time // key: issueKey; tracks when FetchItemDetails last failed
	idleCount          int                   // consecutive idle polls; triggers self-upgrade at threshold
	sem                chan struct{}         // semaphore bounding concurrent workers across poll cycles
	wg                 sync.WaitGroup        // tracks in-flight workers for graceful shutdown
	inFlight           sync.Map              // key: issueKey string, value: bool (isPR)
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
	var jobControlMode bool
	repoDir, err := gitToplevel()
	if err != nil {
		// Not in a git repo — job-control directory for multi-repo projects.
		// Repos are cloned lazily by ensureRepoReady when the first issue for each repo arrives.
		fabrikDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolving working directory: %w", err)
		}
		jobControlMode = true
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
		jobControlMode:     jobControlMode,
		jobControlDir:      fabrikDir,
		processedSet:         make(map[string]time.Time),
		lockedIssues:         make(map[string]bool),
		lastUsage:            make(map[string]TokenUsage),
		lastCompleted:        make(map[string]bool),
		lastBlocked:          make(map[string]bool),
		lastUpdatedAt:        make(map[string]time.Time),
		deepFetchFailureTime: make(map[string]time.Time),
		retryCount:           make(map[string]int),
		pausedDueToRetries:   make(map[string]bool),
		sem:                  make(chan struct{}, cfg.MaxConcurrent),
	}

	if !jobControlMode && cfg.Repo != "" {
		// Single-repo git-repo mode: register the configured repo's WM upfront.
		nameWithOwner := cfg.Owner + "/" + cfg.Repo
		// Use "owner-repo" as the directory segment to avoid cross-owner collisions.
		dirName := cfg.Owner + "-" + cfg.Repo
		wm := NewWorktreeManagerForRepo(repoDir, worktreeRoot, dirName)
		eng.worktreeManagers[nameWithOwner] = wm
		wm.logfFn = eng.logf
	}

	// Migrate any old-style worktrees (issue-N/) to the new per-repo layout.
	migrateWorktrees(worktreeRoot, func(msg string) { fmt.Printf("[startup] %s", msg) })

	// Migrate any old-style session files (issue-N/) to the new per-repo layout.
	// Must run after migrateWorktrees so namespaced worktree paths exist for remote lookup.
	home, _ := os.UserHomeDir()
	migrateSessions(
		filepath.Join(home, ".fabrik", "sessions"),
		worktreeRoot,
		func(msg string) { fmt.Printf("[startup] %s", msg) },
	)

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
		processedSet:         make(map[string]time.Time),
		lockedIssues:         make(map[string]bool),
		lastUsage:            make(map[string]TokenUsage),
		lastCompleted:        make(map[string]bool),
		lastBlocked:          make(map[string]bool),
		lastUpdatedAt:        make(map[string]time.Time),
		deepFetchFailureTime: make(map[string]time.Time),
		retryCount:           make(map[string]int),
		pausedDueToRetries:   make(map[string]bool),
		sem:                  make(chan struct{}, maxConcurrent),
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

// isFabrikSourceCheckout reports whether dir is a git checkout of the fabrik
// source repo (tenaciousvc/fabrik or handarbeit/fabrik). Returns false on any
// error (no git, no remote, wrong remote, etc.).
func isFabrikSourceCheckout(dir string) bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	for _, pattern := range []string{"tenaciousvc/fabrik", "handarbeit/fabrik"} {
		if strings.Contains(url, pattern) {
			return true
		}
	}
	return false
}

// devCheckout returns the WorktreeManager for Fabrik's own source checkout, or nil.
// Only available in dev mode (version starts with "dev") when running from Fabrik's own repo.
// Used by the dev upgrade path (git pull + go build); the release upgrade path has no dependency on this.
func (e *Engine) devCheckout() *WorktreeManager {
	key := e.defaultRepo()
	if key == "" {
		return nil
	}
	e.mu.Lock()
	wm := e.worktreeManagers[key]
	e.mu.Unlock()
	if wm == nil {
		return nil
	}
	if !isFabrikSourceCheckout(wm.BaseDir()) {
		e.logf(0, "upgrade", "not in fabrik source checkout — skipping dev auto-upgrade\n")
		return nil
	}
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
	owner, rname := parseOwnerRepo(nameWithOwner)
	// Use "owner-repo" as directory segment to avoid cross-owner collisions.
	dirName := owner + "-" + rname
	wm := NewWorktreeManagerForRepo(baseDir, worktreeRoot, dirName)
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

// ErrSkipItem is returned by ensureRepoReady when a repo cannot be cloned and
// the item should be skipped for this poll cycle (but not surfaced as an error).
var ErrSkipItem = errors.New("skip item")

// ensureRepoReady guarantees that a WorktreeManager exists for the repo that
// owns item. In single-repo git mode the WM is already registered at startup,
// so this is a no-op. In job-control (multi-repo) mode it lazily bare-clones
// the repo on first access. If the clone fails it posts a comment, adds
// fabrik:paused and fabrik:awaiting-input labels, records a history entry, and
// returns ErrSkipItem so the caller skips without treating it as a hard error.
func (e *Engine) ensureRepoReady(ctx context.Context, item gh.ProjectItem) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if owner == "" || repo == "" {
		return nil // cannot determine repo — let processItem handle it
	}
	nameWithOwner := owner + "/" + repo

	e.mu.Lock()
	_, registered := e.worktreeManagers[nameWithOwner]
	e.mu.Unlock()
	if registered {
		return nil
	}

	// Job-control mode: clone the repo bare.
	worktreeRoot := filepath.Join(e.jobControlDir, ".fabrik", "worktrees")
	bareDir, err := ensureBareClone(e.jobControlDir, owner, repo)
	if err != nil {
		msg := fmt.Sprintf("🏭 **Fabrik — cannot clone repo**\n\nFailed to clone `%s/%s`:\n```\n%v\n```\nHuman intervention required. Fix the clone issue and remove `fabrik:paused` to retry.", owner, repo, err)
		if commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
			e.logf(item.Number, "warn", "could not post clone-failure comment: %v\n", commentErr)
		}
		if labelErr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); labelErr != nil {
			e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", labelErr)
		}
		if labelErr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); labelErr != nil {
			e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", labelErr)
		}
		// Append a history entry so the TUI records the failure.
		hist := tui.LoadHistory()
		hist = append(hist, tui.HistoryEntry{
			IssueNumber: item.Number,
			Repo:        nameWithOwner,
			Title:       item.Title,
			StageName:   "clone",
			Success:     false,
			CompletedAt: time.Now(),
		})
		tui.SaveHistory(hist)
		e.logf(item.Number, "error", "cannot clone repo %s: %v — pausing issue\n", nameWithOwner, err)
		return ErrSkipItem
	}

	e.registerWorktrees(nameWithOwner, bareDir, worktreeRoot)
	return nil
}
