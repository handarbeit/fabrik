package engine

import "time"

// Clock supplies the current time. Structurally identical to
// tests/sim/simgh.Clock (Now() time.Time) — deliberately, so a scenario can
// construct one test clock and pass it to both simgh.WithClock and
// Engine.SetClock, keeping GitHub-anchored timing (read via
// FetchLabelAppliedAt) and engine-local timing (itemstate.CooldownAt) in
// lockstep under a single Advance(d) call.
//
// This seam covers exactly the R3 "engine-local" timing group identified in
// #1449's research: the itemstate.CooldownAt stamping/reading call sites.
// Every other named timing gate (the review-wait timeout, the CI settle
// scan, the Done-archive scan) is anchored on a GitHubClient-read timestamp
// via FetchLabelAppliedAt and is already fully controllable from a
// GitHubClient substitute with no engine-side seam — see ADR-1449.
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
