package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/warnings"
)

// The forge-unreachable hold (#2393, ADR 2393). When GitHub calls have failed
// on transport errors for a sustained, unbroken run, the engine starts no new
// model work: with the forge down a worker's results cannot be recorded, so the
// next poll sees an item that looks untouched and dispatches it again (#2388:
// ~150 runs of one Research stage in ~21 hours). Running workers are left
// alone. The hold lifts the moment any GitHub call gets an answer — a success
// or a 4xx, either proves the forge is reachable. In memory only.

const (
	// forgeHoldMinFailures and forgeHoldWindow are the conservative engine
	// defaults for "sustained": a run of at least this many consecutive
	// transport failures spanning at least this long on e.now(). A single slow
	// call, or a burst shorter than the window, never engages the hold.
	forgeHoldMinFailures = 5
	forgeHoldWindow      = 2 * time.Minute
	// forgeHoldRunLapse ends a failure run that has seen no further failure for
	// this long, so old failures cannot be completed by one new one.
	forgeHoldRunLapse = 10 * time.Minute

	forgeWarningType    = "forge_unreachable"
	forgeWarningSubject = "github"
)

// errForgeHeldNotAttempted marks work that was not started because the forge
// hold is engaged. Like errDrainingNotAttempted it is uncharged: trainCancelled
// reads it as "not attempted".
var errForgeHeldNotAttempted = errors.New("forge unreachable: new model work is held")

// forgeHealth tracks the current unbroken run of transport failures.
type forgeHealth struct {
	mu          sync.Mutex
	runStart    time.Time // first failure of the current run; zero = no run
	lastFailAt  time.Time
	failures    int
	lastErr     string
	held        bool
	heldSince   time.Time
	disabled    bool // test seam: neutralise the hold
	minFailures int  // test override; 0 = default
	window      time.Duration
}

// SetForgeHoldDisabledForTest neutralises the forge hold (R3) for this engine.
func (e *Engine) SetForgeHoldDisabledForTest(disabled bool) {
	e.forge.mu.Lock()
	e.forge.disabled = disabled
	e.forge.mu.Unlock()
}

// SetForgeHoldThresholdsForTest overrides the engagement thresholds (0 = default).
func (e *Engine) SetForgeHoldThresholdsForTest(minFailures int, window time.Duration) {
	e.forge.mu.Lock()
	e.forge.minFailures, e.forge.window = minFailures, window
	e.forge.mu.Unlock()
}

// ObserveForgeResult feeds one GitHub call outcome (nil = success) to the
// tracker. It is the result observer of the production *github.Client and of
// the sim's fault table. Safe for concurrent use.
func (e *Engine) ObserveForgeResult(err error) {
	transport := gh.IsTransportError(err)
	now := e.now()

	f := &e.forge
	f.mu.Lock()
	if f.disabled {
		f.mu.Unlock()
		return
	}
	if !transport {
		f.runStart, f.lastFailAt, f.failures, f.lastErr = time.Time{}, time.Time{}, 0, ""
		wasHeld, since := f.held, f.heldSince
		f.held, f.heldSince = false, time.Time{}
		f.mu.Unlock()
		if wasHeld {
			e.onForgeHoldLifted(since, now)
		}
		return
	}

	if f.runStart.IsZero() || now.Sub(f.lastFailAt) > forgeHoldRunLapse {
		f.runStart, f.failures = now, 0
	}
	f.lastFailAt = now
	f.failures++
	f.lastErr = err.Error()
	minFailures, window := forgeHoldMinFailures, forgeHoldWindow
	if f.minFailures > 0 {
		minFailures = f.minFailures
	}
	if f.window > 0 {
		window = f.window
	}
	engaged := false
	if !f.held && f.failures >= minFailures && now.Sub(f.runStart) >= window {
		f.held, f.heldSince = true, now
		engaged = true
	}
	held, since, failures, lastErr := f.held, f.heldSince, f.failures, f.lastErr
	f.mu.Unlock()

	if engaged {
		e.onForgeHoldEngaged(since, failures, lastErr)
	} else if held {
		e.health.setForgeHold(since, failures, lastErr)
	}
}

// forgeHeld reports whether new model work is currently held back.
func (e *Engine) forgeHeld() bool {
	e.forge.mu.Lock()
	defer e.forge.mu.Unlock()
	return !e.forge.disabled && e.forge.held
}

func (e *Engine) onForgeHoldEngaged(since time.Time, failures int, lastErr string) {
	e.health.setForgeHold(since, failures, lastErr)
	e.logf(0, "forge-hold", "GitHub has been unreachable for %s (%d consecutive transport failures, last: %s); holding all new model work until a GitHub call succeeds — running workers are not interrupted",
		forgeHoldWindow, failures, lastErr)
	if err := warnings.Report(e.warnWriter("forge-hold"), warnings.Condition{
		Type:    forgeWarningType,
		Subject: forgeWarningSubject,
		Title:   "GitHub is unreachable; new work is on hold",
		Detail: fmt.Sprintf("GitHub calls have failed on transport errors since %s (%d consecutive, last: %s). "+
			"Fabrik starts no new model work until a GitHub call succeeds again; running workers are left alone.",
			since.UTC().Format(time.RFC3339), failures, lastErr),
		Fingerprint: forgeWarningType,
	}); err != nil {
		e.logf(0, "forge-hold", "could not record the warning: %v", err)
	}
	e.channelNudge() // observation only (#1968): re-check account-wide state
}

func (e *Engine) onForgeHoldLifted(since, now time.Time) {
	e.health.clearForgeHold()
	e.logf(0, "forge-hold", "GitHub answered again after %s; resuming new model work", now.Sub(since).Round(time.Second))
	if err := warnings.Resolve(warnings.Condition{Type: forgeWarningType, Subject: forgeWarningSubject}.Key()); err != nil {
		e.logf(0, "forge-hold", "could not resolve the warning: %v", err)
	}
	e.channelNudge()
}
