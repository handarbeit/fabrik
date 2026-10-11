package engine

import (
	"sync"
	"time"
)

// daemonHealth holds the freshness timestamps and the rate-limit backoff
// mirror the local read API (#1967) reports. It exists so the API's goroutine
// never reads the poll loop's own single-goroutine fields (lastPollAttemptAt,
// backoff*): those keep their semantics and write sites, and each write site
// additionally publishes a copy here under mu. Every accessor is safe for
// concurrent use; every "never recorded" time is the zero time, which the API
// reports as unknown rather than a healthy default.
type daemonHealth struct {
	mu sync.Mutex

	startedAt        time.Time
	pollAttemptAt    time.Time
	pollSuccessAt    time.Time
	reconcileOKAt    time.Time
	webhookEventAt   time.Time
	backoffObserved  bool
	backoffLow       bool
	backoffRestPause bool
	backoffRatio     float64
	backoffRemaining int

	// Poll liveness (#2124). The poll goroutine publishes; API and channel
	// goroutines read only the snapshot.
	pollInProgress  bool
	pollStartedAt   time.Time
	pollCompletedAt time.Time
	pollStep        string
	nextPollDueAt   time.Time
	backoffReason   string
	// pollPeriod/periodReason are the period the Run() ticker was last reset to
	// (the engine's own effective interval) and why it exceeds the configured one.
	// An errored poll does not reset the ticker, so its next poll comes after
	// this period, not after the configured interval (#2124).
	pollPeriod   time.Duration
	periodReason string
	// stallEpoch counts ended overdue episodes; lastStallEndAt is when the
	// latest one ended.
	stallEpoch     uint64
	lastStallEndAt time.Time
	// backoffAsOf is when the poll last refreshed the GraphQL backoff gauge,
	// restPausedAsOf when it last refreshed the REST pause flag.
	backoffAsOf    time.Time
	restPausedAsOf time.Time
}

// backoffMirror is a copy of the poll loop's backoff state.
type backoffMirror struct {
	Observed   bool
	Low        bool
	RestPaused bool
	Ratio      float64
	Remaining  int
}

// healthSnapshot is a consistent copy of daemonHealth.
type healthSnapshot struct {
	StartedAt      time.Time
	PollAttemptAt  time.Time
	PollSuccessAt  time.Time
	ReconcileOKAt  time.Time
	WebhookEventAt time.Time
	Backoff        backoffMirror

	PollInProgress  bool
	PollStartedAt   time.Time
	PollCompletedAt time.Time
	PollStep        string
	NextPollDueAt   time.Time
	BackoffReason   string
	StallEpoch      uint64
	LastStallEndAt  time.Time
	// BackoffAsOf / RestPausedAsOf are zero until the poll first refreshes them.
	BackoffAsOf    time.Time
	RestPausedAsOf time.Time
}

// markStarted records the process start time. The zero daemonHealth is usable
// (engines built as struct literals in tests never call this); an unset start
// reads as unknown uptime.
func (h *daemonHealth) markStarted(at time.Time) {
	h.mu.Lock()
	h.startedAt = at
	h.mu.Unlock()
}

func (h *daemonHealth) notePollAttempt(at time.Time) {
	h.mu.Lock()
	h.pollAttemptAt = at
	h.mu.Unlock()
}

func (h *daemonHealth) notePollSuccess(at time.Time) {
	h.mu.Lock()
	h.pollSuccessAt = at
	h.mu.Unlock()
}

func (h *daemonHealth) noteReconcileOK(at time.Time) {
	h.mu.Lock()
	h.reconcileOKAt = at
	h.mu.Unlock()
}

func (h *daemonHealth) noteWebhookEvent(at time.Time) {
	h.mu.Lock()
	h.webhookEventAt = at
	h.mu.Unlock()
}

// setRestPaused publishes the REST hard-gate state as refreshed by a poll at.
func (h *daemonHealth) setRestPaused(paused bool, at time.Time) {
	h.mu.Lock()
	h.backoffRestPause = paused
	h.restPausedAsOf = at
	h.mu.Unlock()
}

// setGraphQLBackoff publishes the GraphQL backoff state after a poll read it.
func (h *daemonHealth) setGraphQLBackoff(low bool, ratio float64, remaining int, at time.Time) {
	h.mu.Lock()
	h.backoffObserved = true
	h.backoffAsOf = at
	h.backoffLow = low
	h.backoffRatio = ratio
	h.backoffRemaining = remaining
	h.mu.Unlock()
}

// beginPoll records that a poll started. If the engine was overdue because no
// poll had started, the episode ends here.
func (h *daemonHealth) beginPoll(at time.Time, v pollVerdict) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v.Overdue {
		h.stallEpoch++
		h.lastStallEndAt = at
	}
	h.pollInProgress = true
	h.pollStartedAt = at
	h.pollStep = ""
}

// endPoll records that the poll returned (with or without error). v is the
// verdict computed just before the poll is marked finished, so an overdue
// in-progress episode ends here. fallbackNext is the next-poll-due time to use
// until PollWithBackoff publishes the engine's own.
func (h *daemonHealth) endPoll(at time.Time, v pollVerdict, fallbackNext time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v.Overdue {
		h.stallEpoch++
		h.lastStallEndAt = at
	}
	h.pollInProgress = false
	h.pollCompletedAt = at
	h.pollStep = ""
	h.nextPollDueAt = fallbackNext
	h.backoffReason = h.periodReason
}

// setPollStep records what the in-progress poll is doing; "" when unknown.
func (h *daemonHealth) setPollStep(step string) {
	h.mu.Lock()
	if h.pollInProgress {
		h.pollStep = step
	}
	h.mu.Unlock()
}

// setNextPollDue publishes the engine's own next-poll-due time and the reason
// it is later than the configured interval ("" when it is not).
func (h *daemonHealth) setNextPollDue(at time.Time, reason string) {
	h.mu.Lock()
	h.nextPollDueAt = at
	h.backoffReason = reason
	h.mu.Unlock()
}

// setPollPeriod records the period the poll ticker was reset to, and the
// back-off reason when it is longer than the configured interval. endPoll's
// fallback next-due time uses it, so a poll that errors during a deliberate
// back-off is not mistaken for a missed poll.
func (h *daemonHealth) setPollPeriod(d time.Duration, reason string) {
	h.mu.Lock()
	h.pollPeriod = d
	h.periodReason = reason
	h.mu.Unlock()
}

// pollPeriodSnapshot returns the last recorded ticker period (zero if none).
func (h *daemonHealth) pollPeriodSnapshot() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pollPeriod
}

func (h *daemonHealth) snapshot() healthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return healthSnapshot{
		StartedAt:      h.startedAt,
		PollAttemptAt:  h.pollAttemptAt,
		PollSuccessAt:  h.pollSuccessAt,
		ReconcileOKAt:  h.reconcileOKAt,
		WebhookEventAt: h.webhookEventAt,
		Backoff: backoffMirror{
			Observed:   h.backoffObserved,
			Low:        h.backoffLow,
			RestPaused: h.backoffRestPause,
			Ratio:      h.backoffRatio,
			Remaining:  h.backoffRemaining,
		},
		PollInProgress:  h.pollInProgress,
		PollStartedAt:   h.pollStartedAt,
		PollCompletedAt: h.pollCompletedAt,
		PollStep:        h.pollStep,
		NextPollDueAt:   h.nextPollDueAt,
		BackoffReason:   h.backoffReason,
		StallEpoch:      h.stallEpoch,
		LastStallEndAt:  h.lastStallEndAt,
		BackoffAsOf:     h.backoffAsOf,
		RestPausedAsOf:  h.restPausedAsOf,
	}
}
