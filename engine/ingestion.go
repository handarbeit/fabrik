package engine

// eventIngestionManager is the minimal shared surface both event-ingestion
// transports implement — the existing gh-webhook-forward manager (PAT mode,
// *webhookManager) and the Hookdeck manager (App-auth mode, *hookdeckManager,
// #1142) — so poll.go's health check, UpdateRepos call, and the ~40 scattered
// RegisterEcho*/MatchEcho call sites across the engine package work
// unmodified regardless of which transport (if either) is active.
//
// Echo-check (RegisterEcho*/MatchEcho) deliberately stops at this interface
// rather than growing transport-specific behavior underneath it: the
// mechanism exists to infer "the gh webhook forward subprocess looks alive
// but isn't actually delivering" — a failure mode Hookdeck doesn't have,
// since its OnHealth callback reports genuine WebSocket connectivity
// directly. hookdeckManager implements these as no-ops rather than
// replicating the mutation-echo heuristic for a signal it already has
// natively. See adrs/1142-hookdeck-ingestion-for-app-auth.md.
type eventIngestionManager interface {
	// IsHealthyOrStartingUp returns true when the transport's connectivity
	// state should be treated as healthy (or still within its startup grace
	// window) for the purposes of e.g. extending idle-cap behavior.
	IsHealthyOrStartingUp() bool
	// IsDisabled returns true when the transport has permanently disabled
	// itself for this run (e.g. a circuit breaker) and the engine should
	// treat delivery as poll-only. Hookdeck has no per-repo circuit-breaker
	// analogue, so hookdeckManager always returns false.
	IsDisabled() bool
	// Stop shuts the transport down.
	Stop()
	// UpdateRepos is called after each board poll with the current
	// managed-repo set.
	UpdateRepos(repos map[string]bool)
	// RegisterEcho, RegisterEchoIfSubscribed, and MatchEcho implement the
	// gh-webhook-forward echo-check mechanism; hookdeckManager's
	// implementations are no-ops (see the type doc above).
	RegisterEcho(eventType, action, key string)
	RegisterEchoIfSubscribed(eventType, action, key string)
	MatchEcho(eventType, action, key string)
}

// var _ eventIngestionManager = (*webhookManager)(nil) confirms *webhookManager
// already satisfies this interface without any changes to webhook.go.
var _ eventIngestionManager = (*webhookManager)(nil)
