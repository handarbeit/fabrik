# ADR 042: Mutation Echo-Check for Webhook Health Detection

## Status

Accepted

## Context

Fabrik's webhook-based board cache can enter a silent failure mode where GitHub webhooks stop arriving but Fabrik continues operating on stale cached state. The existing time-based health monitor (`webhookHealthWindow` = 10 min, `webhookEventStaleTimeout` = 5 min) only detects this after several minutes of silence.

During an active pipeline run, Fabrik issues many GitHub mutations (label adds/removes, comment posts, PR creates, status updates). Each successful mutation should produce a corresponding webhook event within a few seconds under normal conditions. If multiple expected echoes fail to arrive, the webhook stream has almost certainly failed.

This ADR documents the design of the mutation echo-check (Plan B) that detects silent stream failures within seconds during active mutation periods. It complements the slow-cadence periodic reconcile (Plan A, ADR 036) which catches failures during quiet periods.

## Decision

Add an echo-check subsystem to `webhookManager` that:

1. Records a pending entry in `pendingEchoes` for each successful tracked mutation.
2. Clears the entry when the corresponding inbound webhook arrives via `MatchEcho`.
3. A background sweep goroutine runs every 5 seconds and counts unmatched entries older than `webhookEchoTimeout` (15s) as misses.
4. When K = 3 misses accumulate within a rolling W = 60s window, `healthChangeFn(false)` is called — the same path used by the existing time-based health checks.

## Key Design Choices

### Self-signal via Fabrik's own mutations

Using Fabrik's own mutations as the health signal is sound because: (a) under a healthy webhook stream, Fabrik's mutations reliably produce webhook events (documented in ADR 032 §Self-feedback); (b) the K=3 threshold in a 60s window provides substantial margin against transient GitHub delivery lag (~2–10s typical, 15s cap); (c) the signal volume is highest exactly when false negatives are most costly (active pipeline runs).

### Rolling-window miss threshold (not consecutive)

A rolling time window is used instead of "K consecutive misses" because Fabrik issues many concurrent mutations for different issues. The "consecutive" semantics break down when mutations are interleaved — e.g., an echo from issue A arrives between misses from issue B. Rolling-window misses are well-defined regardless of interleaving.

**Implementation:** Append each miss's timestamp to `missHistory`. On each append, prune entries older than `now - W`. If `len(missHistory) >= K`, fire. Successful `MatchEcho` calls remove the specific `pendingEchoes` entry before the sweep can count it as a miss; they do NOT retroactively remove past miss timestamps. Past misses age out of the rolling window naturally.

### `matchEchoFn` injection to avoid circular import

`boardcache` cannot import `engine` (engine → boardcache is the existing direction). `MatchEcho` is wired into `CacheImpl` via a `matchEchoFn func(string, string, string)` function field set from `poll.go` after `webhookManager` is constructed. This is the same dependency-injection pattern established in ADR 036.

### Suppression during startup

Echo registration and sweep are no-ops while the webhook manager is in `WebhookStreamStartingUp` state. Mutations during the startup grace period have no expected echo because the webhook subscription may not yet be established.

### `missHistory` cleared on recovery

When any verified inbound webhook event flips the stream back to `Healthy`, `missHistory` is cleared in the same `wm.mu` lock block. Without this, stale miss timestamps from before the recovery could immediately re-trigger the threshold after a brief recovery.

### `projects_v2_item` conditional (R7)

`UpdateProjectItemStatus` mutations are excluded from echo registration when `projects_v2_item` is not in `wm.events` at registration time. The `wm.events` slice is mutated by the subprocess stderr handler goroutine when GitHub rejects the event type, so the check must be done under `wm.mu`. This is handled by `RegisterEchoIfSubscribed`, which atomically checks event subscription and registers.

## Constants

All three constants are defined as package-level `var` (not `const`) in `engine/webhook.go` so tests can override them without real tickers:

| Constant | Value | Rationale |
|----------|-------|-----------|
| `webhookEchoTimeout` | 15s | GitHub typical delivery < 2s, up to 10s under load; 5s margin |
| `webhookEchoMissThreshold` | 3 | Three independent misses is far past statistical fluke |
| `webhookEchoWindow` | 60s | Covers a stage's mutation burst; old misses age out within a minute |

## Tracked Mutations

| Mutation | Expected event | Action | Key format |
|----------|---------------|--------|------------|
| `AddLabelToIssue` | `issues` | `labeled` | `owner/repo#N+labelName` |
| `RemoveLabelFromIssue` | `issues` | `unlabeled` | `owner/repo#N+labelName` |
| `AddComment` | `issue_comment` | `created` | `owner/repo#N` |
| `UpdateIssueBody` | `issues` | `edited` | `owner/repo#N` |
| `CreateDraftPR` | `pull_request` | `opened` | `owner/repo#pr<prNum>` |
| `MarkPRReady` | `pull_request` | `ready_for_review` | `owner/repo#pr<prNum>` |
| `MergePR` | `pull_request` | `closed` | `owner/repo#pr<prNum>` |
| `UpdateProjectItemStatus` | `projects_v2_item` | `edited` | `itemID` (R7 conditional) |

## Consequences

**Benefits:**
- Silent webhook stream failures detected within seconds during active pipeline runs (previously took up to 10 minutes).
- Complements Plan A (reconcile loop) for full coverage: Plan B is fast during active periods, Plan A is the safety net during quiet periods.
- No new external dependencies; entirely in-memory state.

**Drawbacks / risks:**
- False positives under sustained GitHub webhook delivery lag > 15s (rare but possible under infrastructure stress). K=3 threshold mitigates but does not eliminate.
- ~25 call sites across 5 engine files must be kept in sync as new mutation APIs are added. Missing a call site leaves a mutation type unmonitored.
- Composite map key `eventType:action:key` uses last-write-wins for concurrent mutations to the same entity. Earlier entries may be overwritten; acceptable since the goal is stream-failure detection, not per-mutation tracking.

## Related ADRs

- [ADR-032: Webhook-Driven Event Delivery](032-webhook-event-delivery.md) — foundational webhook architecture; §Self-feedback loop establishes that Fabrik's own mutations produce webhook events.
- [ADR-034: BoardCache Event-Sourced Delta](034-boardcache-event-sourced-delta.md) — defines `ApplyDelta` as the entry point where `MatchEcho` is called.
- [ADR-036: Reactive Cache Single Owner](036-reactive-cache-single-owner.md) — establishes the `matchEchoFn` dependency-injection pattern.
