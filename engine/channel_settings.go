package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// channelTestSettings is the per-Engine home of every channel test seam (#2210,
// ADR 2210). They used to be process-globals, which is why no channel sim test
// could be t.Parallel: one test's neutralisation or timing leaked into every
// engine in the binary. The zero value means "production behaviour", so no
// constructor needs to know about it, and the production cost of a seam is one
// atomic load. Fields are atomics because the consumer goroutine and the poll
// goroutine read them while a test may still be setting up.
type channelTestSettings struct {
	// Durations in nanoseconds; 0 keeps the production default.
	debounce     atomic.Int64
	tick         atomic.Int64
	readyTimeout atomic.Int64
	situationMin atomic.Int64
	anchorGrace  atomic.Int64

	// Neutralisations: each switches one behaviour off so a non-vacuity test can
	// show its positive assertion fails without it.
	downtimeOff        atomic.Bool
	anomalyRetryOff    atomic.Bool
	anomalyWedgeOff    atomic.Bool
	anomalyDivOff      atomic.Bool
	decisionPendingOff atomic.Bool
	withdrawalOff      atomic.Bool
	settleCauseOff     atomic.Bool
	situationDedupOff  atomic.Bool
	reviewReinvokeOff  atomic.Bool
}

// Production defaults. Nothing mutates them. The debounce and the ticker are
// real-time mechanisms (wall clock, not the e.now() seam): a debounce that
// followed a frozen test clock would never elapse.
const (
	defaultChannelDebounce     = 750 * time.Millisecond
	defaultChannelTick         = 30 * time.Second
	defaultChannelReadyTimeout = 3 * time.Minute
	defaultDecisionAnchorGrace = 3 * time.Minute
)

// channelPrune is the arrival/hub prune period. It has no seam.
const channelPrune = time.Hour

func durOr(v int64, def time.Duration) time.Duration {
	if v > 0 {
		return time.Duration(v)
	}
	return def
}

func (e *Engine) chanDebounce() time.Duration {
	return durOr(e.chanTest.debounce.Load(), defaultChannelDebounce)
}
func (e *Engine) chanTick() time.Duration {
	return durOr(e.chanTest.tick.Load(), defaultChannelTick)
}
func (e *Engine) chanReadyTimeout() time.Duration {
	return durOr(e.chanTest.readyTimeout.Load(), defaultChannelReadyTimeout)
}
// decisionAnchorGrace is stored as nanoseconds+1 so that an explicit zero grace
// (announce at once) is distinguishable from "unset".
func (e *Engine) decisionAnchorGrace() time.Duration {
	if v := e.chanTest.anchorGrace.Load(); v > 0 {
		return time.Duration(v - 1)
	}
	return defaultDecisionAnchorGrace
}

// CopyChannelTestSettingsFrom copies every channel test seam from old, so an
// engine rebuilt by a restart helper keeps the timing and neutralisations the
// test configured on its predecessor.
func (e *Engine) CopyChannelTestSettingsFrom(old *Engine) {
	if old == nil || old == e {
		return
	}
	s, o := &e.chanTest, &old.chanTest
	s.debounce.Store(o.debounce.Load())
	s.tick.Store(o.tick.Load())
	s.readyTimeout.Store(o.readyTimeout.Load())
	s.situationMin.Store(o.situationMin.Load())
	s.anchorGrace.Store(o.anchorGrace.Load())
	s.downtimeOff.Store(o.downtimeOff.Load())
	s.anomalyRetryOff.Store(o.anomalyRetryOff.Load())
	s.anomalyWedgeOff.Store(o.anomalyWedgeOff.Load())
	s.anomalyDivOff.Store(o.anomalyDivOff.Load())
	s.decisionPendingOff.Store(o.decisionPendingOff.Load())
	s.withdrawalOff.Store(o.withdrawalOff.Load())
	s.settleCauseOff.Store(o.settleCauseOff.Load())
	s.situationDedupOff.Store(o.situationDedupOff.Load())
	s.reviewReinvokeOff.Store(o.reviewReinvokeOff.Load())
}

// idle reports whether the consumer has nothing left to do: no queued events,
// no dirty (debounced) item, no nudge, no settle candidate and no pass in
// flight. Guarded by ce.mu.
func (ce *channelEvents) idle() bool {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	return len(ce.queue) == 0 && len(ce.dirty) == 0 && !ce.nudge &&
		len(ce.settleCand) == 0 && !ce.busy
}

// forceDue makes every debounced item due now, so a drain wait need not sit out
// the debounce, and wakes the consumer.
func (ce *channelEvents) forceDue() {
	ce.mu.Lock()
	now := time.Now()
	for _, d := range ce.dirty {
		d.due = now
	}
	ce.mu.Unlock()
	ce.signal()
}

// DrainChannelEventsForTest blocks until the channel consumer has processed
// everything the engine has enqueued so far, and everything that processing
// itself enqueued (#2210). Debounced re-evaluations are made due at once rather
// than waited out. It returns an error naming the stage when timeout elapses,
// and nil at once when no hub is running. It is the deterministic replacement
// for a fixed sleep: a poll's observers run synchronously inside Apply, so once
// PollOnce has returned, every change it produced is already enqueued.
//
// It does not cover time-driven work (cadence ticks, the decision anchor grace):
// those need real time to pass and have no drain point.
func (e *Engine) DrainChannelEventsForTest(timeout time.Duration) error {
	ce := e.channelEvents()
	if ce == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		ce.forceDue()
		if ce.idle() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("channel consumer did not drain within %s", timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// ChannelDeliverableForTest reports whether the hub still owes the named
// attached subscriber a deliverable batch (see channelevents.Hub.Deliverable).
func (e *Engine) ChannelDeliverableForTest(name string) bool {
	ce := e.channelEvents()
	return ce != nil && ce.hub.Deliverable(name)
}
