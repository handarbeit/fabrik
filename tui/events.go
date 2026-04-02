package tui

import "time"

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
	ItemCount  int
	Dispatched int
}

func (PollCompletedEvent) tuiEvent() {}

// JobStartedEvent is emitted when a worker goroutine begins processing an item.
type JobStartedEvent struct {
	IssueNumber int
	StageName   string
	StartedAt   time.Time
}

func (JobStartedEvent) tuiEvent() {}

// JobCompletedEvent is emitted when a worker goroutine finishes.
type JobCompletedEvent struct {
	IssueNumber int
	StageName   string
	Success     bool
	Duration    time.Duration
	CompletedAt time.Time
}

func (JobCompletedEvent) tuiEvent() {}

// TickEvent is emitted once per second by the TUI loop to drive timer updates.
type TickEvent struct {
	At time.Time
}

func (TickEvent) tuiEvent() {}
