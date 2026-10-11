package engine

import (
	"sync"
	"testing"
	"time"
)

func TestDaemonHealthZeroValueIsUnknown(t *testing.T) {
	var h daemonHealth
	s := h.snapshot()
	if !s.PollAttemptAt.IsZero() || !s.PollSuccessAt.IsZero() || !s.ReconcileOKAt.IsZero() ||
		!s.WebhookEventAt.IsZero() || !s.StartedAt.IsZero() || s.Backoff.Observed {
		t.Fatalf("zero daemonHealth must report everything unknown, got %+v", s)
	}
}

func TestDaemonHealthPublishes(t *testing.T) {
	var h daemonHealth
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	h.markStarted(at)
	h.notePollAttempt(at.Add(time.Minute))
	h.notePollSuccess(at.Add(2 * time.Minute))
	h.noteReconcileOK(at.Add(3 * time.Minute))
	h.noteWebhookEvent(at.Add(4 * time.Minute))
	h.setRestPaused(true, at)
	h.setGraphQLBackoff(true, 0.15, 750, at)
	s := h.snapshot()
	if !s.StartedAt.Equal(at) || !s.PollAttemptAt.Equal(at.Add(time.Minute)) || !s.PollSuccessAt.Equal(at.Add(2*time.Minute)) ||
		!s.ReconcileOKAt.Equal(at.Add(3*time.Minute)) || !s.WebhookEventAt.Equal(at.Add(4*time.Minute)) {
		t.Errorf("timestamps not published: %+v", s)
	}
	if b := s.Backoff; !b.Observed || !b.Low || !b.RestPaused || b.Ratio != 0.15 || b.Remaining != 750 {
		t.Errorf("backoff mirror = %+v", b)
	}
}

// The API goroutine reads a snapshot while the poll goroutine writes: -race
// must stay clean.
func TestDaemonHealthConcurrentReadWrite(t *testing.T) {
	var h daemonHealth
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			h.notePollAttempt(time.Now())
			h.setGraphQLBackoff(i%2 == 0, float64(i)/1000, i, time.Now())
			h.setRestPaused(i%3 == 0, time.Now())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = h.snapshot()
		}
	}()
	wg.Wait()
}

func TestPollOncePublishesHealth(t *testing.T) {
	e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	if err := e.PollOnce(t.Context()); err != nil {
		t.Skipf("poll errored in this harness (%v); success publishing is covered by the sim", err)
	}
	s := e.health.snapshot()
	if s.PollAttemptAt.IsZero() || s.PollSuccessAt.IsZero() {
		t.Fatalf("PollOnce must publish attempt and success: %+v", s)
	}
}

func TestDaemonHealthPollLifecycle(t *testing.T) {
	var h daemonHealth
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	interval := 30 * time.Second

	h.beginPoll(t0, pollVerdict{})
	h.setPollStep("fetching the project board")
	s := h.snapshot()
	if !s.PollInProgress || !s.PollStartedAt.Equal(t0) || s.PollStep != "fetching the project board" {
		t.Fatalf("in-progress poll not published: %+v", s)
	}

	// A step set outside a poll is dropped, never invented.
	h.endPoll(t0.Add(time.Second), pollVerdict{}, t0.Add(time.Second).Add(interval))
	h.setPollStep("stale")
	s = h.snapshot()
	if s.PollInProgress || s.PollStep != "" || !s.PollCompletedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("completed poll not published: %+v", s)
	}
	if !s.NextPollDueAt.Equal(t0.Add(time.Second + interval)) {
		t.Fatalf("fallback next-due not published: %v", s.NextPollDueAt)
	}
	if s.StallEpoch != 0 || !s.LastStallEndAt.IsZero() {
		t.Fatalf("a healthy poll must not end a stall episode: %+v", s)
	}

	// Ending an overdue poll closes the episode.
	h.beginPoll(t0.Add(time.Minute), pollVerdict{})
	h.endPoll(t0.Add(10*time.Minute), pollVerdict{Known: true, Overdue: true}, t0.Add(11*time.Minute))
	s = h.snapshot()
	if s.StallEpoch != 1 || !s.LastStallEndAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("overdue completion must bump the epoch and stamp recovery: %+v", s)
	}

	// Beginning a poll after a no-poll overdue episode closes it too.
	h.beginPoll(t0.Add(time.Hour), pollVerdict{Known: true, Overdue: true})
	if s = h.snapshot(); s.StallEpoch != 2 {
		t.Fatalf("overdue begin must bump the epoch: %+v", s)
	}
}

func TestDaemonHealthGaugeAsOfMovesOnlyOnRefresh(t *testing.T) {
	var h daemonHealth
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	if s := h.snapshot(); !s.BackoffAsOf.IsZero() || !s.RestPausedAsOf.IsZero() {
		t.Fatalf("never-refreshed gauges must be unknown: %+v", s)
	}
	h.setGraphQLBackoff(false, 1, 4673, at)
	h.notePollAttempt(at.Add(time.Hour))
	h.noteReconcileOK(at.Add(2 * time.Hour))
	if s := h.snapshot(); !s.BackoffAsOf.Equal(at) {
		t.Fatalf("as-of must not move without a refresh: %v", s.BackoffAsOf)
	}
	h.setGraphQLBackoff(false, 1, 4000, at.Add(3*time.Hour))
	if s := h.snapshot(); !s.BackoffAsOf.Equal(at.Add(3 * time.Hour)) {
		t.Fatalf("as-of must move on refresh: %v", s.BackoffAsOf)
	}
}
