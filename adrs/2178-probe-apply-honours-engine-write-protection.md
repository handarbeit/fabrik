# ADR 2178: The probe apply honours engine-write protection

## Status

Accepted

## Context

ADR 2159 made the engine's own recent writes (a Status move, an issue close, a label change) survive a stale board listing, but only on the light-reconcile path (`ShallowBoardItemUpdated` → `applyShallowItem` → `protectShallow`). The probe loop applies listings through a separate path, `ProbeBoardItemUpdated` → `applyProbeItem`, which carried no fetch time and never consulted `EngineWriteAt`. GitHub's board query lags writes, so a probe listing fetched just before an engine write could arrive just after it and restore the old `Status` or open state, set `StatusChanged`/`StateChanged` and reset `StatusEnteredAt`. A reverted Status can re-dispatch a stage that just completed; a reverted close can re-admit a landed item. ADR 2159 recorded this as a residual.

## Decision

- **One rule, split by field group.** `protectShallow` is split into `protectStatusClosed` (expiry drops, `Status`, closed state and their confirm logic) and `protectLabels` (the label merge and label-record confirmation); `protectShallow` composes them with unchanged behaviour. The probe calls only `protectStatusClosed`. There is one rule and one `DefaultEngineWriteGrace`. A probe listing carries no labels, so running the label half would read "absent from the listing" as confirming an engine label *removal*, dropping the record and unshielding a later lagging reconcile listing; calling only the Status/closed half removes that hazard by construction rather than by a flag.
- **Fetch time on the mutation.** `ProbeBoardItemUpdated.FetchedAt` mirrors `ShallowBoardItemUpdated.FetchedAt`, so the `GitHubClient` interface and its several implementers (`github.Client`, `simgh`, mocks) are untouched. Zero means no protection, so existing callers and tests are unchanged. `runProbeAndDeepFetch` stamps `time.Now()` immediately before `ProbeProjectBoard` — wall clock, because the Store stamps `EngineWriteAt` with `time.Now()` and `SetClock` only moves `e.now()`; taken before the query, so a slow response is never mistaken for a fresh one (the probe is paginated and retried inside the call, so an earlier stamp only means more protection).
- **The decision is made in the Store under its mutex**, inside `applyProbeItem`, so a write landing between the fetch and the apply is honoured. A kept value is never assigned: no flag, no channel event, no `StatusEnteredAt` reset. `FromProbe` wrapping (ADR 2080) is unchanged.
- **Confirm and drop.** A probe fetched at or after W that agrees with the cache drops the `status`/`closed` record, as the reconcile apply does.
- **The probe loop reads the protected view too.** `Store.EffectiveProbe` returns the Status/closed state the apply will leave, and the loop uses it before the apply for the stage-configuration check, the terminal-flag clear and the probe-only-terminal check, so a stale listing cannot clear the terminal flag while the apply keeps the cached Status.
- **Deep-fetch seeding.** The stale-item deep fetch was seeded from the probe's `Status`/`IsClosed`, and `ItemDeepFetched` → `applyProjectItem` writes them unprotected, which would re-revert the Status immediately after the apply kept it. The seed now comes from the post-apply Store, which equals the probe whenever no engine write is active. This stays inside the probe loop and does not touch the deep-fetch *label* write-back residual. The seed is effective for `Status` (the deep fetch never sets it) and for PR cards; for an issue card `github.Client.FetchItemDetails` overwrites `IsClosed` from the live issue node (`node.Closed`), so the deep-fetch write-back of the closed state is not protected. That read is of the issue itself, not of the lagging ProjectV2 board listing this ADR protects against, and protecting `ItemDeepFetched` is the deep-fetch residual below, deliberately out of scope.
- **`UpdatedAt` is unchanged.** The probe still assigns `EffectiveUpdatedAt`. A lagging listing can move it backwards, which only makes the item look no fresher than it was; holding it would change deep-fetch staleness semantics and is out of scope.

## Alternatives considered

- A `labelsKnown` flag on `protectShallow`: leaves the label-confirm hazard one wrong argument away.
- A second copy of the rule in `applyProbeItem`: two rules that can drift.
- An engine-side pre-read of the write record: racy against concurrent engine writes; the Store decides under its lock.
- A new configuration knob for the grace: not needed.

## Consequences

- After an engine Status move or close, no probe listing fetched before write + grace changes the cached value. A human move inside the window is applied one probe (one poll) later.
- The live sim cannot reproduce listing lag (`simgh` builds probe items fresh), so coverage is `internal/itemstate` unit tests and an engine test with a mock client.
- Remaining residuals from ADR 2159: writes that bypass the cache write-through and the deep-fetch write-back (labels, and the closed state of an issue card, which the deep fetch re-reads live).
