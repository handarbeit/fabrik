# ADR 2344: The deep-fetch apply writes only deep-owned facts

## Status

Accepted. Amends ADR 2180 (the freshness baselines are no longer assigned unconditionally) and closes the "deep-fetch write-back stays unprotected" residual named in ADR 2178, including for a live read.

## Context

Every poll takes a board listing, then deep-fetches the items that need work and applies `ItemDeepFetched{FreshState: board.Items[i]}` to the Store. The deep fetch never reads `Status`, and a cache hit copies none of `Status`, labels or closed state. So `FreshState` carried the listing's values for those, and `applyProjectItem` wrote them unprotected. A webhook move, a label change, or a worker's own `LocalStatusUpdated`/`LocalLabelAdded` landing between the listing and the item's turn was reverted: a spurious `StatusChanged` fired, `LastSeenSourceUpdatedAt` took the stale snapshot's time, and the poll could dispatch on the reverted value. The same pattern sat at `ci_settle.go`, `poll_settle.go`, `dependencies.go` and the cache's own miss path.

Separately, `copyDeepFieldsFromState` and `snapshotToProjectItem` omitted the three linked-PR merge-queue facts, so a cache-served poll left the working item with `IsInMergeQueue = false` and the write-back persisted it. `prInMergeQueue` gates rebase and push, so the engine could push to a PR in GitHub's merge queue and eject it.

## Decision

- **Change the mutation, not each call site.** `ItemDeepFetched` gets its own apply, `applyDeepFetched`. All five call sites (and any found later) are fixed by construction. `applyProjectItem` keeps serving its other producers; the LinkedPR block is shared as `applyLinkedPRFields`.
- **It writes what a deep fetch produces:** title, body, URL, author, assignees, blocked-by, comments, milestone, layer evidence and the LinkedPR block (including the merge-queue facts). `UpdatedAt` only moves forward.
- **Status, labels and closed are first-population only, except a live read.** `Status` is written only while the Store's is empty. Labels are written only while `!LabelsKnown`, closed state only while the Store has no state yet. Otherwise they reach the Store through the listing paths and their ADR 2159/2178 protection. This keeps the probe-seeded minimal item (Status seeded, `LabelsAbsent`) getting its first label population once, which the #2123 baseline and downtime-delta logic rely on.
- **A live fetch still writes labels and closed state, through the engine-write protection.** `gh.ProjectItem.DeepFetchLive` (set by `github.Client.FetchItemDetails` and the sim; cleared by the cache-hit copy and, on the caller's copy, by `Engine.applyDeepFetched` once the result is applied; false on a listing item) says the deep fields were just read from GitHub. A first draft dropped live labels as well. That broke label changes that arrive with no webhook: the probe carries no labels, so the stale-item deep fetch is the only path that heals them. A live response can still predate an engine or webhook write that landed after GitHub produced it, or lag it (read-after-write), so the live label and closed writes run through the rule every listing path shares (ADR 2159/2178: `protectStatusClosed` and `protectLabels`, `DefaultEngineWriteGrace`). The observation time is `gh.ProjectItem.DeepFetchStartedAt`, stamped just before `FetchItemDetails` sends the request, the analogue of `ProjectBoard.FetchedAt` (`deepFetchLiveView`). A record inside the grace window of that instant keeps its local value; a fetch started after it wins; no stamp protects nothing. Status is not read by a deep fetch, so its record is neither kept nor confirmed nor dropped here. No parallel mechanism was added.
- **Freshness baselines (R4).** `LastSeenSourceUpdatedAt` and `LastSeenPRUpdatedAt` are raised only (`After()`), after a live fetch and after a cache hit alike, so a deep-fetch apply never lowers a baseline that `SelfWriteObserved` raised while the fetch was in flight. An earlier draft let a live refetch assign them, because ADR 2180 relied on a real refetch re-anchoring to GitHub's value to cure clock skew; review rejected that exception (R4 as written). The cost: `SelfWriteObserved` anchors the baseline at the daemon's clock, so for as long as it is ahead of GitHub's `updatedAt` for the engine's own write (skew plus the write's latency, normally seconds), an external change inside that window is not detected as stale until a later change moves `updatedAt` past it. The #2180 comment-total rule, which does not use the baseline, still catches new issue comments, and the periodic safety-net re-evaluation and the reconcile bound the rest. ADR 2180's "unconditional assignment" sentence no longer holds.
- **Merge-queue round trip (R3).** `copyDeepFieldsFromState` and `snapshotToProjectItem` carry `LinkedPRIsMergeQueueEnabled`, `LinkedPRIsInMergeQueue` and a cloned `LinkedPRMergeQueueEntry`. A reflection test enumerates `gh.ProjectItem`'s `LinkedPR*` fields and fails on any that a cache-hit copy drops, with an explicit reasoned exemption list.
- **Engines without a board cache.** `NewWithDeps` engines (the sim default) have no probe, reconcile or webhook, so the write-back was the only thing feeding their Store. `Engine.applyDeepFetched` applies the poll-start listing through `ShallowBoardItemUpdated` first when `e.cache() == nil`. Production always has the cache.

## Alternatives considered

- *Migrate each call site to a new field-scoped mutation.* Leaves the old full writer available for the next caller to misuse, and each site needs the same migration.
- *Strip Status/labels/closed from `FreshState` at each call site.* Fragile; the in-hand copy is still used by handlers.
- *Drop live labels and closed too.* Regresses webhook-less label healing (see above).
- *Keep the live labels unprotected (the first implementation).* Reverts an engine write that lands after GitHub produced the response, the stale-listing class ADR 2159/2178 close on the other paths.
- *Let a live refetch assign the baselines.* Lowers a baseline raised mid-poll by a self-write; see Freshness baselines.
- *Make `FetchItemDetails` return a hit/miss flag.* Touches every implementation and mock; a data field on the item does the same with no interface change.

## Consequences

- A change that lands between the listing and the deep-fetch apply survives; no reverting `StatusChanged` or label event fires.
- No deep-fetch apply lowers the freshness baselines, so fewer forced refetches (the #2180 no-refetch-loop guarantee is unchanged); the skew window above is the accepted trade-off.
- A live deep fetch no longer reverts an engine label or close recorded inside the grace window of its start.
- The "left the merge queue" edge (`priorInQueue`) now fires correctly on cache-served polls; before, it was silently lost.
- The dispatch handlers still run on the poll-start `board.Items[i]` copy, which can hold the old Status (audit §3.1). This ADR fixes what the Store holds, not that copy.
  *Amended by ADR 2350:* the dispatch decision (and the item `processItem` receives) is now re-read from the Store immediately before dispatch (`refreshDispatchItem`); the Phase 1/2 handlers still run on the poll-start copy.
- The pre-#2344 apply remains behind `Store.SetLegacyDeepFetchApplyForTest` / `Engine.SetLegacyDeepFetchApplyForTest` so the tests can show they are not vacuous.
