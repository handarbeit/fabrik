package engine

import (
	"fmt"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
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
	retryCount         map[string]int       // key: "<issueNum>-<stageName>", value: failed attempt count
	pausedDueToRetries map[string]bool      // key: "<issueNum>-<stageName>", true if engine paused this issue
	idleCount          int                  // consecutive idle polls; triggers self-upgrade at threshold
	sem                chan struct{}        // semaphore bounding concurrent workers across poll cycles
	wg                 sync.WaitGroup       // tracks in-flight workers for graceful shutdown
	inFlight           sync.Map             // key: issue number (int), value: struct{}
}

func New(cfg Config) (*Engine, error) {
	// Resolve git repo root (works even if launched from a subdirectory)
	repoDir, err := gitToplevel()
	if err != nil {
		return nil, fmt.Errorf("resolving git repo root: %w", err)
	}
	return &Engine{
		cfg:                cfg,
		client:             gh.NewClient(cfg.Token),
		claude:             &RealClaudeInvoker{},
		worktrees:          NewWorktreeManager(repoDir),
		processedSet:       make(map[string]time.Time),
		lockedIssues:       make(map[int]bool),
		retryCount:         make(map[string]int),
		pausedDueToRetries: make(map[string]bool),
		sem:                make(chan struct{}, cfg.MaxConcurrent),
	}, nil
}

// NewWithDeps creates an Engine with explicit dependencies (for testing).
func NewWithDeps(cfg Config, client GitHubClient, claude ClaudeInvoker, worktrees *WorktreeManager) *Engine {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Engine{
		cfg:                cfg,
		client:             client,
		claude:             claude,
		worktrees:          worktrees,
		processedSet:       make(map[string]time.Time),
		lockedIssues:       make(map[int]bool),
		retryCount:         make(map[string]int),
		pausedDueToRetries: make(map[string]bool),
		sem:                make(chan struct{}, maxConcurrent),
	}
}

func logf(issueNumber int, tag, format string, args ...any) {
	prefix := fmt.Sprintf("[#%d %s] ", issueNumber, tag)
	fmt.Printf(prefix+format, args...)
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
