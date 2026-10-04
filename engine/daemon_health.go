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

// setRestPaused publishes the REST hard-gate state.
func (h *daemonHealth) setRestPaused(paused bool) {
	h.mu.Lock()
	h.backoffRestPause = paused
	h.mu.Unlock()
}

// setGraphQLBackoff publishes the GraphQL backoff state after a poll read it.
func (h *daemonHealth) setGraphQLBackoff(low bool, ratio float64, remaining int) {
	h.mu.Lock()
	h.backoffObserved = true
	h.backoffLow = low
	h.backoffRatio = ratio
	h.backoffRemaining = remaining
	h.mu.Unlock()
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
	}
}
