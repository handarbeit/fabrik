package pruefer

import (
	"context"
	"strconv"

	"github.com/handarbeit/fabrik/pruefer/events"
)

// ReviewFromEvent resolves owner/repo to its owner-scoped GitHubLister
// client, fetches the PR's authoritative current state from GitHub — never
// trusting webhook payload contents as PR state, per the issue's "webhooks
// as triggers, not source of truth" requirement — and dispatches it through
// runReview, the same review execution path poll() uses. Any failure to
// resolve a client or fetch PR details is logged and the event is dropped:
// non-fatal, since a missed event either gets retried on GitHub's
// at-least-once redelivery or is caught by the poll-fallback safety net.
//
// The semaphore slot is acquired here, before FetchPRDetails, not just
// before runReview — a burst of webhook events would otherwise fan out an
// unbounded number of concurrent FetchPRDetails REST calls (one goroutine
// per event, gated by nothing) before ever touching the daemon's
// concurrency budget, exactly the budget-bypass the shared semaphore exists
// to prevent. Acquiring can block arbitrarily long while the budget is
// full, so this must never be called inline from daemonEventSink.Handle (it
// is always invoked in its own goroutine there) or it would stall acking
// the current webhook and reading the next one off the same connection.
func (d *Daemon) ReviewFromEvent(ctx context.Context, owner, repo string, prNumber int) {
	client, ok := d.Clients[owner]
	if !ok {
		logf(prNumber, "warn", "event for %s/%s#%d: no client for owner %q — dropping\n", owner, repo, prNumber, owner)
		return
	}

	sem := d.semaphore()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-sem }()

	pr, err := client.FetchPRDetails(owner, repo, prNumber)
	if err != nil {
		logf(prNumber, "warn", "event for %s/%s#%d: fetching PR details: %v — dropping\n", owner, repo, prNumber, err)
		return
	}

	d.runReview(ctx, client, owner, repo, *pr)
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
//
// Both dispatch paths below run in their own goroutine so Handle itself
// always returns immediately, regardless of whether the daemon's shared
// semaphore (ReviewFromEvent) or an in-flight poll cycle's wg.Wait()
// (poll) is currently blocked on other work. Handle is called synchronously
// from the hookdeck.Source read loop (see source.go's handleFrame/ack) —
// blocking here would delay acking the current webhook and stall reading
// every subsequent one, exactly what the issue's "ack the webhook promptly
// ... never run a review synchronously in the webhook receiver" requirement
// forbids.
func (s *daemonEventSink) Handle(ctx context.Context, ev events.GitHubEvent) {
	switch {
	case ev.EventType == "pull_request" && reviewTriggerActions[ev.Action]:
		prNumber, err := strconv.Atoi(ev.ResourceID)
		if err != nil {
			logf(0, "warn", "pull_request event with unparseable ResourceID %q: %v — dropping\n", ev.ResourceID, err)
			return
		}
		go s.daemon.ReviewFromEvent(ctx, ev.Owner, ev.Repo, prNumber)
	case installEventTypes[ev.EventType]:
		logf(0, "poll", "installation change event (%s) — triggering a reconciliation poll\n", ev.EventType)
		go s.daemon.poll(ctx)
	}
}
