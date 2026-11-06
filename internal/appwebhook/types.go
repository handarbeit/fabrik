package appwebhook

import "github.com/handarbeit/fabrik/warnings"

// Delivery is one verified App webhook delivery handed to the consumer's Sink.
// ReceivedAt and any ordering concern belong to the consumer.
type Delivery struct {
	// ID is the X-GitHub-Delivery GUID.
	ID        string
	EventType string
	Payload   []byte
	// Transport is the receiver's name ("direct", "tailscale"), for logs.
	Transport string
}

// Sink receives an accepted, verified delivery. The consumer owns dedupe,
// ordering and attribution behind it.
type Sink func(Delivery)

// LogFunc is the consumer's logger: issueNumber 0 for non-item lines.
type LogFunc func(issueNumber int, tag, format string, args ...any)

// Panel is the consumer's operator-warnings reporter. Implementations log (not
// fail) on a recording error, as the engine's always has.
type Panel interface {
	Report(c warnings.Condition)
	Resolve(key string)
	// Reconcile sweeps one panel type to exactly conds.
	Reconcile(typ string, conds []warnings.Condition)
}

// HealthSink receives the receiver's current state for display. State is the
// derived health ("healthy"/"unhealthy"); counts are per-event-type delivery
// counts; coverageNote is the installation-coverage note ("" = none).
type HealthSink func(state string, counts map[string]int, coverageNote string)

// HealthState is the derived receiver health.
type HealthState string

const (
	StateHealthy   HealthState = "healthy"
	StateUnhealthy HealthState = "unhealthy"
)

// MaxBodyBytes caps a request body (25 MiB). A larger body is answered 413
// before any signature verification.
const MaxBodyBytes = 25 << 20
