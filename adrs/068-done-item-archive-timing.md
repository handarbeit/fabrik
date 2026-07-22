# ADR 068: Done-Item Archive Timing

**Date**: 2026-07-21
**Status**: Accepted
**Issue**: #1035 — rework Done-item archive timing and re-enable board auto-archival

## Context

Fabrik previously had an auto-archive path, `archiveDoneCompleteItems`, that archived board items sitting in the Done (cleanup) column with their completion label. It was deliberately disabled — its call site commented out — because it archived items *immediately*, before a human had any chance to see the work land in Done: completed work appeared to vanish the instant it finished. Code-health issue #1025 later removed the dead function entirely (deletions-only scope), leaving `ArchiveProjectItem` (`github/status.go`) implemented and declared on `GitHubClient` but unused, and `engine/process_item_test.go` asserting it is *not* called inline from `processItem`.

ADR-064 introduced `settleClosedItemsToDone`, which advances closed items stranded outside Done back into it, but explicitly left the archival gap open — items reach Done, but nothing removes them from the board afterward. Un-archived Done and closed items accumulate indefinitely; every one of them is fetched on each poll cycle, inflating the per-poll GraphQL cost across the shared-budget instance fleet.

This is the third attempt at this feature, and the first two both failed for opposite reasons:

1. **The original `archiveDoneCompleteItems`** archived on first observation — no grace period at all. This is the "work vanishes instantly" bug that got it disabled.
2. **Issues #687/#688** ("re-enable Done-stage auto-archive with completion-anchored timing") implemented a grace period using a new in-memory `itemstate` field, `DoneCompletedAt`, set once when the completion label was first observed, with a fallback to `item.UpdatedAt` for legacy items or after an engine restart (since `itemstate.ItemState` has no persistence layer — everything resets on restart). That PR fully passed all stages and 12 packages with `-race`, but was never merged: it was closed as an OSS-prep git-history-rewrite casualty, not a design rejection. Its restart-fallback path has a real, separate correctness problem: `item.UpdatedAt` is a coalesced max of issue/item/linked-PR updates, bumped by activity unrelated to the Done transition. Falling back to it after a restart could reintroduce exactly the premature-archival risk the grace period exists to prevent — GitHub's API makes no guarantee that `updatedAt` tracks "time since Done" in the general case.

Any third attempt needs to reason explicitly through both failure modes rather than risk repeating one of them.

No GitHub GraphQL field exposes "when did Status become Done" directly — `ProjectV2Item.updatedAt` is the only timestamp on a project item and is unusable for the reason above. `GitHubClient.FetchLabelAppliedAt(owner, repo, issueNumber, labelName)` (`github/labels.go`) is an established alternative: a GitHub-side, restart-safe REST lookup (pages through issue events, returns the most recent `labeled` event's timestamp, or the zero time with no error if not found) already backing three production "wait N since label applied" gates — the CI-wait timeout (`engine/ci.go`), the review-wait timeout (`engine/reviews.go`), and the auto-merge convergence budget (`engine/merge_gate.go`).

The complication `FetchLabelAppliedAt` introduces here is cost, not correctness: it pages through the full issue-events REST history, and the three existing call sites only invoke it on items already being deep-fetched/dispatched, over short wait windows (15–90 minutes). An archive scan is different — it must run against a 168h (1 week) grace window, unconditionally, against items that are deliberately *not* deep-fetched (that's the entire "Done items are cheap to skip" invariant this issue exists to preserve, protected by ADR-021's housekeeping-mutation exemption test). Calling it once per poll for every waiting Done item would trade the GraphQL burn this issue is meant to fix for an equivalent REST burn.

## Decision

Re-implement Done-item archival as `settleArchiveDoneItems` (`engine/archive_done_settle.go`), a new unconditional per-poll settle scan, structurally identical to `settleClosedItemsToDone` (ADR-064): sourced from `board.Items` directly (not `deepFetchCandidates`), so already-worktree-reaped Done items — which never pass `itemMayNeedWork`'s admission guard again — remain reachable.

**Eligibility** mirrors the original `archiveDoneCompleteItems` predicate exactly: `item.Status == cleanup.Name && hasLabel(item.Labels, "stage:<cleanup.Name>:complete")`, where `cleanup := cleanupStage(e.cfg)`. Nothing else is checked — no `IsClosed`, no other labels — matching this scan's single, narrow responsibility.

**Timing source: `FetchLabelAppliedAt`, not a new `itemstate` field.** This is chosen deliberately over the #687/#688 approach precisely because its restart-fallback path (`item.UpdatedAt`) reintroduces the premature-archival risk described above. `FetchLabelAppliedAt` is GitHub-side and restart-safe by construction — there is no fallback path to get wrong, because there is no local state to lose.

**Bounding the REST cost: cache the *computed* eligible-at time, not the raw label-applied-at, in `itemstate.CooldownAt`.** `archiveEligibleAt` first checks `snap.CooldownAt("archive-eligible-at")`; on a cache hit, every subsequent poll is a pure `time.Now()` comparison — no REST call. On a miss, it calls `FetchLabelAppliedAt` exactly once, computes `appliedAt + ArchiveAfter`, and caches that result via `itemstate.CooldownRecorded`. This bounds `FetchLabelAppliedAt` to once per item per engine lifetime, and at most once more across a restart (since `itemstate.Store` is in-memory only) — an explicit, bounded, acceptable cost, not an open-ended one.

`FetchLabelAppliedAt`'s fail-open contract (zero time + nil error when the label-event is not found) is preserved as "not yet known": neither an eligible-at time nor a cache entry is written, so the next poll retries. This directly satisfies requirement 7 — an unknown timestamp can never be treated as "elapsed," so it can never cause early archival, only a delayed one.

**On expiry:** call `ArchiveProjectItem(board.ProjectID, item.ItemID)`, then write through the cache via a new `CacheImpl.RemoveItem(itemID string)` (wrapping `store.RemoveByItemID`, mirroring the webhook delta path's `"deleted"/"archived"` case exactly), then register a webhook echo (`"projects_v2_item"`/`"archived"`, not `"edited"` — matching the delta handler's actual case label). A `logf` line identifies the archived issue (requirement 8).

**Configuration:** `FABRIK_ARCHIVE_AFTER` (Go duration string, default `168h` — one week; Go's duration parser has no day/week unit, so the literal value is `168h`, not `7d`/`1w`) and `FABRIK_ARCHIVE_DONE` (`on`/`off`, default `on`) are orthogonal knobs — `ArchiveAfter=0` is a legal "archive immediately once eligible" value, not a disable sentinel; disabling is exclusively `ArchiveDone=off`. `FABRIK_ARCHIVE_AFTER` uses Go duration syntax (`resolveDuration` + a dedicated validating helper, `archiveAfter`), following `FABRIK_CONVERGENCE_BUDGET`/`FABRIK_KILL_GRACE_*` rather than the bare-integer `FABRIK_RECONCILE_INTERVAL`/`FABRIK_JANITOR_INTERVAL` family, since "168h" is self-documenting where "604800" is not. The week-long default favors keeping completed work glanceable on the board over minimizing board size — a deliberately conservative choice given the original implementation's failure mode was archiving too early, not too late. `FABRIK_ARCHIVE_DONE` defaults to `"on"` (unlike `MergeTrain`'s `"off"` default) because this issue's entire point is re-enabling the capability, not gating an experimental one.

**No durable marker beyond the `CooldownAt` cache.** Archival itself is terminal and self-idempotent: an archived item stops appearing in default board queries, so there is nothing left to re-observe. The `CooldownAt` cache is a pure cost-bounding optimization, not a correctness-load-bearing marker — losing it (e.g., on restart) costs exactly one extra `FetchLabelAppliedAt` call, never an incorrect archival, satisfying requirement 7's "one extra wait cycle is acceptable; early or duplicate archival is not."

**Shallow-label truncation is an accepted residual risk, not mitigated.** The board query's `labels(first: 30)` may omit the completion label for items with 30+ labels, silently skipping their archival indefinitely. ADR-021 already accepted this exact trade-off for the original implementation; mitigating it here would require a deep fetch, which would violate requirement 9 and reintroduce the per-poll GraphQL cost this feature exists to eliminate.

## Rationale

### Why not the #687/#688 `DoneCompletedAt` design, given it already shipped a full Implement→Validate cycle?

Because its restart-fallback to `item.UpdatedAt` is a correctness gap, not a style preference. `updatedAt` is a coalesced timestamp with no contractual relationship to "time since this item's Status became Done" — it can be recent because of unrelated issue activity, or old because GitHub simply hasn't touched the field. Reviving that design would carry the same latent premature-archival risk that motivated disabling the very first implementation, just moved behind a restart instead of behind first observation. `FetchLabelAppliedAt` removes the fallback path entirely by removing the need for one: the timestamp always comes from GitHub, never from engine-local state that can be lost.

### Why cache the computed eligible-at time instead of the raw `FetchLabelAppliedAt` result?

Caching the raw result would still require a `time.Now() >= cachedAppliedAt + ArchiveAfter` comparison against the *current* `ArchiveAfter` setting on every poll — cheap, but functionally equivalent to caching the sum once. Caching the sum is simpler and, as a minor side benefit, freezes the effective grace period for an already-observed item against a mid-flight `FABRIK_ARCHIVE_AFTER` config change, which is an acceptable (and arguably more predictable) behavior for an operator-facing timing knob.

### Why is a bare `CooldownAt` cache sufficient, without ADR-060/061/062's marker/retry/escalation machinery?

That machinery exists to protect multi-step sequences where losing track mid-sequence on restart could resurrect the underlying bug (per ADR-064's own rationale for *not* needing it either). Archival here is a single, self-contained `ArchiveProjectItem` call whose precondition (`item.Status == cleanup.Name && hasLabel(...)`) is durable GitHub board state re-derived every poll, exactly like ADR-064's advance-to-Done scan. A failed archive call is simply retried next poll — logged as a warning, nothing more. The `CooldownAt` entry is a cache, not a decision record; its loss is a bounded cost (one extra REST call), not a stuck state.

## Consequences

**Positive:**
- Board bloat and the per-poll GraphQL cost of fetching it now have an actual upper bound: a Done item is removed from the board 168h/1 week (default, configurable) after its work is visibly complete.
- The "work vanishes instantly" UX regression that got the original implementation disabled cannot recur — the grace period is anchored to a GitHub-side timestamp with no local-state restart-fallback path that could shorten it unpredictably.
- `ArchiveProjectItem`, dormant since before this codebase's public history, finally has a call site.
- `CacheImpl.RemoveItem` reuses the webhook delta path's existing `"deleted"/"archived"` semantics exactly, so cache consistency after archival requires no new invariant.

**Negative / Trade-offs:**
- **`ArchiveProjectItem` has never actually run in production** — the original call site was removed before shipping broadly, and #687/#688 never merged. Its doc comment asserts GitHub-side idempotency, but this is the first real exercise of that call; an archive-related bug report should be treated as high priority post-ship.
- **Shallow-label truncation** (`labels(first: 30)`) could silently skip archival for an eligible item with 30+ labels indefinitely — accepted per ADR-021's original trade-off, out of scope for this issue.
- **One extra `FetchLabelAppliedAt` REST call per item per engine restart.** Bounded and explicit, not a design flaw, but worth noting as the cost of the restart-safety guarantee.

## Related Work

- [ADR-021: Housekeeping Mutations on Shallow Data](021-housekeeping-mutations-on-shallow-data.md) — establishes the idempotent/terminal/non-advancing exemption test this scan satisfies, and originally named `archiveDoneCompleteItems` as a canonical example; also the source of the shallow-label-truncation trade-off accepted here.
- [ADR-064: Closed-Item-At-Any-Stage Advance To Done](064-closed-item-any-stage-advance-to-done.md) — the immediate predecessor settle-owner and structural template this scan follows; explicitly deferred the archival gap to this issue.

**References:** [docs/state-machine.md §6.12](../docs/state-machine.md)
