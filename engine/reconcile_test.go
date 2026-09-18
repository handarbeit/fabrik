package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/tui"
)

// ---------------------------------------------------------------------------
// Layer 1 — opportunistic per-event Status refresh
// ---------------------------------------------------------------------------

// seedTestCache creates a CacheImpl bootstrapped with one item (PVTI_001, owner/repo#1, "Research").
func seedTestCache(t *testing.T, client *mockGitHubClient) *boardcache.CacheImpl {
	t.Helper()
	cache := boardcache.NewCacheImpl(client, itemstate.NewStore(nil), func(format string, args ...any) {})
	board := &gh.ProjectBoard{
		ProjectID: "PVT_test",
		Items: []gh.ProjectItem{
			{Number: 1, ItemID: "PVTI_001", Status: "Research", Repo: "owner/repo"},
		},
	}
	testBootstrapFromBoard(cache, board)
	return cache
}

func TestLayer1StatusRefresh_UpdatesCacheOnIssueCommentEvent(t *testing.T) {
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			return "Plan", nil
		},
	}
	cache := seedTestCache(t, client)
	eng := testEngine(t, client, &mockClaudeInvoker{})

	payload, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 1},
		"repository": map[string]any{"full_name": "owner/repo"},
	})
	eng.applyLayer1StatusRefresh("issue_comment", payload, cache)

	// FetchProjectItemStatus must have been called for PVTI_001.
	client.mu.Lock()
	calls := append([]string(nil), client.fetchProjectItemStatusCalls...)
	client.mu.Unlock()
	if len(calls) != 1 || calls[0] != "PVTI_001" {
		t.Errorf("want FetchProjectItemStatus called once with %q, got %v", "PVTI_001", calls)
	}

	// Verify the cache now reflects "Plan" — the status returned by FetchProjectItemStatus.
	board, err := cache.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	var gotStatus string
	for _, item := range board.Items {
		if item.Number == 1 {
			gotStatus = item.Status
		}
	}
	if gotStatus != "Plan" {
		t.Errorf("want cached Status %q after Layer 1 refresh, got %q", "Plan", gotStatus)
	}
}

func TestLayer1StatusRefresh_SkipsWhenCachePaused(t *testing.T) {
	var callCount int32
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			atomic.AddInt32(&callCount, 1)
			return "Plan", nil
		},
	}
	cache := seedTestCache(t, client)
	cache.Pause()
	eng := testEngine(t, client, &mockClaudeInvoker{})

	payload, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 1},
		"repository": map[string]any{"full_name": "owner/repo"},
	})
	eng.applyLayer1StatusRefresh("issue_comment", payload, cache)

	if n := atomic.LoadInt32(&callCount); n != 0 {
		t.Errorf("FetchProjectItemStatus should not be called when cache is paused; got %d calls", n)
	}
}

func TestLayer1StatusRefresh_SkipsNonIssueEvents(t *testing.T) {
	var callCount int32
	client := &mockGitHubClient{
		fetchProjectItemStatusFn: func(itemID string) (string, error) {
			atomic.AddInt32(&callCount, 1)
			return "Plan", nil
		},
	}
	cache := seedTestCache(t, client)
	eng := testEngine(t, client, &mockClaudeInvoker{})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{"number": 1},
		"repository":   map[string]any{"full_name": "owner/repo"},
	})
	eng.applyLayer1StatusRefresh("pull_request", payload, cache)

	if n := atomic.LoadInt32(&callCount); n != 0 {
		t.Errorf("FetchProjectItemStatus should not be called for pull_request events; got %d calls", n)
	}
}

// TestReconcileLoop_RunsWithoutWebhookManager is the #955 regression for the
// architectural fix: the reconcile ticker must run — and repair label drift — even
// when the webhook manager is nil (webhooks disabled or wm.Start failed).
// Previously the ticker was nested inside the webhook-start block, so a webhook-less
// deployment never reconciled and could not self-heal a drifted fabrik label set,
// stranding items at gates (e.g. fabrik:awaiting-ci missing from the store forever).
func TestReconcileLoop_RunsWithoutWebhookManager(t *testing.T) {
	t1 := time.Now().Truncate(time.Second)
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.ReconcileInterval = 10 * time.Millisecond

	cache := boardcache.NewCacheImpl(client, eng.store, func(string, ...any) {})
	// Seed #1 at Validate WITHOUT fabrik:awaiting-ci — the drifted store state.
	testBootstrapFromBoard(cache, &gh.ProjectBoard{
		ProjectID: "PVT_1",
		Items: []gh.ProjectItem{
			{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise"}, UpdatedAt: t1},
		},
	})
	eng.readClient = cache

	// GitHub's fresh board: same status + updatedAt, but WITH fabrik:awaiting-ci.
	// Only the fabrik-managed label differs — exactly the #1479 stranding condition.
	client.fetchProjectBoardFn = func(_, _ string, _ int, _ string) (*gh.ProjectBoard, error) {
		return &gh.ProjectBoard{
			ProjectID: "PVT_1",
			Items: []gh.ProjectItem{
				{ID: "I_1", ItemID: "PVTI_1", Number: 1, Repo: "owner/repo", Status: "Validate", Labels: []string{"fabrik:cruise", "fabrik:awaiting-ci"}, UpdatedAt: t1},
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// nil webhook manager — the whole point: reconcile must run anyway.
	go eng.reconcileLoop(ctx, cache, nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		snap, err := eng.store.Get("owner/repo", 1)
		if err == nil {
			for _, l := range snap.Labels() {
				if l == "fabrik:awaiting-ci" {
					return // success: reconcile ran with nil wm and synced the gate label
				}
			}
		}
		if time.Now().After(deadline) {
			var labels []string
			if err == nil {
				labels = snap.Labels()
			}
			t.Fatalf("reconcileLoop did not sync fabrik:awaiting-ci within 2s (nil wm); labels = %v", labels)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTransitionMgrHealthState_HookdeckDriftFreeReconcileDoesNotMaskSignatureDrift
// is the direct regression test for the PR-review-found bug (#1142): before
// the fix, reconcileLoop's "no cache drift found" signal called
// hookdeckManager.transitionHealthState(Healthy) directly, unconditionally
// overriding an active signature-drift episode (R4, ADR-1563) — silently
// clearing the exact misconfigured-secret warning that mechanism exists to
// surface. transitionMgrHealthState must route a "healthy" hint through
// reconcileHint/recomputeHealthState instead, which re-derives the state
// from hm's own connHealth/sigDriftActive rather than accepting reconcile's
// signal as an unconditional override.
func TestTransitionMgrHealthState_HookdeckDriftFreeReconcileDoesNotMaskSignatureDrift(t *testing.T) {
	hm := newHookdeckManager(
		func(int, string, string, ...any) {},
		func(tui.Event) {},
		nil, nil, "key", "secret",
	)
	// Transport is connected...
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if !hm.IsHealthyOrStartingUp() {
		t.Fatal("expected healthy after HealthConnected")
	}
	// ...but an active signature-drift episode forces Unhealthy.
	hm.handleSignatureDrift(true)
	if hm.IsHealthyOrStartingUp() {
		t.Fatal("expected unhealthy while signature drift is active")
	}

	// reconcileLoop's own "no cache drift found" signal must NOT clear the
	// signature-drift-forced Unhealthy state.
	transitionMgrHealthState(hm, WebhookStreamHealthy, "")
	if hm.IsHealthyOrStartingUp() {
		t.Error("transitionMgrHealthState(Healthy) from reconcileLoop must not mask an active signature-drift episode")
	}

	// Once the drift episode clears, the same hint correctly reports Healthy
	// again (connHealth was never lost).
	hm.handleSignatureDrift(false)
	transitionMgrHealthState(hm, WebhookStreamHealthy, "")
	if !hm.IsHealthyOrStartingUp() {
		t.Error("transitionMgrHealthState(Healthy) after drift recovery should report healthy again")
	}

	// A drift-found hint (cache drift, not signature drift) is always safe
	// to apply directly — it only escalates toward Unhealthy.
	transitionMgrHealthState(hm, WebhookStreamUnhealthy, "3 item(s) drifted")
	if hm.IsHealthyOrStartingUp() {
		t.Error("transitionMgrHealthState(Unhealthy) must report unhealthy")
	}
}

// TestTransitionMgrHealthState_HookdeckCacheDriftSurvivesConcurrentHealthEvent
// is the direct regression test for the PR-review-found bug (third pass,
// #1142): before this fix, reconcileLoop's "cache drift found" signal set
// hm.state to Unhealthy via a one-shot transitionHealthState call with no
// persistent backing flag — so a connectivity event firing while
// cacheImpl.Reconcile was still running (handleHealth/handleSignatureDrift,
// which recompute purely from connHealth/sigDriftActive) could silently
// clear the drift-in-progress Unhealthy state before the repair actually
// completed. reconcileHint must record the condition as sticky
// (cacheDriftActive) so recomputeHealthState continues to honor it
// regardless of what triggered the recompute.
func TestTransitionMgrHealthState_HookdeckCacheDriftSurvivesConcurrentHealthEvent(t *testing.T) {
	hm := newHookdeckManager(
		func(int, string, string, ...any) {},
		func(tui.Event) {},
		nil, nil, "key", "secret",
	)
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if !hm.IsHealthyOrStartingUp() {
		t.Fatal("expected healthy after HealthConnected")
	}

	// reconcileLoop finds cache drift and reports it — this must persist as
	// Unhealthy for the duration of the (simulated) repair, not just for
	// the instant this call runs.
	transitionMgrHealthState(hm, WebhookStreamUnhealthy, "5 item(s) drifted")
	if hm.IsHealthyOrStartingUp() {
		t.Fatal("expected unhealthy immediately after a drift-found hint")
	}

	// A connectivity event fires while the (simulated) cache repair is
	// still in flight — e.g. a brief reconnect blip unrelated to the cache
	// drift. This must NOT clear the drift-in-progress Unhealthy state.
	hm.handleHealth(events.HealthEvent{State: events.HealthConnected})
	if hm.IsHealthyOrStartingUp() {
		t.Error("a concurrent HealthConnected event must not clear an in-progress cache-drift Unhealthy state")
	}

	// Once the repair completes, reconcileLoop's "drift reconciled" hint
	// correctly clears it.
	transitionMgrHealthState(hm, WebhookStreamHealthy, "drift reconciled")
	if !hm.IsHealthyOrStartingUp() {
		t.Error("transitionMgrHealthState(Healthy, \"drift reconciled\") should clear the cache-drift condition")
	}
}
