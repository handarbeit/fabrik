# ADR-043: Reconcile-driven webhook health detection

**Status:** Accepted  
**Issue:** #641  
**Date:** 2026-05-07

## Context

The original webhook health model (ADR-032) used an event-silence heuristic: if no verified webhook event arrived within `webhookHealthWindow` (10 minutes), the stream was declared `Unhealthy`, triggering a full `Reconcile` + `Pause`/`Resume` cycle.

This conflated two fundamentally different conditions:

- **Idle board with healthy webhooks**: GitHub produces no events when nothing changes. The heuristic marks the stream unhealthy after 10 minutes of board inactivity even when the cache is perfectly coherent — producing a false-unhealthy transition that burns a GraphQL call for a full `FetchProjectBoard` + `Reconcile`.

- **Dead webhook stream with active board**: Webhook silence AND cache staleness — the true unhealthy case.

The threshold was raised from 60 s to 5 minutes in #638, and a startup grace window (`webhookStartupGrace = 30s`) was added. These tuned the symptom but did not fix the root cause: **event silence is not the same as cache staleness**.

Additionally, the `healthChangeFn` callback (ADR-034) created an indirect coupling between the `webhookManager` struct and the cache Pause/Reconcile/Resume sequence — a callback injected at construction time that made the health-to-cache wiring non-obvious.

## Decision

**Replace event-silence health detection with a periodic light-reconcile loop.**

The `reconcileTicker` goroutine runs in `engine/poll.go:Run()` at a configurable cadence (default: 3 minutes). On each tick it calls `cacheImpl.LightReconcile(...)`, which:

1. Snapshots the current cache items (under lock).
2. Calls `c.fallback.FetchProjectBoard(...)` to fetch a shallow live snapshot from GitHub (without holding the lock).
3. Compares each item on `status`, `len(labels)`, and `updatedAt`.
4. Returns `(driftCount int, driftedKeys []string, freshBoard *gh.ProjectBoard, err error)`.

The `reconcileTicker` goroutine acts on the result:

| Result | Action |
|--------|--------|
| No drift | `wm.transitionHealthState(Healthy, "")` |
| Drift detected | `transitionHealthState(Unhealthy, ...)` → `Pause()` → `Reconcile(freshBoard)` → `Resume()` → `transitionHealthState(Healthy, "drift reconciled")` |
| Network error | Log warning; no state change |

The `freshBoard` returned by `LightReconcile` is passed directly to `Reconcile()` on drift — no second API call is needed.

`healthChangeFn` and the `runHealthMonitor` / `checkHealthTransitions` functions are removed. The `reconcileTicker` goroutine owns the Pause/Reconcile/Resume sequence directly.

## What changed

**Removed:**
- `runHealthMonitor` and `checkHealthTransitions` functions from `engine/webhook.go`
- `healthChangeFn` field, constructor parameter, and closure from `webhookManager` / `poll.go`
- Event-silence constants: `webhookEventStaleTimeout`, `webhookHealthWindow`, `webhookStartupGrace`
- Session-level health fields: `lastEventTime`, `sessionFirstStartAt`, `sessionLastEventAt`
- `handleWebhook` no longer sets `wm.state = WebhookStreamHealthy` on verified events

**Added:**
- `boardcache.LightReconcile()` — shallow drift-detection method on `*CacheImpl`
- `reconcileTicker` goroutine in `engine/poll.go:Run()` (started only when `cacheImpl != nil && webhookMgr != nil`)
- `wm.transitionHealthState(newState, reason)` — compact helper that acquires `wm.mu`, updates state if changed, logs the transition, and calls `emitCurrentState()`
- `lightReconcileInterval = 3 * time.Minute` constant in `engine/webhook.go`
- `--reconcile-interval` flag and `FABRIK_RECONCILE_INTERVAL` env var (int seconds; 0 → 180 s default)
- `ReconcileInterval time.Duration` in `engine.Config`

## Startup behavior

`WebhookStreamStartingUp` is retained with **pessimistic behavior**: health stays `StartingUp` until the first `reconcileTicker` tick completes (no drift → transitions to `Healthy`; drift → reconciles and then transitions to `Healthy`). `IsHealthyOrStartingUp()` returns `true` for `StartingUp`, so the 60-minute idle cap applies as soon as the subprocess launches. The first tick (~3 minutes after launch) resolves the state.

The `webhookStartupGrace` constant is removed; `StartingUp` naturally persists until the first tick.

## API cost

Each `LightReconcile` tick costs one `FetchProjectBoard` GraphQL call. At the default 3-minute cadence: **20 calls/hour** — negligible against the 5000-point/hour budget. At 5-minute cadence: 12 calls/hour.

## Consequences

- **Idle boards no longer trigger false-unhealthy transitions.** Hours of webhook silence on a quiet board produce no health state change, no Pause, and no unnecessary GraphQL spend.
- **Drift is detected within one reconcile interval** (≤ 3 minutes by default) regardless of whether a webhook event arrived.
- **Network errors during reconcile are silent.** A transient GitHub API failure during `LightReconcile` logs a warning and makes no state change — it does not flip health to unhealthy, avoiding the false-positive regression introduced by a different mechanism.
- **Drift window briefly sets state to `Unhealthy` then immediately back to `Healthy`** within the same tick. If `IsHealthyOrStartingUp()` is sampled in this window, the poll loop uses the 5-minute idle cap for one cycle. This is acceptable and mirrors the existing Pause/Reconcile/Resume window.
- **Startup latency**: The engine operates with the shorter idle cap (5 minutes) for up to ~3 minutes at startup, until the first reconcile tick confirms no drift and transitions to `Healthy`. Previously, the first verified webhook event could trigger a `Healthy` transition within seconds. This is a minor startup regression accepted as a tradeoff for correctness.
- **422 circuit-breaker no longer immediately pauses the cache.** The circuit-breaker in `supervise()` still sets `wm.disabled = true` and `wm.state = WebhookStreamUnhealthy`, but no longer calls `cacheImpl.Pause()` directly. The cache continues serving from its in-memory state until the next reconcile tick; if the board is truly unchanged (as expected when webhooks are merely disabled), the tick finds no drift and health remains or recovers to `Healthy`.

## Cross-references

- [ADR-032: Webhook-Driven Event Delivery](032-webhook-event-delivery.md) — established the three-state health model; this ADR supersedes its health-detection and state-transition sections while retaining the three states (`StartingUp`/`Healthy`/`Unhealthy`) and their idle-cap semantics.
- [ADR-034: Board-Cache Event-Sourced Delta](034-boardcache-event-sourced-delta.md) — established `healthChangeFn` as the bridge from webhook health to cache Pause/Resume; the `healthChangeFn` callback is removed by this ADR.
- [Issue #638](https://github.com/handarbeit/fabrik/issues/638) — raised `webhookEventStaleTimeout` from 60 s to 5 min (treated the symptom; this ADR fixes the root cause).
