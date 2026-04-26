// Package watch implements the fabrik watch TUI for monitoring a single issue.
package watch

// LogLineMsg carries a single rendered line of Claude output from the log follower.
type LogLineMsg struct {
	Text string
}

// NewLogFileMsg is sent when the log follower detects a new .log file in the
// issue's log directory, indicating a stage transition.
type NewLogFileMsg struct {
	Path string
}

// GitHubPollMsg is sent by the GitHub poll ticker to trigger a refresh of
// issue metadata, PR status, CI check runs, and comment count.
type GitHubPollMsg struct{}

// TickMsg is the generic ticker message used for periodic UI refreshes
// (e.g. elapsed-time display).
type TickMsg struct{}

// ClaudeFinishedMsg is sent when the inline Claude subprocess launched via
// tea.ExecProcess exits. Err is non-nil if Claude exited with an error.
type ClaudeFinishedMsg struct {
	Err error
}

// StatusMsgMsg carries a transient status message to display in the status bar.
type StatusMsgMsg struct {
	Text string
}

// TurnCountMsg is sent by the log follower each time it detects a user event
// (logical turn start) in the live NDJSON stream, carrying the cumulative per-invocation count.
type TurnCountMsg struct {
	TurnsUsed int
}
