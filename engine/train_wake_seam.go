package engine

import "time"

// Test seams of #2542 (ADR 2542). Per-Engine, so sim tests can run in parallel.

// SetOpenTrialCapDisabledForTest neutralises (or restores) the idle-interval cap
// applied while a merge-train run has an open trial.
func (e *Engine) SetOpenTrialCapDisabledForTest(off bool) { e.openTrialCapOff.Store(off) }

// SetTrialWakeDisabledForTest neutralises (or restores) the early-poll wake for
// an open trial's PR deliveries.
func (e *Engine) SetTrialWakeDisabledForTest(off bool) { e.trialWakeOff.Store(off) }

// SetIdleStartForTest sets when consecutive idle polls began. idleStart is real
// wall time (time.Since), so a sim test cannot otherwise produce a backed-off
// interval. Call it between polls, never concurrently with one.
func (e *Engine) SetIdleStartForTest(t time.Time) { e.idleStart = t }
