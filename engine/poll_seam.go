package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/pollctl"
)

// pollSeam is the bed-only poll hold/trigger seam (#1978, ADR-1978). TEST-ONLY:
// it is enabled solely by Config.PollControlFile (FABRIK_TEST_POLL_CONTROL),
// which the gate runner and the live harness set on the e2e bed and nothing
// else does. With it unset Engine.pollSeam is nil, every hook below is skipped
// by a nil check, and the engine starts no goroutine, touches no file and logs
// nothing extra — the poll loop is byte-identical to a build without the seam.
//
// Unlike the sim seam (Engine.PollOnce, ADR-1449), which bypasses Run(), this
// one gates the real Run() loop: it exists so a live test whose subject is a
// state window ("the engine sees exactly 7 Queued members at once") can stop
// the engine taking a decision until the window is built, then let it take
// exactly one.
//
// Hold means the whole poll. Every path that starts poll(), or the
// poll-equivalent reconcile work, goes through ordinary/runUnlessHeld, which
// drop the cycle while held: the startup poll, the ticker, wakeCh wakes (and
// the GraphQL-recovery self-wake, which only ever sends on wakeCh) and
// reconcileLoop. A dropped cycle is dropped, not queued — a state window must
// not move. Hold gates new cycles only: item workers and merge-train workers
// dispatched by an earlier poll keep running.
//
// Protocol: two single-writer files (internal/pollctl). The harness writes the
// request (hold, hold_until, trigger_seq); a watcher goroutine, started only
// when the seam is enabled, applies it and writes the ack (held, applied hold
// generation, done trigger sequence, outcome).
type pollSeam struct {
	e    *Engine
	path string

	// watchInterval is the watcher's cadence. A var, not a const, so tests can
	// shrink it.
	watchInterval time.Duration
	// nowFn reports unix time for hold_until; overridable by tests.
	nowFn func() time.Time

	// pollMu serializes poll execution: an ordinary poll, a triggered poll and
	// the application of a hold all take it, so two polls never overlap and a
	// hold is only acknowledged once no poll is running. reconMu does the same
	// for reconcile ticks. When both are needed, take pollMu first.
	pollMu  sync.Mutex
	reconMu sync.Mutex
	// held is written only with both pollMu and reconMu held (so applying a hold
	// waits for in-flight work), and read atomically anywhere.
	held atomic.Bool

	// cycle is Run()'s pollCycle closure: the exact code path an ordinary poll
	// takes (PollWithBackoff plus the ticker reset), so a triggered poll has the
	// same backoff and rate-limit bookkeeping.
	cycle func() (PollBackoffResult, error)

	// Watcher-goroutine-only state.
	appliedGen int64
	doneSeq    int64
	ack        pollctl.Ack
}

const (
	pollSeamWatchInterval = 200 * time.Millisecond
	// pollSeamFloorRetries bounds how often a triggered poll that hit the
	// minimum-poll-interval floor is retried before being reported blocked.
	pollSeamFloorRetries = 3
)

func newPollSeam(e *Engine, path string) *pollSeam {
	return &pollSeam{e: e, path: path, watchInterval: pollSeamWatchInterval, nowFn: time.Now}
}

// start clears any stale request/ack (a hold never survives an engine restart:
// the engine always comes up released), writes a fresh released ack — whose
// existence tells the harness the seam is live on this bed, and that any request
// it writes from now on will be seen — and launches the watcher.
func (s *pollSeam) start(ctx context.Context, cycle func() (PollBackoffResult, error)) {
	s.cycle = cycle
	if err := pollctl.Clear(s.path); err != nil {
		s.e.logf(0, "pollseam", "could not clear stale control files: %v\n", err)
	}
	s.writeAck()
	s.e.logf(0, "pollseam", "TEST-ONLY poll hold/trigger seam enabled (control file %s)\n", s.path)
	go s.watch(ctx)
}

// ordinary runs an automatic poll cycle (startup, ticker, wake) unless polls
// are held, in which case it is dropped without polling and without resetting
// the ticker.
func (s *pollSeam) ordinary(cycle func() (PollBackoffResult, error)) error {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if s.held.Load() {
		s.e.logfThrottled("poll-seam-held", 0, "pollseam", "poll held (test-only seam) — dropping automatic poll\n")
		return nil
	}
	_, err := cycle()
	return err
}

// runUnlessHeld runs a reconcile tick unless polls are held, in which case the
// tick is dropped.
func (s *pollSeam) runUnlessHeld(fn func()) {
	s.reconMu.Lock()
	defer s.reconMu.Unlock()
	if s.held.Load() {
		s.e.logfThrottled("poll-seam-held-reconcile", 0, "pollseam", "reconcile held (test-only seam) — dropping tick\n")
		return
	}
	fn()
}

func (s *pollSeam) setHeld(v bool) {
	s.pollMu.Lock()
	s.reconMu.Lock()
	s.held.Store(v)
	s.reconMu.Unlock()
	s.pollMu.Unlock()
}

func (s *pollSeam) watch(ctx context.Context) {
	t := time.NewTicker(s.watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.step(ctx)
		}
	}
}

// step applies one observation of the request file. Exposed to the package's
// tests so they can drive the seam without the watcher's timing.
func (s *pollSeam) step(ctx context.Context) {
	req := pollctl.ReadRequest(s.path)
	now := s.nowFn()
	live := req.Hold && (req.HoldUntil == 0 || now.Unix() < req.HoldUntil)

	// Apply a new hold generation (also acknowledges a release).
	if req.HoldGen > s.appliedGen {
		s.setHeld(live)
		s.appliedGen = req.HoldGen
		s.writeAck()
	} else if s.held.Load() && !live && req.Hold {
		// Deadline passed: self-release so a dead harness cannot leave the bed held.
		s.setHeld(false)
		s.e.logf(0, "pollseam", "hold deadline passed — released\n")
		s.writeAck()
	}

	if req.TriggerSeq > s.doneSeq {
		outcome, detail := s.runTriggered(ctx)
		s.doneSeq = req.TriggerSeq
		s.ack.Outcome, s.ack.Detail = outcome, detail
		s.writeAck()
	}
}

func (s *pollSeam) writeAck() {
	s.ack.Held = s.held.Load()
	s.ack.HoldGen = s.appliedGen
	s.ack.DoneSeq = s.doneSeq
	if err := pollctl.WriteAck(s.path, s.ack); err != nil {
		s.e.logf(0, "pollseam", "could not write ack: %v\n", err)
	}
}

// runTriggered runs one complete poll — Run()'s own pollCycle, under pollMu so
// it never overlaps an ordinary poll — and classifies how it ended. A call that
// hit the minimum-poll-interval floor ran no poll, so it is retried a bounded
// number of times; anything else that ran no poll is reported blocked, never
// success, so the harness cannot assert on a non-poll.
func (s *pollSeam) runTriggered(ctx context.Context) (string, string) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return pollctl.OutcomeError, "engine shutting down"
		}
		res, err := s.cycle()
		switch {
		case err != nil:
			return pollctl.OutcomeError, err.Error()
		case res.Ran:
			return pollctl.OutcomeRan, ""
		case res.NextInterval <= minPollInterval && attempt < pollSeamFloorRetries:
			select {
			case <-ctx.Done():
			case <-time.After(res.NextInterval + 10*time.Millisecond):
			}
		default:
			return pollctl.OutcomeBlocked, "poll not run (rate-limit gate or minimum-poll-interval floor)"
		}
	}
}
