# ADR 036 — Reactive Cache with Single State Owner

**Status:** Proposed (2026-05-04)
**Supersedes (in part):** ADR 034 (Boardcache event-sourced delta), ADR 035 (Four-layer status reconciliation) — these defined the previous design that this ADR replaces.

## Context

Between 2026-05-02 and 2026-05-03, fabrik shipped a substantial set of features — board-cache (#452/#455 + follow-ups), webhook-driven event delivery, four-layer status reconciliation (per ADR 035) — intended to dramatically reduce GraphQL load by serving most reads from an in-memory cache fed by webhook deltas.

In production use over the following day, these features caused a cascade of cache-coherency failures:

- Stale Status causing advance-loops on issues #501 and #506 (fabrik repeatedly tried to advance a stage that had already advanced on GitHub but not in cache).
- "Fabrik went deaf" — newly opened issues invisible to the engine because `applyIssuesDelta` did not handle `action == "opened"` and the cache was the only path for reads.
- Issue #467 column-move propagating with up to 76-min latency.
- A regression in #488's deep-fetch-loop fix (#504): the cooldown was over-broadly refreshed, blocking incomplete-stage retries indefinitely.
- Stale `fabrik:locked:<user>` labels persisting after worker crashes, with no automated recovery path.

Investigation (documented in `docs/cache-refactor/01-state-inventory.md`) identified the structural cause: fabrik was already maintaining substantial in-memory state in the Engine struct (8+ maps including `processedSet`, `lastUpdatedAt`, `retryCount`, `lockedIssues`, etc.) before the boardcache feature was added. The cache feature was bolted on alongside this existing state rather than consolidating it. Result: 25+ separate state structures, with mutations through one path failing to propagate to the others.

The fix is not another patch. The fix is to consolidate engine state into a single owner.

## Decision

Introduce a new `internal/itemstate` package with:

1. A canonical `ItemState` struct that consolidates all per-item state currently spread across the boardcache and the engine.
2. A `Store` type that owns all `ItemState` instances. All mutations flow through `Store.Apply(Mutation)`. All reads flow through `Store.Get` returning an immutable `Snapshot`.
3. A `Mutation` interface — a discriminated union of every possible state change (webhook deltas, self-mutations, periodic reconcile updates, engine-internal counter changes).
4. A `Subscribe`/`Observer` mechanism so downstream code can react to changes rather than poll for them.

The migration is staged across 8 incremental PRs (Phase 3-A through 3-H, documented in `docs/cache-refactor/02-design.md` §5), each independently shipping value:

- 3-A: Skeleton package, not yet wired in.
- 3-B: Boardcache delegates to Store internally — behaviour-equivalent, no engine changes.
- 3-C: Self-mutation write-through — fixes #501/#506 advance-loop class.
- 3-D: Webhook delta complete coverage — fixes "fabrik went deaf" class.
- 3-E: Engine state consolidation — moves per-item Engine maps into ItemState.
- 3-F: Stage-state consolidation — moves stage-keyed maps; **splits `processedSet`** into separate retry-cooldown and re-eval-cooldown fields, structurally fixing #504.
- 3-G: Worker handle — heartbeat-based liveness; fixes stale-lock recovery gap.
- 3-H: Reactive observer plumbing — wakeCh, TUI events become observers.

Then Phase 4 audits all downstream readers and Phase 5 lands per-reader fixes. Phase 5 F3 (issue #537) completed the single-Store intent by unifying `engine.store` and `cacheImpl.store` into one shared instance (see addendum below).

## Consequences

### Positive

- **Single source of truth per item.** Bugs of the form "mutation path A updated structure X but forgot structure Y" become structurally impossible: there is one X.
- **Inbound and outbound coherence checked structurally.** Both webhook deltas and self-mutations flow through the same `Apply` call, so the cache cannot diverge from operations the engine performed.
- **Reactive downstream readers.** Observer pattern eliminates the redundant per-poll re-evaluation of `itemMayNeedWork`/`itemNeedsWork` for items that have not changed.
- **Tests have a clear surface.** Invariants (I1-I10 in design doc §6) test the Store directly, not the entanglement of 25 maps.
- **Crash recovery.** Worker handles with heartbeats let the engine detect dead workers and recover without manual label cleanup.
- **Persistence boundary clear.** Store is in-memory; persistence (across restarts) is achieved by re-bootstrapping from GitHub. No half-persistent state to reason about.

### Negative

- **Substantial code change.** ~25 state structures redirect to a single new package. Every read site and write site touches.
- **Migration risk.** Each Phase 3 PR has potential for regression. Incremental landing strategy mitigates this; thorough tests per PR (per the build-issue spec) further mitigate.
- **Performance.** A Store under a mutex serializes all writes to all items. Profile during 3-A skeleton phase. If contention becomes a problem, shard by repo or by item-key.

### Neutral

- The boardcache package remains, but its internals become a thin adapter over Store. ADR 034 describes the previous boardcache architecture; this ADR supersedes it for the *internal structure*. The external API (`ReadClient` interface) is preserved.

## Alternatives considered

### Alternative 1: Continue patching individual bugs

Bugs are found, issues filed, PRs land. No refactor.

**Rejected.** The structural pattern (mutation paths forget to update co-dependent state) means the bugs come from a category, not a count. Patches address one site at a time; the next time a developer adds a new event type or a new mutation, they hit the same class of bug. Validated empirically: every cache bug filed on 2026-05-03/04 fits the pattern. The whack-a-mole is open-ended.

### Alternative 2: Revert the cache entirely; return to direct GitHub queries

Remove `boardcache` and webhook-delta machinery; engine talks directly to GitHub on every read.

**Rejected.** Loses the GraphQL load reduction the cache was introduced to achieve. The cache is *fine in principle*; the implementation is fragmented. Reverting throws away too much, and would lose the work invested in webhook subscription, delta handlers, etc. (Most of which is correct in isolation; just not coherent in aggregate.)

### Alternative 3: Multiple narrow fixes (e.g. just consolidate processedSet; just add `opened` action)

Fix the most painful bugs without restructuring.

**Rejected.** This is what we were doing on 2026-05-03; we filed 7+ issues in a day and they covered maybe a third of the structural-fragility surface. The user's framing — "we've lost cache coherency" — captured that this is not isolated bugs.

### Alternative 4: Event-sourcing / CQRS architecture

Treat all state as derived from an append-only log of events. Replay log to reconstruct state; never mutate.

**Rejected for now.** Heavier than fabrik needs. Persistence concerns multiply (where does the log live, how is it bounded, what happens to it across restarts). The Store/Mutation design captures the most useful properties of CQRS (single mutator path, observers see all changes) without the persistence and replay overhead. Worth revisiting if fabrik later needs cross-instance coordination or audit-quality history.

## Implementation notes

The migration PRs (Phase 3-A through 3-H) are tracked as separate fabrik issues, each with thorough test requirements. The order is chosen to land bug-fix value early:

- 3-C (self-mutation write-through) and 3-D (webhook complete coverage) ship before the bigger structural moves (3-E onward) so the operator-visible bugs are fixed first.
- Each PR includes test coverage for at least one of the design invariants (I1-I10).
- State-machine.md is updated as part of every PR that changes engine state semantics, per existing repo convention.

## Status / progress

- Phase 1 inventory: complete (`docs/cache-refactor/01-state-inventory.md`).
- Phase 2 design: complete (`docs/cache-refactor/02-design.md`).
- Phase 3-A through 3-H: filed as fabrik issues; pipeline executing.
- Phase 4 audit: complete (`docs/cache-refactor/04-phase4-audit.md`).
- Phase 5 F3 (store unification): complete (issue #537, PR #538). See addendum below.

## Addendum — Phase 5 F3: Store Unification (2026-05-05)

### Deviation that landed in Phase 3

The Phase 3 implementation deviated from this ADR's single-Store intent: both the
Engine and the CacheImpl independently constructed their own `*itemstate.Store`
instance via `itemstate.NewStore(nil)`. Engine mutations (lock acquire/release,
CI gate, invocation outcomes) went to `engine.store`; webhook delta and reconcile
mutations went to `cacheImpl.store`. Field reads from one store were invisible to
the other.

The Phase 4 audit documented this deviation in `docs/cache-refactor/04-phase4-audit.md`
§1.3, and identified two concrete foot-guns in §3.5–§3.6:

- `engine/worker_liveness.go`: `snap.Labels()` read from `engine.store` silently
  returned an empty slice because webhook-delivered labels lived only in `cacheImpl.store`.
- `engine/ci.go`: `snap.LinkedPR()` was available for `HasHadChecks` (engine-applied
  mutation) but not for `Number`, `Mergeable`, or `Reviews` (webhook-applied mutations).

Any future reader querying `e.store.Get(...).Item().Status` would silently receive
`""` instead of the actual GitHub Status, with no compile-time or runtime warning.

### Resolution

Issue #537 completed the unification. `boardcache.NewCacheImpl` now accepts an
`*itemstate.Store` parameter (panics on nil) rather than constructing its own.
`engine/engine.go:New()` creates exactly one shared store, assigns it to `eng.store`,
and passes it to `NewCacheImpl`. All mutations — engine-side and webhook/reconcile-side
— flow through one `Apply` call on the shared instance.

Observer registration in `engine/poll.go` was consolidated: `wakeObs` and `mwnObs`
now register once on the shared store (the prior double-registration via both
`e.store.Subscribe` and `cacheImpl.Subscribe` was a latent double-fire bug that the
unification also resolves). `stageObs` migrated from `cacheImpl.Subscribe` to
`e.store.Subscribe`.

### Rationale

A single source of truth per item was the core design goal of this ADR. The split
created a class of silent-read-from-wrong-store bugs structurally identical to the
mutation-path fragmentation that motivated the refactor in the first place. The
foot-guns identified in Phase 4 make the split untenable: the `worker_liveness.go`
label read was already silently broken on any board running in in-memory cache mode.
Unification restores the structural guarantee the ADR intended.

## References

- `docs/cache-refactor/01-state-inventory.md` — inventory of every existing state structure with read/write sites and known bugs.
- `docs/cache-refactor/02-design.md` — full design including data model, contracts, and migration strategy.
- ADR 034 (boardcache event-sourced delta): the previous design this ADR supersedes for internal structure.
- ADR 035 (four-layer status reconciliation): the layer model whose Layer 0 (write-through) gap motivated this work.
- Issues filed during the 2026-05-03/04 cluster: #467, #501, #504, #506, plus the Specify-stage cluster on cache coherency.
