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
	Skipped        bool // synthetic fallback emit (deferred at emission site); InvocationObserver is authoritative (Skipped:false)
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

// WebhookStatusEvent is emitted when the webhook stream health state changes
// or when new events are received. State is one of: "starting_up", "healthy",
// "unhealthy", or "" (webhook mode disabled).
type WebhookStatusEvent struct {
	State       string         // WebhookHealthState as string to avoid import cycle
	EventCounts map[string]int // per-event-type received counts
}

func (WebhookStatusEvent) tuiEvent() {}

// StageChangedEvent is emitted when an issue moves to a new project-board stage.
// It allows the TUI to reactively update the displayed stage for an active item
// without waiting for the next full poll cycle.
type StageChangedEvent struct {
	Repo     string // "owner/repo" — empty for single-repo projects
	Number   int
	Title    string
	NewStage string // the new board column / stage name
}

func (StageChangedEvent) tuiEvent() {}

// SkillsStaleEvent is emitted once after startup when the on-disk plugin skill
// files differ from the embedded versions. Count is the number of diffing files;
// zero means skills are up to date (used to clear the header badge after upgrade).
type SkillsStaleEvent struct {
	Count int
}

func (SkillsStaleEvent) tuiEvent() {}

// CustomWorkflowEvent is emitted when the three-way plugin comparison determines
// that the operator has local customizations in .fabrik/plugin/ that differ from
// the last recorded installed-version. This state is mutually exclusive with
// SkillsStaleEvent (customWorkflow takes priority over skillsStaleCount).
type CustomWorkflowEvent struct{}

func (CustomWorkflowEvent) tuiEvent() {}

// RateLimitAlertEvent is emitted by the engine when the GraphQL rate-limit state
// transitions: Exhausted=true when a probe failure occurs while quota is low or
// zero; Exhausted=false when quota recovers above rateLimitHealthyThreshold.
// Reset is the time at which the quota is expected to reset (zero if unknown).
type RateLimitAlertEvent struct {
	Exhausted bool
	Reset     time.Time
}

func (RateLimitAlertEvent) tuiEvent() {}
