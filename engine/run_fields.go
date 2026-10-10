package engine

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// The Last activity / Last run display fields (#2049, ADR 2049): two more
// display-only ProjectV2 fields on the status-line writer's foundation
// (engine/status_line.go). Same rules — display-only and never read back,
// written only when the value differs from the last one this process wrote
// for the item, silent when the field is absent or of the wrong type, a
// failed write logged and swallowed — and no new state: the only memory is the
// writer's skip-unchanged record, which a restart forgets.
//
// Hooks are call sites, not a subscription to the TUI event channel or the
// InvocationObserver: those carry a phantom Done-cleanup "run" and the board
// column at observation time rather than the stage that ran. The call sites
// have the stage, the project item and the usage in hand and fire exactly
// once per real run. Merge-train batch jobs, synthetic Skipped completions and
// per-turn progress are never hooked. Nothing clears the fields at Done: they
// stay as history.

// noteJobStarted records a job start (stage run or comment review) for
// item: Last activity shows the UTC date of startedAt.
func (e *Engine) noteJobStarted(item gh.ProjectItem, startedAt time.Time) {
	e.writeDisplayField(e.lastActivitySpec(), &e.lastActivity, item, lastActivityDate(startedAt), false)
}

// noteJobFinished records a finished run for item: Last activity shows the UTC
// date of completedAt and Last run the one-line outcome built from the run's
// existing fields. Both writes are skipped when unchanged, so a completion on
// the day the job started writes only Last run. Callers pass the same
// wall-clock source the job's start instant uses (time.Now, as the TUI
// events do), so the two dates can never disagree about the day.
func (e *Engine) noteJobFinished(item gh.ProjectItem, completedAt time.Time, o runOutcome) {
	e.writeDisplayField(e.lastActivitySpec(), &e.lastActivity, item, lastActivityDate(completedAt), false)
	e.writeDisplayField(e.lastRunSpec(), &e.lastRun, item, lastRunLine(o), false)
}
