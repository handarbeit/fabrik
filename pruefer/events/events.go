// Package events defines the transport-agnostic boundary between a webhook
// source and Pruefer's event-driven review dispatch: the normalized
// GitHubEvent type, the EventSource/EventSink interfaces, and shared
// per-webhook mechanics (signature verification, delivery-ID dedupe) that
// any GitHub-webhook-forwarding transport can reuse. No transport-specific
// concept (e.g. Hookdeck) may appear here or leak into pruefer's
// review/domain code — see adrs/1254-*.md.
package events

import (
	"context"
	"encoding/json"
	"time"
)

// GitHubEvent is the normalized representation of a single GitHub webhook
// delivery, produced by an EventSource from whatever wire protocol it
// speaks. It carries just enough to identify the affected resource and
// route it — never enough to reconstruct authoritative PR state; callers
// must re-fetch that from GitHub.
type GitHubEvent struct {
	// DeliveryID is GitHub's X-GitHub-Delivery header value, used for
	// at-least-once-delivery dedupe.
	DeliveryID string
	// EventType is GitHub's X-GitHub-Event header value, e.g. "pull_request"
	// or "installation_repositories".
	EventType string
	Owner     string
	Repo      string
	// ResourceID identifies the affected resource within Owner/Repo — e.g.
	// a PR number rendered as a string. Empty for events with no single
	// affected resource (e.g. an installation/repo-selection change).
	ResourceID string
	// Action is GitHub's payload "action" field, e.g. "opened", "synchronize",
	// "ready_for_review".
	Action string
	// Installation is the GitHub App installation ID the event was
	// delivered for, letting a client be selected via the existing
	// per-owner installation-token routing (#1233). Zero when the source
	// could not determine it.
	Installation int64
	ReceivedAt   time.Time
	// Payload is the raw webhook body, retained for handlers that need
	// fields beyond what was parsed into the fields above.
	Payload json.RawMessage
}

// HealthState describes an EventSource's current transport connectivity.
type HealthState string

const (
	HealthConnected    HealthState = "connected"
	HealthDisconnected HealthState = "disconnected"
	HealthReconnecting HealthState = "reconnecting"
)

// HealthEvent reports a transition in an EventSource's transport health.
// Callers (e.g. the daemon) use reconnect transitions to trigger a
// reconciliation poll, since events may have been missed while disconnected.
type HealthEvent struct {
	State HealthState
	At    time.Time
	// Err is non-empty when State reflects a failure (Disconnected or
	// Reconnecting following an error); empty on a clean Connected transition.
	Err string
}

// EventSink receives normalized events from an EventSource. Handle must not
// run a review synchronously — implementations hand off to an existing
// concurrency-capped dispatch mechanism and return promptly, so the source
// can ack the webhook without delay. Handle must be safe to call
// concurrently and must tolerate at-least-once, possibly out-of-order,
// possibly duplicate delivery.
type EventSink interface {
	Handle(ctx context.Context, event GitHubEvent)
}

// EventSource is the boundary between a webhook transport and Pruefer's
// event-driven review dispatch. Run blocks until ctx is cancelled, or until
// a genuinely fatal, non-recoverable error occurs; transient transport
// failures (auth, connect, read) are the source's own responsibility to
// retry with backoff internally, never surfaced as a Run error — an
// EventSource must never crash the daemon on transport outage.
type EventSource interface {
	Run(ctx context.Context, sink EventSink) error
}
