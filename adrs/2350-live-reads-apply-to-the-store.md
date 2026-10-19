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
   own read cannot request a poll. The cycleSet observer does not ignore it: the item is evaluated
   once more on the next poll (a no-change read raises no flags, so this is one extra evaluation,
   never a loop). Channel events built from a live-read change are marked unverified like a
   reconcile's or a probe's (`unattributed()`), since no known write or webhook backs them. A scan test forbids the raw client calls elsewhere; each remaining raw call
   is audited per call site with a `// live-read-audited: <reason>` marker (only `live_read.go` is
   exempt as a file), so a new raw read in a file that already has an audited one still fails. The
   scan matches the method on any receiver chain ending in `client` or `readClient` (`e.client`,
   `e.readClient`, a local `client`, ...), not only `e.client`; the board-cache-backed
   `readClient.FetchItemDetails` admission reads are audited as such (the cache writes a miss back
   to the Store itself), and the dispatcher-view read in `dispatcherGap` is deliberately not applied.
2. **Comments** carry no observed-at record, so a live deep fetch keeps a Store comment the read
   lacks when it was created at or after the read began (matched by node or database ID), rather than
   adding a comment record kind.
3. **Dispatch re-read.** The facts earlier phases can change (labels, Status, closed state,
   comments additively, `blockedBy`) are overlaid from the Store onto the poll-start copy, which is
   what `processItem` receives. A skip records nothing and re-arms the periodic re-evaluation
   (ADR 2096). Overlay, not rebuild: the Store snapshot lacks fields a `ProjectItem` carries.
4. **Cacheless engines** mirror the labels that decide whether an item still needs work, and
   closes and comments, into the Store (`localWriter`), plus the REMOVAL of a stage completion
   label (`stage:<name>:complete`), so a phase that strips it (e.g. SHA invalidation) is seen by the
   dispatch re-read (FR-003). The gate labels, and the ADD of a completion label, are deliberately
   not mirrored at the instant of the write: doing so exposes intermediate label states
   (`awaiting-ci` removed before `awaiting-review` is applied) to the channel deriver that a
   poll-start listing never showed. `settlePRMergeState`'s terminal merged-state confirmation goes
   through `livePRMerged`, so the merge it confirms is applied to the Store like every other one.
5. **Review gates.** The landing gate reads live through `liveReviews`. The advance gate confirms
   live only when its cached view would clear, and decides on that live read alone (reviews and
   requests), never on a union with the cached copy: a review dismissed or removed on GitHub no
   longer counts, so the advance and the landing gate (`reviewGateHold`, also live alone) agree on
   identical PR state (FR-007). Within `liveFreshFor()` the Store's view after the last applied read
   is used instead of repeating it. A failed read leaves the cached verdict.
   `liveFreshFor()` is one poll interval floored at 30 s (zero when no poll interval is configured),
   deliberately not the ten-interval API-cost knob of ADR 1831: a reviewer or blocker added inside
   the window is invisible to a caller that trusts it, so the window stays short.
6. **Dependency check.** `checkDependenciesMode`: Phase 1 is cache-only; `processItem` and the
   stage advance are decision points that read live, skipped while the edge list was observed within
   `liveFreshFor()` (any applied `liveItem` read of the item, stamped on the engine clock, or a write that
   stamped the Store's `blockedBy` record such as the #1995 webhook). An already-blocked item is always re-read
   (ADR 1419). Cleanup-stage items are never read.

**Ordering of the new reads.** `PRReviewsRead` is judged by the shifted authority time (fetch start
minus the grace), as ADR 2348 specifies for a PR read, both for the whole-list check and per author:
a webhook review is stamped at whole-second resolution (and read replicas lag), so a review submitted
just after the read began can carry a time before its start, and a read that predates the review must
not wipe it. **Deviation from ADR 2348:** `LinkedPRStateObserved` is judged by the raw request start.
It applies only `merged=true`, a merge is monotonic, and the single-PR GET does not lag, so a record
from the previous grace window must not drop a merge just confirmed; the baseline it records is still
the shifted lower bound. (ADR 2348 carries a matching amendment line.) Because that baseline is
the shifted one, a listing read that starts shortly *after* the confirmation (authority `T-25s` against
a baseline of `T-30s`) is not stale to the ordering rule, yet the list endpoint can trail a merge by
longer than the grace window. So `PRDetailsUpdated` makes a recorded merge monotonic for the same PR
number: an open/unmerged write for PR N never clears `Merged`/`State` of a PR the Store holds as
merged (the rest of the read, Title and Draft, still applies); a different PR number replaces the
linked PR as before. Recording the unshifted start instead would only narrow the window, not close it.

**Deciding from the Store at the feedback and landing re-reads (item 5).** After `liveItem`,
`feedbackGateBlocks` (`unprocessedFeedback`) and `attemptMergeOnValidate` decide on the read overlaid
with the Store's facts (`overlayStoreFacts`, the same comment/`blockedBy` overlay `refreshDispatchItem`
uses): the Store keeps a comment recorded after the read began (a webhook delivery mid-read) that the
raw copy lacks. Comments are added by ID, `blockedBy` comes from the Store once it has been deep
fetched, and labels are **add-only**: a label the Store holds that the read lacks is added, but a label
the read shows is never hidden. That is a deliberate deviation from "overlay the Store's labels": the
Store's merged view protects an engine write made inside the grace window, which would hide a pause a
human re-applied right after an engine unpause from the fail-closed pause guard that runs on this copy
(see `liveLabels`). Seam: `SetLiveOverlayDisabledForTest`.

## Alternatives considered

- *Read live in every per-poll handler.* One GraphQL or REST read per stage-complete item per poll;
  rejected on cost. Reads happen at a transition instead.
- *Replace the advance gate's data with the landing gate's raw live read.* Changes the verdict for
  every cached-only fact, and the existing review-gate tests that model "the cache holds reviews" with
  an empty live read had to be updated to model GitHub holding them too (`mirrorLiveReviews`). The
  confirmation nevertheless decides on the live read alone (a cached-and-live union let a dismissed
  review keep counting at the advance while the landing gate held, which broke FR-007).
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
`SetCachelessWriteThroughDisabledForTest`, `SetLiveFreshForTest`, `SetLiveOverlayDisabledForTest`.
