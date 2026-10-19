# ADR 2350: Live reads are applied to the Store, and decisions are made from the Store

Status: accepted (2026-10-19). Builds on ADR 2348 (observed-at ordering) and ADR 2344
(deep-fetch apply); amends ADR 2344's closing remark on the dispatch handlers.

## Context

The engine state audit (§3.1–3.4) found decisions that read one source and act on another:

1. `dispatchCandidates` decided on a copy captured before Phase 1/2 and the settle scans of the
   same poll, re-reading only the worker. A Phase 1 cycle-limit pause did not stop a comment pass
   being dispatched in the same poll.
2. Gates that read GitHub live (lock verification, the feedback gate, the linked-PR-terminal and
   "already landed" checks) decided on the read and discarded it, so the next decision fell back
   to the stale cached value.
3. The advance review gate decided on cached requests and reviews, the landing review gate on a
   live read: a reviewer requested after the cache filled was caught only at landing.
4. The dependency check read `blockedBy` live only for an item already marked blocked.

ADR 2348 made it safe to apply a read to the Store: an older observation can no longer revert a
newer fact.

## Decision

1. **One helper family** (`engine/live_read.go`: `liveLabels`, `liveItem`, `livePRMerged`,
   `liveReviews`). Request start first, then the read, then an apply through a mutation carrying an
   authority time. New narrow mutations: `LabelsRead`, `PRReviewsRead`, `LinkedPRStateObserved`
   (State/Merged only, so Title and Draft are never blanked). A failed read is never applied. The
   apply is wrapped in `FromLiveRead` (`OriginLiveRead`), which the wake observer ignores, so a gate's
   own read cannot request a poll. A scan test forbids the raw client calls elsewhere, with an
   audited exception list.
2. **Comments** carry no observed-at record, so a live deep fetch keeps a Store comment the read
   lacks when it was created at or after the read began (matched by node or database ID), rather than
   adding a comment record kind.
3. **Dispatch re-read.** The facts earlier phases can change (labels, Status, closed state,
   comments additively, `blockedBy`) are overlaid from the Store onto the poll-start copy, which is
   what `processItem` receives. A skip records nothing and re-arms the periodic re-evaluation
   (ADR 2096). Overlay, not rebuild: the Store snapshot lacks fields a `ProjectItem` carries.
4. **Cacheless engines** mirror the labels that decide whether an item still needs work, and
   closes and comments, into the Store (`localWriter`). The gate labels are deliberately not
   mirrored at the instant of the write: doing so exposes intermediate label states (`awaiting-ci`
   removed before `awaiting-review` is applied) to the channel deriver that a poll-start listing
   never showed.
5. **Review gates.** The landing gate reads live through `liveReviews`. The advance gate confirms
   live only when its cached view would clear, folding the live read into the cached view so the
   confirmation can only add constraints; a read within `liveFreshFor()` is not repeated.
   `liveFreshFor()` is one poll interval floored at 30 s (zero when no poll interval is configured),
   deliberately not the ten-interval API-cost knob of ADR 1831: a reviewer or blocker added inside
   the window is invisible to a caller that trusts it, so the window stays short.
6. **Dependency check.** `checkDependenciesMode`: Phase 1 is cache-only; `processItem` and the
   stage advance are decision points that read live, skipped while the edge list was observed within
   `liveFreshFor()` (the check's own last live read, or any write that stamped the Store's
   `blockedBy` record such as the #1995 webhook). An already-blocked item is always re-read
   (ADR 1419). Cleanup-stage items are never read.

## Alternatives considered

- *Read live in every per-poll handler.* One GraphQL or REST read per stage-complete item per poll;
  rejected on cost. Reads happen at a transition instead.
- *Replace the advance gate's data with the landing gate's raw live read.* Changes the verdict for
  every cached-only fact (a dismissed or absent review) and breaks the tests that pin the cached
  contract; the union keeps gate policy untouched.
- *Rebuild a full `ProjectItem` from the Store.* Duplicates `copyDeepFieldsFromState` and still
  lacks the linked-PR head and the live markers.
- *Mirror every label for cacheless engines.* Exposed the channel's transient label states.

## Consequences

- A decision made earlier in a poll is respected at dispatch; a live read is visible to the next
  decision with no second call.
- A reviewer requested after the cache filled holds the advance; a blocker added after the cache
  filled stops the dispatch. Items that advanced on cached data before now hold (intended).
- Steady-state reads grow only by one live item read per dispatch and per stage advance (at most one
  per item per `liveFreshFor()` window), and one
  review read pair per clearing advance gate per `liveFreshFor()`.
- A decision-point live read applies the item's fresh body, comments and edges to the Store, which
  heals staleness the #2346 scenarios used to observe; those non-vacuity tests disable the apply
  (`SetLiveReadApplyDisabledForTest`).
- Merge-train, overseer and repair reads stay raw by decision (see the scan test).
- Known gap: the reviewer-requested-after-cache (SC-003) and blocker-added-after-cache (SC-004)
  scenarios are covered by engine-level tests with mock clients, not sim twins. `simgh` was not
  checked for whether a review request or `blockedBy` add bumps `updatedAt`, so those tests show the
  gates' behaviour but not that the sim reproduces the cache staleness itself. Only the dispatch
  re-read (SC-001) has a sim twin (`tests/sim/dispatch_reread_test.go`).

Seams (per-Engine): `SetDispatchRereadDisabledForTest`, `SetLiveReadApplyDisabledForTest`,
`SetReviewConfirmLiveDisabledForTest`, `SetDependencyLiveReadDisabledForTest`,
`SetCachelessWriteThroughDisabledForTest`, `SetLiveFreshForTest`.
