package tui

import "time"

// RateLimitStats holds the minimal GraphQL rate limit data needed by the TUI.
type RateLimitStats struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// Event is the interface implemented by all typed engine events.
type Event interface {
	tuiEvent()
}

// LogEvent carries a single log line emitted by the engine.
type LogEvent struct {
	IssueNumber int
	Tag         string
	Message     string
}

func (LogEvent) tuiEvent() {}

// PollStartedEvent is emitted at the beginning of each poll cycle.
type PollStartedEvent struct {
	Owner   string
	Repo    string
	Project int
}

func (PollStartedEvent) tuiEvent() {}

// PollCompletedEvent is emitted when a poll cycle finishes dispatching.
type PollCompletedEvent struct {
	ItemCount         int
	Dispatched        int
	GraphQLStats      RateLimitStats
	EffectiveInterval time.Duration
}

func (PollCompletedEvent) tuiEvent() {}

// JobStartedEvent is emitted when a worker goroutine begins processing an item.
type JobStartedEvent struct {
	IssueNumber int
	Repo        string // "owner/repo" — empty for single-repo projects
	Title       string
	StageName   string
	IsComment   bool // true when processing a user comment, not a stage run
	StartedAt   time.Time
}

func (JobStartedEvent) tuiEvent() {}

// JobCompletedEvent is emitted when a worker goroutine finishes.
type JobCompletedEvent struct {
	IssueNumber    int
	Repo           string // "owner/repo" — empty for single-repo projects
	Title          string
	StageName      string
	StageModel     string // model configured for the stage (e.g. "sonnet")
	IsComment      bool   // true when processing a user comment, not a stage run
	Success        bool   // no error from processItem
	Completed      bool   // stage actually completed (FABRIK_STAGE_COMPLETE detected)
	BlockedOnInput bool   // stage needs user input (FABRIK_BLOCKED_ON_INPUT detected)
	Duration       time.Duration
	CompletedAt    time.Time
	TurnsUsed      int
	MaxTurns       int
	CostUSD        float64
}

func (JobCompletedEvent) tuiEvent() {}

// IssueBlockedEvent is emitted when an issue is held at the dependency gate.
// It is emitted each time checkDependencies fires for a blocked issue.
type IssueBlockedEvent struct {
	IssueNumber int
	Repo        string // "owner/repo" — empty for single-repo projects
	Title       string
	StageName   string
	WaitingFor  []string // e.g. ["#214", "owner/repo#215"]
}

func (IssueBlockedEvent) tuiEvent() {}

// TickEvent is emitted once per second by the TUI loop to drive timer updates.
type TickEvent struct {
	At time.Time
}

func (TickEvent) tuiEvent() {}

// ProjectMetaEvent is emitted once after the startup board fetch succeeds.
// It delivers the project board title and URL to the TUI for display in the footer.
type ProjectMetaEvent struct {
	BoardTitle string // display name of the GitHub Project board
	BoardURL   string // https://github.com/{orgs|users}/{owner}/projects/{num}
}

func (ProjectMetaEvent) tuiEvent() {}

// TurnProgressEvent is emitted after each user event (logical turn start) during a
// Claude invocation. It carries the current live turn count and effective budget
// so the TUI can display a real-time turn counter for in-progress stages.
type TurnProgressEvent struct {
	IssueNumber int
	TurnsUsed   int
	MaxTurns    int // 0 means unlimited
}

func (TurnProgressEvent) tuiEvent() {}
