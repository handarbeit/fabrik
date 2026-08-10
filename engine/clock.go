package engine

import "time"

// Clock supplies the current time. Structurally identical to
// tests/sim/simgh.Clock (Now() time.Time) — deliberately, so a scenario can
// construct one test clock and pass it to both simgh.WithClock and
// Engine.SetClock, keeping GitHub-anchored timing (read via
// FetchLabelAppliedAt) and engine-local timing (itemstate.CooldownAt) in
// lockstep under a single Advance(d) call.
//
// This seam covers the R3 "engine-local" timing group identified in #1449's
// research: the itemstate.CooldownAt stamping/reading call sites, plus one
// call site research did not anticipate — recordLabelAppliedAtNow's
// in-memory write-through cache (engine/mutate.go, #1314). Every named
// timing gate (the review-wait timeout, the CI settle scan, the Done-archive
// scan) is conceptually anchored on a GitHubClient-read timestamp via
// FetchLabelAppliedAt, but #1314 added a same-process cache that
// labelAppliedAt prefers over a live fetch whenever present — so a scenario
// backdating FetchLabelAppliedAt's response has no effect until the cache
// entry (recorded by the engine itself, at the moment it first applies the
// label) also honors the injected clock instead of real time.Now(). Found
// empirically while building #1449's review-wait-timeout scenario: the
// mutation-log evidence showed the timeout evaluating "not yet elapsed"
// indefinitely despite a simgh timestamp backdated by 24 hours, because the
// cache had already recorded a fresh real time.Now() first. See
// ADR-1449.
type Clock interface {
	Now() time.Time
}

// realClock is the default Clock, reading wall time. Used whenever no clock
// has been injected via SetClock — byte-identical to the pre-seam
// time.Now()-based behavior at every call site this seam touches.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// SetClock substitutes the clock e.now() reads from. A test seam: production
// (New) never calls this, so e.clock stays nil and now() falls back to
// time.Now() unconditionally. Not part of NewWithDeps's signature — this is
// a separate, optional setter so the many existing NewWithDeps call sites
// are unaffected.
func (e *Engine) SetClock(c Clock) {
	e.clock = c
}

// now returns the current time, from the injected clock if one was set via
// SetClock, otherwise from time.Now(). Every itemstate.CooldownAt
// stamp/read in this package must go through this method rather than calling
// time.Now() directly, so a test clock can control cooldown timing
// deterministically.
func (e *Engine) now() time.Time {
	if e.clock != nil {
		return e.clock.Now()
	}
	return time.Now()
}
