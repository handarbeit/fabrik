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
// executeReview, the same review execution path poll() uses. Any failure to
// resolve a client, an unwatched repo, or a failure to fetch PR details is
// logged and the event is dropped: non-fatal, since a missed event either
// gets retried on GitHub's at-least-once redelivery or is caught by the
// poll-fallback safety net.
//
// d.Clients is owner-keyed, not repo-keyed — see isWatchedRepo's doc for why
// an explicit membership check against Config.WatchedRepos is required here
// even though poll() never needs one.
//
// The semaphore slot is acquired first, before anything else — including
// the PR's stripe lock (prLock) — to match reviewOne/runReview's own
// acquisition order (semaphore, then prLock inside the spawned goroutine).
// Acquiring these two resources in opposite orders across the two dispatch
// paths is a classic AB-BA lock-ordering inversion and a real deadlock risk
// under semaphore saturation: an event goroutine holding prLock(X) while
// blocked on the semaphore, racing a poll goroutine that just won a freed
// slot and then blocks on prLock(X), can — if this pattern recurs across
// enough distinct PRs to fill every slot — wedge the daemon's entire
// concurrency budget with no timeout or ctx-based escape on runReview's
// mu.Lock(). Acquiring the semaphore first in both paths closes that: this
// path never blocks on prLock while holding a slot (see TryLock below), so
// it can never be the "poll goroutine" side of the cycle.
//
// After the slot is held, the PR's stripe lock is claimed non-blockingly
// (TryLock, not Lock): if a review for this exact PR is already in flight
// (from either dispatch path), this event drops immediately — briefly
// holding, then releasing, the slot it just acquired — rather than blocking
// on that lock for the duration of the in-flight review. Dropping is safe:
// the in-flight review already covers this PR, and if a further push
// follows after it completes, a later event (or the poll-fallback safety
// net) will trigger a fresh review at the new head SHA.
//
// Acquiring the semaphore can block arbitrarily long while the budget is
// full, so this must never be called inline from daemonEventSink.Handle (it
// is always invoked in its own goroutine there) or it would stall acking
// the current webhook and reading the next one off the same connection.
func (d *Daemon) ReviewFromEvent(ctx context.Context, owner, repo string, prNumber int) {
	client, ok := d.Clients[owner]
	if !ok {
		logf(prNumber, "warn", "event for %s/%s#%d: no client for owner %q — dropping\n", owner, repo, prNumber, owner)
		return
	}
	if !d.isWatchedRepo(owner, repo) {
		logf(prNumber, "warn", "event for %s/%s#%d: repo is not in watched_repos — dropping\n", owner, repo, prNumber)
		return
	}

	sem := d.semaphore()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-sem }()

	mu := d.prLock(owner, repo, prNumber)
	if !mu.TryLock() {
		logf(prNumber, "info", "event for %s/%s#%d: a review is already in flight for this PR — dropping\n", owner, repo, prNumber)
		return
	}
	defer mu.Unlock()

	pr, err := client.FetchPRDetails(owner, repo, prNumber)
	if err != nil {
		logf(prNumber, "warn", "event for %s/%s#%d: fetching PR details: %v — dropping\n", owner, repo, prNumber, err)
		return
	}
	// Unlike poll() (which only ever sees PRs ListOpenPRs returned, i.e.
	// open by construction), this path fetches by number regardless of
	// current state — a webhook can arrive for a PR that's since been
	// closed or merged (e.g. synchronize immediately followed by merge, or
	// a stale/delayed delivery). Eligible/ReviewPR don't check PR state, so
	// without this guard such an event would proceed straight to cloning
	// and submitting a formal review against a closed/merged PR.
	if pr.State != "open" || pr.Merged {
		logf(prNumber, "info", "event for %s/%s#%d: PR is no longer open (state=%s merged=%v) — dropping\n", owner, repo, prNumber, pr.State, pr.Merged)
		return
	}

	d.executeReview(ctx, client, owner, repo, *pr)
}

// reviewTriggerActions are the pull_request webhook actions that should
// trigger a review, per the issue's "Triggers, not source of truth"
// requirement. reopened is included alongside opened/synchronize/
// ready_for_review: a closed-then-reopened PR needs a fresh review just as
// much as a newly opened one, and without it here the PR would silently
// wait out the reconciliation-fallback interval (or a full poll sweep)
// before pruefer notices — the same "review it now" transition, just
// missed.
var reviewTriggerActions = map[string]bool{
	"opened":           true,
	"reopened":         true,
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
// Handle itself always returns immediately, regardless of whether the
// daemon's shared semaphore (ReviewFromEvent) or an in-flight poll cycle's
// wg.Wait() (poll) is currently blocked on other work. Handle is called
// synchronously from the hookdeck.Source read loop (see source.go's
// handleFrame/ack) — blocking here would delay acking the current webhook
// and stall reading every subsequent one, exactly what the issue's "ack the
// webhook promptly ... never run a review synchronously in the webhook
// receiver" requirement forbids.
//
// The pull_request branch dispatches via its own `go`, since
// ReviewFromEvent itself blocks (on the semaphore, then optionally the PR
// lock). The install-event branch calls triggerReconciliationPoll directly,
// not via `go` — that's still non-blocking, but by triggerReconciliationPoll's
// own construction (an atomic CompareAndSwap guard around an internally
// spawned goroutine), not because this call site wraps it. If
// triggerReconciliationPoll ever grew blocking work ahead of that
// CompareAndSwap, this call site would need its own `go` too.
//
// This makes the per-event goroutine count itself uncapped — a burst of
// legitimate events across many distinct PRs can pile up more goroutines
// than the semaphore currently admits, each blocked waiting for a slot
// (holding little more than an HTTP round trip's worth of state). Accepted
// deliberately rather than bounding with a dispatch queue: none of these
// goroutines can leak (ReviewFromEvent always returns, releasing its
// goroutine), Go goroutines are cheap relative to the concurrency-capped
// work behind the semaphore, and Hookdeck's dedupe plus GitHub's own event
// volume make an unbounded burst an edge case, not the steady state a
// bounded queue would need to justify its own added complexity.
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
		s.daemon.triggerReconciliationPoll(ctx)
	}
}
