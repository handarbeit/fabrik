package engine

import (
	"fmt"
	"sync"
	"time"

	gh "github.com/verveguy/fabrik/github"
	"github.com/verveguy/fabrik/stages"
	"github.com/verveguy/fabrik/tui"
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
	worktrees          *WorktreeManager
	mu                 sync.Mutex
	processedSet       map[string]time.Time // track what we've processed: "issue#-commentID" -> timestamp
	lockedIssues       map[int]bool         // issues that have had fabrik:locked added and not yet released
	totalTokens        TokenUsage           // accumulated token usage since process start
	lastReportedCost   float64              // cost at last [stats] report; skip repeat prints when unchanged
	retryCount         map[string]int       // key: "<issueNum>-<stageName>", value: failed attempt count
	pausedDueToRetries map[string]bool      // key: "<issueNum>-<stageName>", true if engine paused this issue
	lastUpdatedAt      map[int]time.Time    // tracks last-seen updatedAt per issue number
	idleCount          int                  // consecutive idle polls; triggers self-upgrade at threshold
	sem                chan struct{}        // semaphore bounding concurrent workers across poll cycles
	wg                 sync.WaitGroup       // tracks in-flight workers for graceful shutdown
	inFlight           sync.Map             // key: issue number (int), value: struct{}
	events             chan tui.Event       // nil in tests / plain-text mode; TUI goroutine consumes
}

func New(cfg Config) (*Engine, error) {
	// Resolve git repo root (works even if launched from a subdirectory)
	repoDir, err := gitToplevel()
	if err != nil {
		return nil, fmt.Errorf("resolving git repo root: %w", err)
	}
	wm := NewWorktreeManager(repoDir)
	eng := &Engine{
		cfg:                cfg,
		client:             gh.NewClient(cfg.Token),
		claude:             &RealClaudeInvoker{},
		worktrees:          wm,
		processedSet:       make(map[string]time.Time),
		lockedIssues:       make(map[int]bool),
		lastUpdatedAt:      make(map[int]time.Time),
		retryCount:         make(map[string]int),
		pausedDueToRetries: make(map[string]bool),
		sem:                make(chan struct{}, cfg.MaxConcurrent),
	}
	wm.logfFn = eng.logf
	return eng, nil
}

// NewWithDeps creates an Engine with explicit dependencies (for testing).
func NewWithDeps(cfg Config, client GitHubClient, claude ClaudeInvoker, worktrees *WorktreeManager) *Engine {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	eng := &Engine{
		cfg:                cfg,
		client:             client,
		claude:             claude,
		worktrees:          worktrees,
		processedSet:       make(map[string]time.Time),
		lockedIssues:       make(map[int]bool),
		lastUpdatedAt:      make(map[int]time.Time),
		retryCount:         make(map[string]int),
		pausedDueToRetries: make(map[string]bool),
		sem:                make(chan struct{}, maxConcurrent),
	}
	if worktrees != nil {
		worktrees.logfFn = eng.logf
	}
	return eng
}

// SetEvents configures the event channel. Must be called before Run().
func (e *Engine) SetEvents(ch chan tui.Event) {
	e.events = ch
	if e.worktrees != nil {
		e.worktrees.logfFn = e.logf
	}
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
