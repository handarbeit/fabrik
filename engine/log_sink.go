package engine

// LogSink receives the plain-text log lines of one Engine in place of the
// process-wide stdout (#2247). issueNumber 0 is a poll- or repo-level line.
type LogSink func(issueNumber int, tag, msg string)

// SetLogSinkForTest routes this Engine's plain-text log lines to sink instead of
// stdout; nil restores stdout. It is a per-Engine field, not a package global, so
// parallel sim tests can each attribute their own output to themselves. It does
// not affect the TUI event channel or the persistent log file, and an Engine
// with no sink prints exactly what it always did.
func (e *Engine) SetLogSinkForTest(sink LogSink) {
	if sink == nil {
		e.logSink.Store(nil)
		return
	}
	e.logSink.Store(&sink)
}
