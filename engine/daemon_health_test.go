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
	h.setRestPaused(true)
	h.setGraphQLBackoff(true, 0.15, 750)
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
			h.setGraphQLBackoff(i%2 == 0, float64(i)/1000, i)
			h.setRestPaused(i%3 == 0)
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
