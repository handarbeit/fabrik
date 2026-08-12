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
// There is deliberately no separate "disconnected but not yet retrying"
// state: EventSource.Run's contract is to retry every transient failure
// internally and never return except on ctx cancellation, so a connection
// loss and the start of its retry are the same transition — Reconnecting.
type HealthState string

const (
	HealthConnected    HealthState = "connected"
	HealthReconnecting HealthState = "reconnecting"
)

// HealthEvent reports a transition in an EventSource's transport health.
// Callers (e.g. the daemon) use reconnect transitions to trigger a
// reconciliation poll, since events may have been missed while disconnected.
type HealthEvent struct {
	State HealthState
	At    time.Time
	// Err is non-empty when State reflects a failure (a Reconnecting
	// transition following an error); empty on a clean Connected transition.
	Err string
}

// DropReason classifies why one delivery was dropped instead of reaching
// ReviewPR — see ADR-1563. Every value here already existed as a distinct
// code branch with its own logf call before that issue (Research found nine
// such branches across source.go and eventsink.go); DropReason turns that
// existing classification into something a caller can count and report,
// instead of it terminating at a rate-limited or unthrottled log line.
//
// The values split into two origins: the first six are raised by
// hookdeck.Source (a malformed outer frame, a malformed attempt body, a
// missing or invalid GitHub signature, a normalization failure, or a
// delivery-ID dedupe hit) and reach a caller via hookdeck.Config.OnDrop,
// since hookdeck cannot import pruefer to call back in directly. The last
// four are raised by Daemon.ReviewFromEvent (pruefer/eventsink.go) — an
// unrecognized owner, a repo outside watched_repos, a PR already under
// review, or a PR no longer open — and are recorded in-package via
// Daemon.recordDrop, with no callback indirection needed.
type DropReason string

const (
	// DropSignatureMissing means the forwarded request carried no
	// X-Hub-Signature-256 header at all — a Hookdeck-side forwarding
	// problem (or source misconfiguration), not a wrong secret.
	DropSignatureMissing DropReason = "signature_missing"
	// DropSignatureInvalid means a X-Hub-Signature-256 header was present
	// but did not verify against the configured webhook secret — either a
	// misconfigured secret or a change in the byte-exact body Hookdeck
	// forwards (see adrs/1254-*.md's byte-exactness dependency).
	DropSignatureInvalid DropReason = "signature_invalid"
	// DropMalformedEnvelope means the outer WebSocket frame itself failed
	// to unmarshal as JSON — protocol drift in the transport, or a
	// corrupting intermediary.
	DropMalformedEnvelope DropReason = "malformed_envelope"
	// DropMalformedAttempt means the frame's inner "attempt" body failed to
	// unmarshal into the expected shape.
	DropMalformedAttempt DropReason = "malformed_attempt"
	// DropMalformedPayload means the forwarded webhook body itself failed
	// to normalize into a GitHubEvent — a malformed body, or an unexpected
	// content-type.
	DropMalformedPayload DropReason = "malformed_payload"
	// DropDedupe means this delivery ID was already seen — an expected,
	// benign at-least-once-delivery redundancy, not a failure.
	DropDedupe DropReason = "dedupe"
	// DropUnwatchedOwner means no GitHubLister client is configured for the
	// event's owner.
	DropUnwatchedOwner DropReason = "unwatched_owner"
	// DropUnwatchedRepo means the event's owner/repo is not in
	// Config.WatchedRepos — expected, benign: an owner's installation token
	// can plausibly cover repos beyond what Pruefer watches.
	DropUnwatchedRepo DropReason = "unwatched_repo"
	// DropReviewInFlight means a review for this exact PR is already in
	// progress (from either dispatch path) — expected, benign.
	DropReviewInFlight DropReason = "review_in_flight"
	// DropPRNotOpen means the PR's authoritative current state (re-fetched
	// from GitHub, never trusted from the webhook payload) is no longer
	// open — e.g. a stale/delayed delivery arriving after the PR closed or
	// merged.
	DropPRNotOpen DropReason = "pr_not_open"
)

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
