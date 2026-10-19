package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
)

// reconcileRequestMinGap is the minimum spacing between a catch-up reconcile
// requested by a transport reconnect and the previous reconcile run. A flapping
// connection requests repeatedly; the requests coalesce into one delayed run
// rather than one reconcile per flap. A variable so tests can shrink it.
var reconcileRequestMinGap = 10 * time.Second

// requestReconcile asks reconcileLoop for an immediate catch-up reconcile
// (R5, #2351). Any transport calls it when it re-establishes its connection:
// events delivered while it was down are lost to the webhook path and only a
// reconcile heals them. Non-blocking; requests made while one is already pending
// coalesce into it.
func (e *Engine) requestReconcile() {
	select {
	case e.reconcileReq <- struct{}{}:
	default:
	}
}

// reconcileLoop is the poll-only correctness backstop for cache/GitHub divergence.
// It periodically runs LightReconcile (a fresh shallow board fetch + drift compare)
// and, on drift, reconciles the cache — re-syncing shallow fields including the
// fabrik-managed label set. It MUST run whether or not an event-ingestion
// transport started (#955): a webhook-less/hookdeck-less (or failed) deployment
// must still self-heal, so this loop is launched unconditionally rather than
// nested in any transport's start path. ts may be nil or empty (no transport is
// active or it failed to start); coverage checks and health-state transitions
// are no-ops in that case, but drift detection and repair still run.
//
// ts is the transport-neutral set (#2351): coverage re-checks and health
// transitions go through the ingestTransport interface, so any transport —
// including ones added later — is covered with no type switch here. Besides the
// ticker, a transport reconnect (requestReconcile) triggers a catch-up tick,
// coalesced and spaced by reconcileRequestMinGap.
func (e *Engine) reconcileLoop(ctx context.Context, cacheImpl *boardcache.CacheImpl, ts *transportSet) {
	reconcileInterval := e.cfg.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = lightReconcileInterval
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	var lastRun time.Time
	var gap <-chan time.Time
	run := func() {
		lastRun = time.Now()
		// TEST-ONLY (#1978): with the bed's poll seam enabled a held bed drops
		// the tick; unset (production) this is a direct call.
		if e.pollSeam != nil {
			e.pollSeam.runUnlessHeld(func() { e.reconcileTick(cacheImpl, ts) })
		} else {
			e.reconcileTick(cacheImpl, ts)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		case <-e.reconcileReq:
			if gap != nil {
				continue // a spaced catch-up is already waiting; it covers this request
			}
			if wait := reconcileRequestMinGap - time.Since(lastRun); wait > 0 && !lastRun.IsZero() {
				gap = time.After(wait)
				continue
			}
			e.logf(0, "reconcile", "transport reconnected — running a catch-up reconcile\n")
			run()
		case <-gap:
			gap = nil
			e.logf(0, "reconcile", "transport reconnected — running a catch-up reconcile\n")
			run()
		}
	}
}

// reconcileTick is one reconcileLoop tick: the transports' coverage re-check, a
// LightReconcile drift compare and, on drift, a cache reconcile. Extracted
// unchanged from the loop body so the test-only poll seam (#1978) can gate it.
func (e *Engine) reconcileTick(cacheImpl *boardcache.CacheImpl, ts *transportSet) {
	// R5 periodic coverage re-check (#1142): a hook deleted (or a
	// newly-managed repo added, or an installation's granted-repo set
	// changed) mid-run should be caught here, not just at startup.
	// Best-effort; never blocks or fails the reconcile pass.
	ts.CheckCoverage()
	driftCount, driftedKeys, freshBoard, err := cacheImpl.LightReconcile(
		e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType,
	)
	if err != nil {
		e.logf(0, "reconcile", "light reconcile failed (no health state change): %v\n", err)
		return
	}
	e.health.noteReconcileOK(e.now())
	if driftCount == 0 {
		// The full Reconcile is skipped on zero drift, but the board is already
		// fetched: capture milestones from it so they become known on a quiet board.
		cacheImpl.ApplyBoardMilestones(freshBoard)
		ts.TransitionHealth(true, "")
		return
	}
	keyStr := fmt.Sprintf("%v", driftedKeys)
	if len(driftedKeys) > 5 {
		keyStr = fmt.Sprintf("%v … %d more", driftedKeys[:5], len(driftedKeys)-5)
	}
	e.logf(0, "reconcile", "light reconcile: %d item(s) drifted (%s) — reconciling cache\n", driftCount, keyStr)
	ts.TransitionHealth(false, fmt.Sprintf("%d item(s) drifted", driftCount))
	cacheImpl.Pause()
	cacheImpl.Reconcile(freshBoard)
	cacheImpl.Resume()
	ts.TransitionHealth(true, "drift reconciled")
}

// applyLayer1StatusRefresh handles the Layer 1 opportunistic per-event Status
// refresh. Called from the deltaFn closure after ApplyDelta. For issue and
// issue_comment events, it fetches the current Status from GitHub and updates
// the cache immediately. Two paths:
//   - Fast path: cache has the item's itemID → calls FetchProjectItemStatus.
//   - Fallback path: cache lacks itemID (brand-new issue, issues.opened before
//     projects_v2_item.created) → calls LookupIssueProjectItem to populate
//     both itemID and Status in one query. Skipped when cache.ProjectID() == ""
//     (Bootstrap not yet complete).
//
// PR conversation comments (an issue_comment whose issue carries a
// pull_request key) are skipped outright: LookupIssueProjectItem queries
// repository.issue(number:) and cannot resolve a PR number (#2093). A
// successful "not on the board" fallback lookup is negative-cached per
// (project, repo, number) for layer1OffBoardTTL so repeated events on an
// off-board issue repeat no call; errors are never cached. The entry is
// bypassed once the cache holds an itemID (the fast path is checked first) and
// cleared on every projects_v2_item "created" event.
//
// The Status is applied as a read (applyLayer1Status, #2347), never as an engine
// write.
//
// All errors are best-effort: logged as warnings and never returned.
func (e *Engine) applyLayer1StatusRefresh(eventType string, payload []byte, cache *boardcache.CacheImpl) {
	if eventType == "projects_v2_item" {
		var pv struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &pv); err == nil && pv.Action == "created" {
			// Deliberately not filtered by project (#2345): the cache drops a
			// foreign-project event itself, and clearing the negative cache is
			// the safe direction — at worst it costs one re-check.
			e.layer1OffBoardClearAll()
		}
		return
	}
	if cache.IsPaused() {
		return
	}
	if eventType != "issues" && eventType != "issue_comment" {
		return
	}
	var ev struct {
		Issue struct {
			Number      int             `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		e.logf(0, "warn", "layer1 status refresh: failed to parse %s payload: %v\n", eventType, err)
		return
	}
	if ev.Repository.FullName == "" || ev.Issue.Number == 0 {
		return
	}
	if rawPresent(ev.Issue.PullRequest) {
		return
	}
	key := boardcache.ItemKey(ev.Repository.FullName, ev.Issue.Number)
	itemID, ok := cache.GetItemID(key)
	if !ok {
		// Fallback: issue is in the cache but has no itemID yet (e.g., arrived via
		// issues.opened before a projects_v2_item.created event). Perform a single
		// GraphQL lookup to populate both itemID and Status in one call.
		// If Bootstrap hasn't completed yet, projectID is empty — skip to avoid a
		// useless API call with an empty project ID during the startup window.
		projectID := cache.ProjectID()
		if projectID == "" {
			return
		}
		offKey := layer1OffBoardKey(projectID, ev.Repository.FullName, ev.Issue.Number)
		if e.layer1OffBoardHit(offKey) {
			return
		}
		fetchedAt := time.Now()
		fetchedItemID, fetchedStatus, err := e.client.LookupIssueProjectItem(projectID, ev.Repository.FullName, ev.Issue.Number)
		if err != nil {
			e.logf(ev.Issue.Number, "warn", "layer1 fallback lookup failed for %s#%d: %v\n", ev.Repository.FullName, ev.Issue.Number, err)
			return
		}
		if fetchedItemID == "" {
			// Issue is not on the project fabrik manages — remember that for a
			// while and silently skip.
			e.layer1OffBoardMark(offKey)
			return
		}
		e.layer1OffBoardClear(offKey)
		cache.RegisterItemID(key, fetchedItemID)
		e.applyLayer1Status(cache, key, fetchedStatus, fetchedAt)
		return
	}
	fetchedAt := time.Now()
	status, err := e.client.FetchProjectItemStatus(itemID)
	if err != nil {
		e.logf(ev.Issue.Number, "warn", "layer1 status fetch failed for %s: %v\n", itemID, err)
		return
	}
	e.applyLayer1Status(cache, key, status, fetchedAt)
}

// applyLayer1Status applies a Status read by the Layer 1 refresh. The read is a
// GitHub observation, not an engine move (#2347): it goes in as a read-sourced
// mutation carrying fetchedAt — the wall-clock instant taken before the GitHub
// call, since the Store stamps with time.Now() and e.now()/SetClock are not
// comparable to those stamps (ADR 2178) — so the engine-write protection and
// the stale-read drop apply, no engine-write record is created and the change is
// not attributed to Fabrik. See ADR 2347.
func (e *Engine) applyLayer1Status(cache *boardcache.CacheImpl, key, status string, fetchedAt time.Time) {
	if e.layer1AsEngineWrite.Load() {
		cache.UpdateItemStatus(key, status)
		return
	}
	cache.ApplyStatusRefresh(key, status, fetchedAt)
}

// rawPresent reports whether a JSON value was present and meaningful: false
// for a missing key, a literal null, or whitespace only.
func rawPresent(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}
