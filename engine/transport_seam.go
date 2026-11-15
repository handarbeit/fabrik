package engine

import (
	"context"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// Test seam for the transport-health-dependent cooldown scheduling (#2532, ADR
// 2532). The sim bed has no transport layer, so tests/sim cannot reach
// transportSet through the engine's own fakeTransport (an engine _test.go type).
// StubTransport is an exported ingestTransport whose strict health, covered repos
// and subscriptions a test sets directly; events are delivered through the
// existing SubmitDeliveryForTest. Tests only; per Engine, nothing package-level.

// StubTransport is a scriptable ingestTransport.
type StubTransport struct {
	mu        sync.Mutex
	healthy   bool
	repos     map[string]bool // nil/empty = every repo
	events    map[string]bool // nil = every event
	reconnect func()
}

// NewStubTransport returns a stub that is strictly healthy, covers every repo and
// subscribes to every event.
func NewStubTransport() *StubTransport { return &StubTransport{healthy: true} }

// SetHealthy sets the stub's strict health.
func (s *StubTransport) SetHealthy(h bool) {
	s.mu.Lock()
	s.healthy = h
	s.mu.Unlock()
}

// SetRepos restricts the repos the stub covers (none given = every repo).
func (s *StubTransport) SetRepos(repos ...string) {
	s.mu.Lock()
	s.repos = nil
	if len(repos) > 0 {
		s.repos = map[string]bool{}
		for _, r := range repos {
			s.repos[r] = true
		}
	}
	s.mu.Unlock()
}

// SetSubscribed restricts the events the stub receives (none given = every event).
func (s *StubTransport) SetSubscribed(events ...string) {
	s.mu.Lock()
	s.events = nil
	if len(events) > 0 {
		s.events = map[string]bool{}
		for _, ev := range events {
			s.events[ev] = true
		}
	}
	s.mu.Unlock()
}

func (s *StubTransport) Name() string                  { return "stub" }
func (s *StubTransport) Start(context.Context) error   { return nil }
func (s *StubTransport) Stop()                         {}
func (s *StubTransport) IsDisabled() bool              { return false }
func (s *StubTransport) UpdateRepos(map[string]bool)   {}
func (s *StubTransport) TransitionHealth(bool, string) {}
func (s *StubTransport) CheckCoverage()                {}
func (s *StubTransport) SetSink(func(delivery))        {}
func (s *StubTransport) SetReconnectHandler(fn func()) { s.mu.Lock(); s.reconnect = fn; s.mu.Unlock() }
func (s *StubTransport) IsHealthyOrStartingUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}
func (s *StubTransport) SubscribesTo(ev string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events == nil || s.events[ev]
}
func (s *StubTransport) HealthRow() localapi.TransportHealth {
	state := "healthy"
	if !s.IsHealthyOrStartingUp() {
		state = "unhealthy"
	}
	return localapi.TransportHealth{Name: "stub", State: state}
}

// HealthyFor implements ingestTransport.
func (s *StubTransport) HealthyFor(repo, eventType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.healthy {
		return false
	}
	if repo != "" && len(s.repos) > 0 && !s.repos[repo] {
		return false
	}
	return eventType == "" || s.events == nil || s.events[eventType]
}

var _ ingestTransport = (*StubTransport)(nil)

// AttachStubTransportForTest attaches t to this engine's transport set, creating
// the set when the engine has none. Call it before Run or the first poll.
func (e *Engine) AttachStubTransportForTest(t *StubTransport) {
	if e.transports == nil {
		e.transports = &transportSet{}
	}
	e.transports.Add(t)
}

// HealthWatchTickForTest runs one health-watcher tick with prev as the
// previous snapshot and returns the new one (tests of the FR-007 revert).
func (e *Engine) HealthWatchTickForTest(prev map[string]bool) map[string]bool {
	return e.healthWatchTick(prev)
}

// EarliestLongCooldownForTest returns the earliest long admission cooldown the idle
// cap would count right now (zero if none), as PollWithBackoff reads it.
func (e *Engine) EarliestLongCooldownForTest() time.Time { return e.earliestLongCooldown(e.now()) }

// EarliestTimerDeadlineForTest returns the earliest unexpired deadline in the timer
// registry (zero if none), as PollWithBackoff reads it, and the engine's clock.
func (e *Engine) EarliestTimerDeadlineForTest() (due, now time.Time) {
	now = e.now()
	return e.earliestTimerDeadline(now), now
}
