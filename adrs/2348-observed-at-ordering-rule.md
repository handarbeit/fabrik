# ADR 2348: One observed-at ordering rule for every write into the item Store

**Date**: 2026-10-18
**Status**: Accepted
**Issue**: #2348 — statefix: one observed-at ordering rule for every write into the item Store
**Supersedes**: the engine-write-only part of ADR 2159 (its protection becomes the engine-write special case of this rule). Notes on ADR 2178 and ADR 2347 below.

## Context

The item Store is fed by listings (the per-poll probe, `LightReconcile`, `Reconcile`, the Layer 1 and Layer 2 status reads), deep fetches and REST/GraphQL PR reads, webhook deliveries and the engine's own write-throughs. Since #2159/#2178 exactly one ordering rule existed: an engine write is remembered with its time, and a listing fetched before that write plus a 30 s grace cannot overwrite it — for Status, labels and closed, on the listing paths only. Every other producer overwrote without asking when its value was observed (engine state audit 2026-10 §2.1–§2.6, §2.8):

- a human's move or label change applied by webhook was reverted by the next probe or reconcile listing, and `LightReconcile` read the newer webhook value as drift;
- webhook deliveries are handled concurrently and unordered, so a late echo of an engine write re-added a removed label, which the label protection then shielded as if the engine had written it;
- eight writers set the head SHA with no ordering, and a regression wipes check runs and the CI verdict;
- `issues.opened` and `projects_v2_item.created` were applied as complete items although they carry no Status, item ID, comments or update time;
- `Reconcile` removed cached items the listing had not caught up with;
- a deep fetch or PR read issued before a concurrent write reverted it when applied;
- review lists differed by producer (webhooks kept every review, a deep fetch the latest per author).

This had to land before a second webhook source (#2143), and #2383 made real `projects_v2_item` deliveries reach the Store, so the gap was live.

## Decision

**One rule, decided inside the Store under its lock** (`Store.applyToItem` and its apply helpers — never in `boardcache` by reading first, which races concurrent handlers):

1. **Authority time on one axis.** An engine write: its write time. A listing, probe, status refresh/sweep, live deep fetch or PR read: the instant its request began minus the listing-lag allowance (`DefaultEngineWriteGrace`, 30 s). A webhook: the payload's GitHub time (`issue.updated_at`, `pull_request.updated_at`, `projects_v2_item.updated_at`, review `submitted_at`), else its receipt time (`boardcache.observedTime`). Zero means no ordering information: the write applies and records nothing.
2. **Per-fact record.** `ItemState.ObservedAt` holds, for `status`, `closed`, `label:<name>`, `labels` (full-set baseline), `head`, `linkedpr`, `reviews` (whole-list reads), `reviews:<author>` (webhook review events, a revision's receipt time included), `reviews:submitted:<author>` (`submitted` events only, ordered by `submitted_at`), `milestone` and `blockedBy`, the authority time of the last applied observation. A fact's recorded time is the later of `ObservedAt` and `EngineWriteAt`; engine writes stamp both. Memory only.
3. **Drop when older.** A write strictly older than the recorded time of its fact is dropped, silently (no `Change`, wake, recorded time or attribution). Ties apply; a first observation applies. Granularity is per fact and per label. A webhook's second-resolution GitHub time is compared against the record truncated to the second, so a change in the same second as an engine write applies; the engine's own echo is dropped by the echo rule. A `check_run`-driven head heal (`FillOnly`) writes the head only while none is recorded.
4. **Engine-write protection is the special case.** A listing's authority is `fetchStart − grace`, so "authority < W" is exactly #2159's "fetched before W + grace". `EngineWriteAt`, its confirm-and-drop logic, `EngineWriteLabelKey` (shutdown cleanup) and `SetEngineWriteGraceForTest` are kept unchanged; `protectStatusClosed`/`protectLabels`/`protectShallow` read the later of the two maps, and drift detection (`EffectiveShallow`/`EffectiveProbe`) uses the same helpers, so a kept webhook value is not drift. The #2159/#2178/#2180 tests pass unmodified.
5. **Partial payloads write only what they carry** (`IssueOpened.Partial`): an unpopulated item gets the payload's identity, state, assignees and labels; a populated item only gets labels the payload adds and a milestone, ordered against their records; Status, item ID, comments, `UpdatedAt`, `BlockedBy` and the `LinkedPR` block are never written, and an opened payload never removes a label. A head write with an empty SHA is link-only.
6. **Head ordered before side effects.** `PRHeadSHAUpdated` is judged first, so a dropped replay cannot prune check runs, reset the verdict or re-key `shaToKey`.
7. **Reconcile removal guard.** `ItemState.AddedAt` is stamped when the running Store creates a record; `Reconcile` removes a missing item through `Store.RemoveIfNotAddedAfter(fetchStart)`, deciding under the lock. A zero `FetchedAt` removes unconditionally.
8. **One review shape.** The Store holds the latest review per author by `(SubmittedAt, DatabaseID)`; webhook, deep-fetch and listing producers all go through `mergeReviews`/`latestReviewPerAuthor`, and `dismissed`/`edited` replace their review in place. Webhook review events are ordered per author (`reviews:<author>`) so one reviewer's newer review cannot drop another's older delivery or a later dismissal; `edited`/`dismissed` are stamped with receipt time because their `submitted_at` is the original submission time, and they do not advance the author's submission record (`reviews:submitted:<author>`), so a late-processed dismissal cannot drop a newer re-review. The audit rates the former divergence fail-safe (a stale `CHANGES_REQUESTED` only holds a gate), and the deep-fetch shape is the one consumers already read.
9. **Layer 2 sweep.** The Layer 2 status sweep (`ApplyStatusBatchAt`) was a fourth unordered Status producer; it now carries its request start like a listing.

## Consequences

- A genuine change that arrives with no webhook, seen only by a listing fetched within 30 s of an earlier observation of the same fact, waits for the next tick (~3 min). ADR 2159 already accepted this for engine writes; it now extends to webhook-versus-listing.
- Webhook times come from GitHub and everything else from the daemon's clock; a badly skewed daemon clock makes webhooks look old or new. No synchronisation mechanism is added. An untimed webhook is ordered by receipt time.
- ADR 2178: the probe apply still uses only the Status/closed half and never touches label records; "a probe carries no labels, so absence is not an observation" is unchanged.
- ADR 2347: `StatusRefreshed`'s drop of a read older than `StatusEnteredAt` is a special case of this rule and is kept (its tests are untouched); it additionally records and honours the `status` observation.
- ADR 2180 and `SelfWriteObserved` (audit §2.7, the freshness baseline) are untouched.
- Review lists change shape (one entry per author, sorted by author); the review gate, feedback gate and `reviewerVerdicts` were checked against it.
- Neutralisation seam: `Store.SetObservedOrderingDisabledForTest` (and `Engine.SetObservedOrderingDisabledForTest`). Sim twins `tests/sim/webhook_ordering_test.go` run with and without it; live case `TestWebhookChangeSurvivesReconcile`.

## Not changed

Dispatch, gate and landing decisions; what the board query, the probe or the webhook handlers fetch beyond carrying observation times; persistence of the records across restarts (the first listing re-baselines every fact); a second webhook source (#2143), which needs only to supply observation times.
