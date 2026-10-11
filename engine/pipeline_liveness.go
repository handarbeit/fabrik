package engine

import (
	"fmt"
	"time"
)

// pipelineOverdueFactor is K (#2124): the poll loop is overdue once a poll has
// been in progress for longer than K × the configured poll interval, or once no
// poll has started within K × that interval of the time the engine itself said
// the next poll was due. It is a single documented constant, not an operator
// setting. The merge-train partition stall threshold reuses it.
const pipelineOverdueFactor = 2

// Poll overdue causes, reported in health and on the pipeline-stalled event.
const (
	pollCauseInProgress = "poll-in-progress"
	pollCauseNoPoll     = "no-poll-started"
)

// Backoff reasons published with the next-poll-due time.
const (
	backoffReasonIdle    = "idle"
	backoffReasonGraphQL = "graphql"
	backoffReasonREST    = "rest"
)

// pollVerdictInput is everything pollVerdict needs. Zero times mean unknown.
type pollVerdictInput struct {
	Now time.Time
	// Interval is the configured poll interval; zero means unknown.
	Interval time.Duration
	// DaemonStartedAt is the baseline before any poll has run.
	DaemonStartedAt time.Time
	InProgress      bool
	PollStartedAt   time.Time
	// NextDueAt is when the engine itself said the next poll was due (it
	// includes idle, GraphQL and REST back-off); zero before the first poll.
	NextDueAt time.Time
}

// pollVerdict is the overdue verdict.
type pollVerdict struct {
	// Known is false when the verdict cannot be computed (unknown interval or
	// baseline). An unknown verdict is never reported as healthy or overdue.
	Known        bool
	Overdue      bool
	Cause        string
	Blocked      time.Duration
	OverdueAfter time.Duration
}

// computePollVerdict computes the overdue verdict. It is a pure function.
func computePollVerdict(in pollVerdictInput) pollVerdict {
	if in.Interval <= 0 || in.Now.IsZero() {
		return pollVerdict{}
	}
	after := pipelineOverdueFactor * in.Interval
	v := pollVerdict{OverdueAfter: after}
	if in.InProgress {
		if in.PollStartedAt.IsZero() {
			return pollVerdict{}
		}
		v.Known = true
		v.Blocked = in.Now.Sub(in.PollStartedAt)
		if v.Blocked > after {
			v.Overdue, v.Cause = true, pollCauseInProgress
		}
		return v
	}
	baseline := in.NextDueAt
	if baseline.IsZero() {
		baseline = in.DaemonStartedAt
	}
	if baseline.IsZero() {
		return pollVerdict{}
	}
	v.Known = true
	if d := in.Now.Sub(baseline); d > 0 {
		v.Blocked = d
	}
	if in.Now.Sub(baseline) > after {
		v.Overdue, v.Cause = true, pollCauseNoPoll
	}
	return v
}

// pollVerdictFor computes the verdict from a health snapshot.
func (e *Engine) pollVerdictFor(h healthSnapshot, now time.Time) pollVerdict {
	return computePollVerdict(pollVerdictInput{
		Now:             now,
		Interval:        e.pollInterval(),
		DaemonStartedAt: h.StartedAt,
		InProgress:      h.PollInProgress,
		PollStartedAt:   h.PollStartedAt,
		NextDueAt:       h.NextPollDueAt,
	})
}

// pollInterval is the configured poll interval; zero when unset (test engines).
func (e *Engine) pollInterval() time.Duration {
	return time.Duration(e.cfg.PollSeconds) * time.Second
}

// pollStallDescription renders a poll stall for event content.
func pollStallDescription(v pollVerdict, step string) string {
	s := fmt.Sprintf("the poll loop is overdue (%s, blocked for %d s, overdue after %d s)",
		v.Cause, int64(v.Blocked.Seconds()), int64(v.OverdueAfter.Seconds()))
	if step != "" {
		s += ", in step " + step
	}
	return s
}

// setPollStep records the phase the in-progress poll is in (#2124). It is a
// name only and is published through the health mirror; "" means unknown.
func (e *Engine) setPollStep(step string) { e.health.setPollStep(step) }
