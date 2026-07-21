package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
)

// reconcileLoop is the poll-only correctness backstop for cache/GitHub divergence.
// It periodically runs LightReconcile (a fresh shallow board fetch + drift compare)
// and, on drift, reconciles the cache — re-syncing shallow fields including the
// fabrik-managed label set. It MUST run whether or not the webhook manager started
// (#955): a webhook-less (or webhook-failed) deployment must still self-heal, so
// this loop is launched unconditionally rather than nested in the webhook-start
// path. wm may be nil (webhooks off/failed); webhook health-state transitions are
// skipped in that case, but drift detection and repair still run.
func (e *Engine) reconcileLoop(ctx context.Context, cacheImpl *boardcache.CacheImpl, wm *webhookManager) {
	reconcileInterval := e.cfg.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = lightReconcileInterval
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			driftCount, driftedKeys, freshBoard, err := cacheImpl.LightReconcile(
				e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType,
			)
			if err != nil {
				e.logf(0, "reconcile", "light reconcile failed (no health state change): %v\n", err)
				continue
			}
			if driftCount == 0 {
				if wm != nil {
					wm.transitionHealthState(WebhookStreamHealthy, "")
				}
				continue
			}
			keyStr := fmt.Sprintf("%v", driftedKeys)
			if len(driftedKeys) > 5 {
				keyStr = fmt.Sprintf("%v … %d more", driftedKeys[:5], len(driftedKeys)-5)
			}
			e.logf(0, "reconcile", "light reconcile: %d item(s) drifted (%s) — reconciling cache\n", driftCount, keyStr)
			if wm != nil {
				wm.transitionHealthState(WebhookStreamUnhealthy, fmt.Sprintf("%d item(s) drifted", driftCount))
			}
			cacheImpl.Pause()
			cacheImpl.Reconcile(freshBoard)
			cacheImpl.Resume()
			if wm != nil {
				wm.transitionHealthState(WebhookStreamHealthy, "drift reconciled")
			}
		}
	}
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
// All errors are best-effort: logged as warnings and never returned.
func (e *Engine) applyLayer1StatusRefresh(eventType string, payload []byte, cache *boardcache.CacheImpl) {
	if cache.IsPaused() {
		return
	}
	if eventType != "issues" && eventType != "issue_comment" {
		return
	}
	var ev struct {
		Issue struct {
			Number int `json:"number"`
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
		fetchedItemID, fetchedStatus, err := e.client.LookupIssueProjectItem(projectID, ev.Repository.FullName, ev.Issue.Number)
		if err != nil {
			e.logf(ev.Issue.Number, "warn", "layer1 fallback lookup failed for %s#%d: %v\n", ev.Repository.FullName, ev.Issue.Number, err)
			return
		}
		if fetchedItemID == "" {
			// Issue is not on the project fabrik manages — silently skip.
			return
		}
		cache.RegisterItemID(key, fetchedItemID)
		cache.UpdateItemStatus(key, fetchedStatus)
		return
	}
	status, err := e.client.FetchProjectItemStatus(itemID)
	if err != nil {
		e.logf(ev.Issue.Number, "warn", "layer1 status fetch failed for %s: %v\n", itemID, err)
		return
	}
	cache.UpdateItemStatus(key, status)
}
