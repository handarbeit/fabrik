package engine

import (
	"context"
	"sync"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// ingestTransport is the surface every webhook delivery source implements
// (#2351, ADR 2351; amends ADR 1142 Decision #3). The existing gh-webhook-forward
// manager (*webhookManager), the Hookdeck manager (*hookdeckManager) and any
// future source (the Funnel receiver, #2143) plug into the engine through it, so
// health transitions, reconcile coverage, repo updates and reconnect catch-up
// work for any transport with no concrete-type switch anywhere.
//
// A transport's job ends at "verify my own signature, normalise, and hand the
// delivery to the sink". Dedupe, ordering, acknowledgement timing and the echo
// registry are engine-level and shared (intake.go, echo_registry.go).
type ingestTransport interface {
	// Name identifies the transport in logs.
	Name() string
	// Start launches the transport. A non-nil error means it could not start;
	// the engine carries on without it.
	Start(ctx context.Context) error
	// Stop shuts the transport down.
	Stop()
	// IsHealthyOrStartingUp reports whether the transport's connectivity should
	// count as healthy (or still within its startup grace) for idle-cap purposes.
	IsHealthyOrStartingUp() bool
	// IsDisabled reports that the transport has permanently disabled itself for
	// this run (e.g. a circuit breaker).
	IsDisabled() bool
	// UpdateRepos is called after each board poll with the managed-repo set.
	UpdateRepos(repos map[string]bool)
	// TransitionHealth applies a reconcile-derived health signal: healthy when
	// the light reconcile found no drift (or repaired it), unhealthy on drift. A
	// transport with independently tracked conditions (Hookdeck's signature
	// drift) must not let a healthy hint mask them.
	TransitionHealth(healthy bool, reason string)
	// CheckCoverage runs the transport's periodic best-effort coverage
	// re-check (a deleted hook, a changed installation). A transport with
	// nothing to check does nothing.
	CheckCoverage()
	// SubscribesTo reports whether the transport receives eventType, for the
	// conditional echo reservation of events a transport may have dropped.
	SubscribesTo(eventType string) bool
	// SetSink installs the engine's delivery sink. Called once before Start.
	SetSink(sink func(delivery))
	// SetReconnectHandler installs the callback the transport calls whenever it
	// RE-establishes its connection after a disconnect (never on first connect),
	// so the engine can run a catch-up reconcile for events missed meanwhile.
	SetReconnectHandler(fn func())
	// HealthRow reports the transport's own state, last delivery time and
	// rejection counts for fabrik_health (#2142). Anything it cannot report is
	// unknown. Required of every transport, so a new one cannot silently report
	// nothing.
	HealthRow() localapi.TransportHealth
}

// transportSet is the engine's collection of attached transports. Every method
// is safe on a nil *transportSet (no transport: poll-only), and with exactly one
// transport each aggregate equals that transport's own value, so single-transport
// behaviour is unchanged.
type transportSet struct {
	mu sync.RWMutex
	ts []ingestTransport
	// failed holds transports whose Start returned an error. They are not
	// attached (the daemon is poll-only for them) and take no part in Empty,
	// Mode, Healthy or StopAll; HealthRows alone reads them, so a failed
	// listener still shows as a row (state "not listening") instead of
	// vanishing from fabrik_health.
	failed []ingestTransport
}

func (s *transportSet) snapshot() []ingestTransport {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ingestTransport(nil), s.ts...)
}

// Add attaches a started transport.
func (s *transportSet) Add(t ingestTransport) {
	s.mu.Lock()
	s.ts = append(s.ts, t)
	s.mu.Unlock()
}

// AddFailed records a transport that could not start, for HealthRows only.
func (s *transportSet) AddFailed(t ingestTransport) {
	s.mu.Lock()
	s.failed = append(s.failed, t)
	s.mu.Unlock()
}

func (s *transportSet) failedSnapshot() []ingestTransport {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ingestTransport(nil), s.failed...)
}

// Empty reports that no transport is attached.
func (s *transportSet) Empty() bool { return len(s.snapshot()) == 0 }

// StopAll stops every transport.
func (s *transportSet) StopAll() {
	for _, t := range s.snapshot() {
		t.Stop()
	}
}

// Healthy reports whether any transport is healthy or starting up — the signal
// that widens the idle poll cap.
func (s *transportSet) Healthy() bool {
	for _, t := range s.snapshot() {
		if t.IsHealthyOrStartingUp() {
			return true
		}
	}
	return false
}

// Disabled reports that transports are attached and every one has disabled itself.
func (s *transportSet) Disabled() bool {
	ts := s.snapshot()
	if len(ts) == 0 {
		return false
	}
	for _, t := range ts {
		if !t.IsDisabled() {
			return false
		}
	}
	return true
}

// Mode names the aggregate state for the local API: off, disabled, healthy or
// unhealthy.
func (s *transportSet) Mode() string {
	switch {
	case s.Empty():
		return "off"
	case s.Disabled():
		return "disabled"
	case s.Healthy():
		return "healthy"
	default:
		return "unhealthy"
	}
}

// UpdateRepos forwards the managed-repo set to every transport.
func (s *transportSet) UpdateRepos(repos map[string]bool) {
	for _, t := range s.snapshot() {
		t.UpdateRepos(repos)
	}
}

// TransitionHealth applies a reconcile-derived health signal to every transport.
func (s *transportSet) TransitionHealth(healthy bool, reason string) {
	for _, t := range s.snapshot() {
		t.TransitionHealth(healthy, reason)
	}
}

// HealthRows returns one health row per attached transport, followed by one per
// transport that failed to start, never nil.
func (s *transportSet) HealthRows() []localapi.TransportHealth {
	rows := []localapi.TransportHealth{}
	for _, t := range s.snapshot() {
		rows = append(rows, t.HealthRow())
	}
	for _, t := range s.failedSnapshot() {
		rows = append(rows, t.HealthRow())
	}
	return rows
}

// CheckCoverage runs every transport's coverage re-check.
func (s *transportSet) CheckCoverage() {
	for _, t := range s.snapshot() {
		t.CheckCoverage()
	}
}

// SubscribesTo reports whether any attached transport receives eventType.
func (s *transportSet) SubscribesTo(eventType string) bool {
	for _, t := range s.snapshot() {
		if t.SubscribesTo(eventType) {
			return true
		}
	}
	return false
}

var (
	_ ingestTransport = (*webhookManager)(nil)
	_ ingestTransport = (*hookdeckManager)(nil)
)
