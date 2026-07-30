package pruefer

import (
	"context"
	"strconv"
	"sync"

	"github.com/handarbeit/fabrik/pruefer/events"
)

// ReviewFromEvent resolves owner/repo to its owner-scoped GitHubLister
// client, fetches the PR's authoritative current state from GitHub — never
// trusting webhook payload contents as PR state, per the issue's "webhooks
// as triggers, not source of truth" requirement — and dispatches it through
// the same concurrency-capped path poll() uses (reviewOne). Any failure to
// resolve a client or fetch PR details is logged and the event is dropped:
// non-fatal, since a missed event either gets retried on GitHub's
// at-least-once redelivery or is caught by the poll-fallback safety net.
//
// Returns once dispatch has been handed to reviewOne's semaphore-gated
// goroutine, not once the review itself completes — matching
// events.EventSink.Handle's "must not block on a long-running review"
// contract.
func (d *Daemon) ReviewFromEvent(ctx context.Context, owner, repo string, prNumber int) {
	client, ok := d.Clients[owner]
	if !ok {
		logf(prNumber, "warn", "event for %s/%s#%d: no client for owner %q — dropping\n", owner, repo, prNumber, owner)
		return
	}

	pr, err := client.FetchPRDetails(owner, repo, prNumber)
	if err != nil {
		logf(prNumber, "warn", "event for %s/%s#%d: fetching PR details: %v — dropping\n", owner, repo, prNumber, err)
		return
	}

	// reviewOne's signature is shared with poll()'s multi-PR fan-out, which
	// needs a caller-owned *sync.WaitGroup to wait on the whole cycle. A
	// single event has no cycle to wait for, so this WaitGroup is
	// single-use and never waited on here — reviewOne's own goroutine runs
	// to completion independently, gated by the daemon's shared semaphore.
	var wg sync.WaitGroup
	d.reviewOne(ctx, &wg, client, owner, repo, *pr)
}

// reviewTriggerActions are the pull_request webhook actions that should
// trigger a review, per the issue's "Triggers, not source of truth"
// requirement.
var reviewTriggerActions = map[string]bool{
	"opened":           true,
	"synchronize":      true,
	"ready_for_review": true,
}

// installEventTypes are GitHub webhook event types signaling an
// installation/repo-selection change. These trigger a full reconciliation
// poll rather than a single-PR review: Daemon.Clients is owner-keyed and
// fixed at startup (see execute.go/BootstrapMulti), so dynamically
// discovering a new installation mid-run is out of scope — re-checking
// GitHub state via a poll sweep satisfies "webhooks are triggers, GitHub is
// truth" without inventing new auth machinery.
var installEventTypes = map[string]bool{
	"installation":              true,
	"installation_repositories": true,
}

// daemonEventSink implements events.EventSink by dispatching normalized
// GitHub events to a Daemon's existing review machinery. It never touches
// ReviewPR/select.go directly, nor any transport-specific (e.g. Hookdeck)
// concept — the events.GitHubEvent it receives is already transport-agnostic.
type daemonEventSink struct {
	daemon *Daemon
}

// Handle implements events.EventSink. Event types and pull_request actions
// Pruefer has no opinion on are silently no-ops, not an error condition —
// GitHub (and Hookdeck's forwarding scope) can deliver many event types
// this sink never needs to act on.
func (s *daemonEventSink) Handle(ctx context.Context, ev events.GitHubEvent) {
	switch {
	case ev.EventType == "pull_request" && reviewTriggerActions[ev.Action]:
		prNumber, err := strconv.Atoi(ev.ResourceID)
		if err != nil {
			logf(0, "warn", "pull_request event with unparseable ResourceID %q: %v — dropping\n", ev.ResourceID, err)
			return
		}
		s.daemon.ReviewFromEvent(ctx, ev.Owner, ev.Repo, prNumber)
	case installEventTypes[ev.EventType]:
		logf(0, "poll", "installation change event (%s) — triggering a reconciliation poll\n", ev.EventType)
		s.daemon.poll(ctx)
	}
}
