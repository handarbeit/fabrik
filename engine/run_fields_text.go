package engine

import (
	"fmt"
	"strings"
	"time"
)

// Wording for the display-only Last activity / Last run fields (#2049,
// ADR 2049), kept apart from the writer like status_line_text.go. Pure
// functions of values the engine already holds for the TUI and log — nothing
// here reads a clock, the store or the board.

// runOutcome is the subset of a finished invocation's existing fields that
// the Last run line is built from. It mirrors tui.JobCompletedEvent's outcome
// fields (Success is carried inverted as Errored, as itemstate stores it).
type runOutcome struct {
	StageName   string
	IsComment   bool
	Completed   bool
	TurnLimited bool
	Blocked     bool
	Errored     bool
	TurnsUsed   int
	MaxTurns    int
	Duration    time.Duration
}

// lastActivityDate is the Last activity value for an event instant: its UTC
// calendar date (YYYY-MM-DD), so it never depends on the host's time zone. A
// ProjectV2 date field has no time of day.
func lastActivityDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// runOutcomeWord names how a run ended. Precedence: a turn-limit exit, then a
// run waiting on input, then a completed stage (a completion marker wins over
// a non-zero exit, as everywhere else in the engine), then a genuine fault;
// anything else — a clean exit without the marker, a tools-denied run — is
// "incomplete".
func runOutcomeWord(o runOutcome) string {
	switch {
	case o.TurnLimited:
		return "turn-limited"
	case o.Blocked:
		return "blocked on input"
	case o.Completed:
		return "completed"
	case o.Errored:
		return "failed"
	default:
		return "incomplete"
	}
}

// lastRunLine builds the one-line Last run summary:
//
//	<Stage>[ · comment review] · <outcome>[ · turns][ · duration]
//
// Turns are shown for completed and turn-limited runs when any were used
// ("42/250 turns", or "42 turns" when the stage has no budget); the duration
// only for a completed run. The line is capped like the status line.
func lastRunLine(o runOutcome) string {
	stage := o.StageName
	if stage == "" {
		stage = "Run"
	}
	parts := []string{stage}
	if o.IsComment {
		parts = append(parts, "comment review")
	}
	word := runOutcomeWord(o)
	parts = append(parts, word)

	if (word == "completed" || word == "turn-limited") && o.TurnsUsed > 0 {
		if o.MaxTurns > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d turns", o.TurnsUsed, o.MaxTurns))
		} else {
			parts = append(parts, fmt.Sprintf("%d turns", o.TurnsUsed))
		}
	}
	if word == "completed" && o.Duration > 0 {
		parts = append(parts, formatRunDuration(o.Duration))
	}
	return truncateStatusLine(strings.Join(parts, " · "))
}

// formatRunDuration renders a run's wall time compactly: "45s" under a
// minute, "18m" under an hour (rounded to the nearest minute), else "1h5m".
func formatRunDuration(d time.Duration) string {
	if secs := d.Round(time.Second); secs < time.Minute {
		return fmt.Sprintf("%ds", int(secs/time.Second))
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
