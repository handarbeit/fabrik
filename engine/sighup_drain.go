package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// drainState is the two-phase SIGHUP restart state (#2259, ADR 2259).
//
// The first SIGHUP begins a drain: every dispatch site is gated (isDraining),
// the main loop stops polling and waits for the in-flight set to empty
// (awaitDrain), and nothing is cancelled — workers run to their own end. A
// second SIGHUP cancels the run context, which hands over to drainAndExit's
// bounded wait and the re-exec. The zero value is usable (engines built as
// struct literals in tests never construct it).
type drainState struct {
	mu        sync.Mutex
	began     chan struct{} // closed when the drain begins; lazily created
	draining  atomic.Bool
	startedAt time.Time

	// Test seams (per-Engine, zero value = production).
	gateDisabled  atomic.Bool           // SetDrainDispatchGateDisabledForTest: dispatch sites ignore the drain
	cancelOnFirst atomic.Bool           // SetSighupDrainDisabledForTest: the pre-#2259 first-SIGHUP cancel
	logInterval   atomic.Int64          // SetDrainLogIntervalForTest, nanoseconds; 0 = drainLogInterval
	pollEvery     atomic.Int64          // SetDrainPollIntervalForTest, nanoseconds; 0 = drainPollInterval
	runCtxHook    func(context.Context) // SetRunCtxHookForTest: receives Run()'s root context
}

// errDrainingNotAttempted is returned by resolveConflictWithClaude while the
// daemon drains for a SIGHUP restart (#2259). Like a cancelled slot wait it means
// "could not attempt": trainCancelled reports it, so no member is ejected, no
// trial is charged and no batch is dissolved.
var errDrainingNotAttempted = errors.New("daemon draining for restart: conflict resolution not attempted")

const (
	// drainLogInterval is how often the in-flight set is logged while draining.
	drainLogInterval = time.Minute
	// drainPollInterval is the safety-net cadence of awaitDrain's emptiness check;
	// the store wake normally gets there first.
	drainPollInterval = 250 * time.Millisecond
)

// drainBegan returns the channel closed when the drain begins.
func (d *drainState) drainBegan() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.began == nil {
		d.began = make(chan struct{})
	}
	return d.began
}

// isDraining reports whether a dispatch site must refuse to start work.
func (e *Engine) isDraining() bool {
	return e.drain.draining.Load() && !e.drain.gateDisabled.Load()
}

// drainGateCh is the channel a queued worker waits on to abandon itself when a
// drain begins; nil (never ready) when the dispatch-gate test seam is on.
func (e *Engine) drainGateCh() <-chan struct{} {
	if e.drain.gateDisabled.Load() {
		return nil
	}
	return e.drain.drainBegan()
}

// drainActive reports the drain flag itself, ignoring the dispatch-gate test seam.
func (e *Engine) drainActive() bool { return e.drain.draining.Load() }

// beginDrain starts the drain (first SIGHUP). It reports false if one had begun.
func (e *Engine) beginDrain() bool {
	d := &e.drain
	d.mu.Lock()
	if d.began == nil {
		d.began = make(chan struct{})
	}
	select {
	case <-d.began:
		d.mu.Unlock()
		return false
	default:
	}
	d.startedAt = e.now()
	d.draining.Store(true)
	close(d.began)
	d.mu.Unlock()
	return true
}

// drainWorker is one entry of the in-flight set.
type drainWorker struct {
	Label   string // "#N" or "merge-train <key>"
	Stage   string
	Kind    string
	Elapsed time.Duration
	HasTime bool
}

// drainWorkers is the in-flight set: item workers (queued ones included, they
// are abandoned at once), merge-train conflict resolutions and merge-train
// worker/step goroutines. A train run merely waiting on CI is in none of these,
// so it never holds the restart (FR-005). Sorted for stable output.
func (e *Engine) drainWorkers() []drainWorker {
	now := e.now()
	var out []drainWorker
	e.store.Scan(func(it *itemstate.ItemState) {
		switch {
		case it.Worker != nil:
			kind := it.Worker.Kind
			if kind == "" {
				kind = itemstate.WorkerKindStageRun
			}
			out = append(out, drainWorker{
				Label: fmt.Sprintf("#%d", it.Number), Stage: it.Worker.StageName, Kind: kind,
				Elapsed: now.Sub(it.Worker.StartedAt), HasTime: !it.Worker.StartedAt.IsZero(),
			})
		case it.ConflictResolution != nil:
			out = append(out, drainWorker{
				Label: fmt.Sprintf("#%d", it.Number), Kind: itemstate.WorkerKindTrainConflict,
				Elapsed: now.Sub(it.ConflictResolution.Since), HasTime: !it.ConflictResolution.Since.IsZero(),
			})
		}
	})
	for _, k := range e.store.RepoWorkerKeys() {
		out = append(out, drainWorker{Label: "merge-train " + k, Kind: "merge-train"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// formatDrainWorkers renders the in-flight set for the log (issue, stage, kind, elapsed).
func formatDrainWorkers(ws []drainWorker) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		s := w.Label
		if w.Stage != "" {
			s += " " + w.Stage
		}
		if w.Kind != "" {
			s += " " + w.Kind
		}
		if w.HasTime {
			s += " " + w.Elapsed.Round(time.Second).String()
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// logDrainInFlight logs the in-flight set once.
func (e *Engine) logDrainInFlight() {
	ws := e.drainWorkers()
	if len(ws) == 0 {
		e.logf(0, "signal", "draining for restart: no workers in flight\n")
		return
	}
	e.logf(0, "signal", "draining for restart: %d worker(s) in flight: %s\n", len(ws), formatDrainWorkers(ws))
}

// runDrainLog repeats the in-flight line every interval until the set is empty
// or stop is closed.
func (e *Engine) runDrainLog(stop <-chan struct{}) {
	iv := time.Duration(e.drain.logInterval.Load())
	if iv <= 0 {
		iv = drainLogInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if len(e.drainWorkers()) == 0 {
				return
			}
			e.logDrainInFlight()
		}
	}
}

// onFirstSighup is the first-SIGHUP action: begin the drain, say so, and start the
// minute repeat. It cancels nothing. stop ends the repeater (closed with the run).
func (e *Engine) onFirstSighup(stop <-chan struct{}) {
	e.sighupRequested.Store(true)
	if !e.beginDrain() {
		return
	}
	e.logf(0, "signal", "received SIGHUP — draining in-flight workers before restarting\n")
	e.logDrainInFlight()
	e.logf(0, "signal", "send SIGHUP again to restart now\n")
	e.publishDrainStatus()
	go e.runDrainLog(stop)
}

// awaitDrain blocks until the in-flight set is empty or done fires (the second
// SIGHUP or SIGINT/SIGTERM cancelled the run). It reads the store rather than
// calling e.wg.Wait(), which must never race an Add from zero (ADR-1393). It
// checks emptiness before its first wait, so an idle SIGHUP restarts at once.
func (e *Engine) awaitDrain(done <-chan struct{}) {
	iv := time.Duration(e.drain.pollEvery.Load())
	if iv <= 0 {
		iv = drainPollInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		ws := e.drainWorkers()
		e.publishDrainStatus()
		if len(ws) == 0 {
			return
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

// publishDrainStatus mirrors the drain into the health snapshot.
func (e *Engine) publishDrainStatus() {
	if !e.drainActive() {
		return
	}
	e.health.setDraining(e.drain.startedAt, len(e.drainWorkers()))
}

// SetSighupDrainDisabledForTest restores the pre-#2259 first-SIGHUP behaviour
// (cancel immediately) so a test can show it fails without the drain.
func (e *Engine) SetSighupDrainDisabledForTest(disabled bool) { e.drain.cancelOnFirst.Store(disabled) }

// SetDrainDispatchGateDisabledForTest makes the dispatch sites ignore the drain.
func (e *Engine) SetDrainDispatchGateDisabledForTest(disabled bool) {
	e.drain.gateDisabled.Store(disabled)
}

// SetDrainLogIntervalForTest overrides the in-flight log repeat interval.
func (e *Engine) SetDrainLogIntervalForTest(d time.Duration) { e.drain.logInterval.Store(int64(d)) }

// SetDrainPollIntervalForTest overrides awaitDrain's emptiness-check cadence.
func (e *Engine) SetDrainPollIntervalForTest(d time.Duration) { e.drain.pollEvery.Store(int64(d)) }

// SetRunCtxHookForTest registers fn to receive Run()'s root context, from which
// every worker context derives, so a test can show the first SIGHUP leaves it
// uncancelled. Call before Run().
func (e *Engine) SetRunCtxHookForTest(fn func(context.Context)) { e.drain.runCtxHook = fn }
