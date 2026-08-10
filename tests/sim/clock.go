package sim

import (
	"sync"
	"time"
)

// Clock is a mutex-guarded, manually-advanced time source. It satisfies both
// engine.Clock and tests/sim/simgh.Clock (both are the single-method
// "Now() time.Time" shape), so one instance can be handed to both
// engine.Engine.SetClock and simgh.WithClock — keeping GitHub-anchored
// timing (label-applied-at, read via FetchLabelAppliedAt) and engine-local
// timing (itemstate.CooldownAt) in lockstep under a single Advance call
// (R3, ADR-1449).
//
// Guarded by a mutex because Now is read from every engine worker goroutine
// concurrently with a scenario's own Advance call — an unguarded time.Time
// field would be a data race the moment a scenario advances the clock with a
// poll's dispatched workers in flight, which is the ordinary case for a
// harness driving PollOnce with MaxConcurrent > 1. Mirrors
// tests/sim/simgh's own fakeClock (helpers_test.go).
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock constructs a Clock starting at t.
func NewClock(t time.Time) *Clock {
	return &Clock{t: t}
}

// Now implements engine.Clock and simgh.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d (or backward, for a negative d).
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set moves the clock to an absolute instant.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}
