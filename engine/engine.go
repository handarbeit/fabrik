package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/verveguy/fabrik/boardcache"
	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/internal/itemstate"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
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
	GitSSH            bool
	PollSeconds       int
	MaxConcurrent     int
	MaxRetries        int
	ReviewWaitTimeout time.Duration // How long to wait for PR reviewers before auto-advancing anyway (default 15m)
	MaxReviewCycles   int           // Max review re-invocation cycles per issue before pausing (default 5)
	CIWaitTimeout     time.Duration // How long to wait for CI in the merge guard before pausing (default 30m)
	MaxCiFixCycles    int           // Max CI-fix re-invocation cycles per issue before pausing (default 5)
	MaxRebaseCycles   int           // Max rebase re-invocation cycles per issue before pausing (default 3)
	ClaudeWaitDelay   time.Duration // How long to wait after Claude exits before giving up on pipe drain and recovering output (default 30s)
	DebugOutput       bool
	PluginDir         string
	Stages            []*stages.Stage
	Webhooks          bool
	WebhookPort       int
	WebhookEvents     []string
	BoardCacheMode           string // "in-memory" or "none"; default "none" when webhooks off, "in-memory" when on
	ProjectStatusPollSeconds int    // Layer 2 status-only sweep cadence in seconds; default 600 (10 min)
	// ReadyCh is closed once Run() has registered signal handlers. Tests use
	// this to avoid sending SIGINT before signal.Notify is installed.
	ReadyCh chan struct{}
}

// cloneCall coordinates concurrent bare-clone attempts for the same repo.
// The first caller to store one in cloneInFlight performs the clone; subsequent
// callers wait on done and share the result.
type cloneCall struct {
	done chan struct{} // closed when clone completes (success or failure)
	dir  string        // bareDir on success; empty on failure
	err  error         // clone error on failure; nil on success
}

type Engine struct {
	cfg                  Config
	client               GitHubClient
	readClient           boardcache.ReadClient // read-only GitHub calls; may be CacheImpl or GitHubAdapter
	claude               ClaudeInvoker
	statusField          *gh.StatusField
	worktreeManagers     map[string]*WorktreeManager // key: "owner/repo"; one WM per discovered repo
	fabrikDir            string                      // directory containing .fabrik/ (always os.Getwd() at startup)
	mu                   sync.Mutex
	store                *itemstate.Store      // per-item engine state (locks, invocation outcomes, deep-fetch, CI-gate); see ADR-036
	totalTokens          TokenUsage            // accumulated token usage since process start
	lastReportedCost     float64               // cost at last [stats] report; skip repeat prints when unchanged
	seenUpdatedAt        map[string]time.Time  // key: issueKey; tracks last-seen updatedAt per issue (deferred to Phase 3-H)
	seededRepos          map[string]bool       // key: "owner/repo"; in-memory guard to avoid re-seeding on every poll
	idleCount            int                   // consecutive idle polls; triggers self-upgrade at threshold
	idleStart            time.Time             // when consecutive idle polls began; zero value = not idle
	wakeCh               chan struct{}         // TUI sends on this to wake the poll loop immediately; nil if no TUI
	sem                  chan struct{}         // semaphore bounding concurrent workers across poll cycles
	wg                   sync.WaitGroup        // tracks in-flight workers for graceful shutdown
	inFlight             sync.Map              // key: issueKey string, value: bool (isPR)
	cloneInFlight        sync.Map              // key: "owner/repo" string, value: *cloneCall; per-repo bare-clone coordination
	baseBranchWarnedSet  sync.Map              // key: "owner/repo#N:branch"; prevents repeated fallback comments for bad base: labels
	events               chan tui.Event        // nil in tests / plain-text mode; TUI goroutine consumes
	logFile              *os.File              // persistent log file at .fabrik/fabrik.log; nil if not opened
	logMu                sync.Mutex            // serializes concurrent writes to logFile
	webhookMgr           *webhookManager       // nil when webhooks are disabled
}

func New(cfg Config) (*Engine, error) {
	// fabrikDir is the directory containing .fabrik/ (stages, plugin, config).
	// Always use the current working directory — Fabrik bare-clones each managed
	// repo to .fabrik/repos/<owner>-<repo>.git and uses worktrees from there.
	fabrikDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolving working directory: %w", err)
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
	claudeWaitDelay = cfg.ClaudeWaitDelay
	worktreeRoot := filepath.Join(fabrikDir, ".fabrik", "worktrees")
	eng := &Engine{
		cfg:                  cfg,
		client:               gh.NewClient(cfg.Token),
		claude:               &RealClaudeInvoker{DebugOutput: cfg.DebugOutput},
		worktreeManagers:     make(map[string]*WorktreeManager),
		fabrikDir:            fabrikDir,
		store:                itemstate.NewStore(nil),
		seenUpdatedAt:        make(map[string]time.Time),
		seededRepos:          make(map[string]bool),
		sem:                  make(chan struct{}, cfg.MaxConcurrent),
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

	// Migrate sessions and logs from ~/.fabrik/ to <cwd>/.fabrik/ (cross-root migration).
	// Must run after migrateSessions so home-dir sessions are already in namespaced layout.
	migrateHomeToProject(fabrikDir, func(msg string) { fmt.Printf("[startup] %s", msg) })

	// Wire the github package's diagnostic logger to engine.logf so retry/
	// degradation warnings (e.g. project board indexer mismatches) reach
	// fabrik.log in both TUI and plain-text modes.
	gh.Logf = eng.logf

	// Initialize the read client: GitHubAdapter (pass-through) unless the in-memory
	// board cache is enabled, in which case CacheImpl is created (bootstrap happens in Run()).
	adapter := boardcache.NewGitHubAdapter(eng.client)
	if cfg.BoardCacheMode == "in-memory" {
		cacheLogFn := func(format string, args ...any) { eng.logf(0, "cache", format, args...) }
		eng.readClient = boardcache.NewCacheImpl(adapter, cacheLogFn)
	} else {
		eng.readClient = adapter
	}

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
		cfg:                  cfg,
		client:               client,
		claude:               claude,
		worktreeManagers:     wms,
		store:                itemstate.NewStore(nil),
		seenUpdatedAt:        make(map[string]time.Time),
		seededRepos:          make(map[string]bool),
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
	// Tests always use pass-through adapter (--board-cache=none behavior).
	eng.readClient = boardcache.NewGitHubAdapter(client)
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

// SetWakeCh configures the wake channel. The TUI sends on this channel to
// reset idle backoff and trigger an immediate poll. Must be called before Run().
func (e *Engine) SetWakeCh(ch chan struct{}) {
	e.wakeCh = ch
}

// SetEvents configures the event channel. Must be called before Run().
// When set, direct stdout writes (pollStatus/pollStatusClear) are suppressed
// because the TUI owns the terminal.
func (e *Engine) SetEvents(ch chan tui.Event) {
	e.events = ch
	tuiMode = ch != nil
	claudeLogf = e.logf
	claudeTUI = ch != nil
	if ch != nil {
		claudeTurnProgress = func(issueNumber, turnsUsed, maxTurns int) {
			e.emit(tui.TurnProgressEvent{
				IssueNumber: issueNumber,
				TurnsUsed:   turnsUsed,
				MaxTurns:    maxTurns,
			})
		}
	} else {
		claudeTurnProgress = nil
	}
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
	} else {
		// Plain-text mode: clear any transient status line before printing.
		pollStatusClear()
		if issueNumber == 0 {
			fmt.Printf("[%s] %s", tag, msg)
		} else {
			fmt.Printf("[#%d %s] %s", issueNumber, tag, msg)
		}
	}
	// Write to persistent log file in both TUI and plain-text modes.
	e.logMu.Lock()
	if e.logFile != nil {
		ts := time.Now().UTC().Format(time.RFC3339)
		if issueNumber == 0 {
			fmt.Fprintf(e.logFile, "%s [%s] %s", ts, tag, msg)
		} else {
			fmt.Fprintf(e.logFile, "%s [#%d %s] %s", ts, issueNumber, tag, msg)
		}
	}
	e.logMu.Unlock()
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
// owns item. On first access it bare-clones the repo to .fabrik/repos/<owner>-<repo>.git.
// Subsequent calls are no-ops (idempotent WM registration). If the clone fails
// it posts a comment, adds fabrik:paused and fabrik:awaiting-input labels,
// records a history entry, and returns ErrSkipItem so the caller skips without
// treating it as a hard error.
//
// Concurrent callers for the same repo are serialized via cloneInFlight: the first
// caller performs the clone while others wait. On failure, only the first caller
// posts the comment/labels; waiters silently return ErrSkipItem.
func (e *Engine) ensureRepoReady(ctx context.Context, item gh.ProjectItem) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if owner == "" || repo == "" {
		return nil // cannot determine repo — let processItem handle it
	}
	nameWithOwner := owner + "/" + repo

	// Fast path: already registered (common case after first clone).
	e.mu.Lock()
	_, registered := e.worktreeManagers[nameWithOwner]
	e.mu.Unlock()
	if registered {
		return nil
	}

	// Singleflight-style coordination: elect one goroutine to perform the clone.
	call := &cloneCall{done: make(chan struct{})}
	actual, loaded := e.cloneInFlight.LoadOrStore(nameWithOwner, call)
	if loaded {
		// Another goroutine is already cloning (or has just cloned) this repo.
		existing := actual.(*cloneCall)
		<-existing.done
		if existing.err != nil {
			e.logf(item.Number, "warn", "bare clone of %s already failed for another worker — skipping\n", nameWithOwner)
			return ErrSkipItem
		}
		// Clone succeeded; register the WM using the winner's bareDir.
		worktreeRoot := filepath.Join(e.fabrikDir, ".fabrik", "worktrees")
		e.registerWorktrees(nameWithOwner, existing.dir, worktreeRoot)
		return nil
	}

	// This goroutine is the owner: perform the clone.
	worktreeRoot := filepath.Join(e.fabrikDir, ".fabrik", "worktrees")
	bareDir, err := ensureBareClone(e.fabrikDir, owner, repo, e.cfg.GitSSH)
	call.dir = bareDir
	call.err = err

	if err != nil {
		// Signal waiters before cleanup so they can read call.err.
		close(call.done)
		// Delete the entry so future poll cycles (after user removes fabrik:paused) can retry.
		e.cloneInFlight.Delete(nameWithOwner)

		msg := fmt.Sprintf("🏭 **Fabrik — cannot clone repo**\n\nFailed to clone `%s/%s`:\n```\n%v\n```\nHuman intervention required. Fix the clone issue and remove `fabrik:paused` to retry.", owner, repo, err)
		if dbID, commentErr := e.client.AddComment(owner, repo, item.Number, msg); commentErr != nil {
			e.logf(item.Number, "warn", "could not post clone-failure comment: %v\n", commentErr)
		} else {
			if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyCommentAdded(boardcache.ItemKey(item.Repo, item.Number), gh.Comment{
					DatabaseID: dbID, Body: msg, Author: e.cfg.User, CreatedAt: time.Now(),
				})
			}
			// no write-through: excluded — AddCommentReaction does not affect dispatch-relevant cache state
			if reactErr := e.client.AddCommentReaction(owner, repo, dbID, "rocket"); reactErr != nil {
				e.logf(item.Number, "warn", "could not add 🚀 to posted comment: %v\n", reactErr)
			}
		}
		if labelErr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:paused"); labelErr != nil {
			e.logf(item.Number, "warn", "could not add fabrik:paused: %v\n", labelErr)
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:paused")
		}
		if labelErr := e.client.AddLabelToIssue(owner, repo, item.Number, "fabrik:awaiting-input"); labelErr != nil {
			e.logf(item.Number, "warn", "could not add fabrik:awaiting-input: %v\n", labelErr)
		} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
			cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), "fabrik:awaiting-input")
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

	// Success: register the WM, then signal waiters.
	// Leave the cloneInFlight entry in place (closed channel, nil err); future callers
	// will exit at the fast-path registered check before reaching cloneInFlight.
	e.registerWorktrees(nameWithOwner, bareDir, worktreeRoot)
	close(call.done)
	return nil
}
