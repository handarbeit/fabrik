# Feature Specification: Merge-train Queued order should follow GitHub's move order for moves observed together

**Feature Branch**: `fabrik/issue-2246`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description:

> ## Problem
>
> The merge train orders each (repo, base) partition by `StatusEnteredAt` (ADR-1833), which `internal/itemstate` stamps with `time.Now()` when the engine *observes* a Status change. When several moves are picked up in the same light reconcile or probe pass, they are stamped in the order the apply loop visits them, so the batch order among them is arbitrary rather than the order the moves happened on GitHub.
>
> Seen in the v0.0.84 gate: four members moved to Queued back to back as A, B, C, P formed the batch `[A, P, B, C]` (bed log 21:43:17 light reconcile, 21:43:52 snapshot). The live test was changed to queue in observed order (PR #2243); the engine behaviour is unchanged.
>
> Impact in production is small (fairness among items queued within the same poll), but the order also decides bisection's halves, so it is not purely cosmetic.
>
> ## Requirements
>
> - R1: decide whether `StatusEnteredAt` should come from GitHub's own time for the move (e.g. the `projects_v2_item` webhook's timestamp, or the project item's field `updatedAt` when the change is applied from a listing), falling back to observation time when unknown.
> - R2: whatever the source, moves seen in one pass must be ordered deterministically and as close to GitHub's order as the available data allows.
>
> ## Scope
>
> Investigation first; Backlog until agreed. No change to the batch cap or overlap admission.

## Background

The merge train lands Queued members in "entry order": each (repo, base) partition is sorted ascending by `(StatusEnteredAt, Number)` before the batch cap is applied (ADR-1833). ADR-1833 chose a record-on-write stamp because GitHub offers no cheap, durable "time this item entered Queued" signal: the REST issue-events API has no event for Projects v2 Status changes, and per-item field-history GraphQL queries were rejected on node-budget grounds. The stamp is therefore the engine's own clock at the moment it *observes* the change (a webhook, a light reconcile, a probe, a deep fetch, or its own write).

That is stable poll-to-poll (the original defect, random re-selection every poll, is fixed), but it makes the order among several moves observed in the *same pass* depend on the order the apply loop happens to visit them, not on the order they happened on GitHub. The v0.0.84 release gate showed this: four members queued back to back (A, B, C, P) formed the batch `[A, P, B, C]`. The live test was adapted to queue in observed order (PR #2243); the engine was not changed.

Production impact is small — fairness among items queued within one poll — but entry order also decides the halves used by bisection after a red batch, so the arbitrariness has a (minor) cost beyond cosmetics. The ADR-1833 "accepted limitations" (restart resets the clock; direct-GraphQL mode degrades to number order) are related but are not what this issue is about.

This issue is **investigation first**: establish what ordering information GitHub actually makes available for these moves, decide whether to use it, and ensure that whatever is used yields deterministic ordering for moves seen together.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Decide the source of the entry time (Priority: P1)

An engine maintainer needs a recorded, evidence-based decision on whether the Queued entry time should come from GitHub's own time for the move (webhook timestamp, project-item/field `updatedAt` from a listing) rather than the engine's observation time, and for which observation paths.

**Why this priority**: R1 is the gating question; no behaviour change is justified until the available data and its reliability are known.

**Independent Test**: The decision record can be reviewed without any code change: it states, per observation path (webhook, light reconcile, probe, deep fetch, engine's own write, restart bootstrap), whether GitHub supplies a time that identifies the move, how precise and trustworthy it is, and what the chosen behaviour is.

**Acceptance Scenarios**:

1. **Given** the investigation is complete, **When** a maintainer reads the decision record, **Then** it names, for every path that stamps a Status change, the candidate GitHub time (or its absence), its granularity, and whether it is adopted.
2. **Given** a candidate GitHub time is found to also change for reasons other than a Status move (e.g. display-only field edits or other field changes bump the same timestamp), **When** the decision is made, **Then** the record states this and the candidate is either rejected or constrained so that such bumps cannot reorder Queued members.

---

### User Story 2 - Moves seen together are ordered as GitHub ordered them (Priority: P1)

An operator queues several items back to back. When the engine learns about them in one pass, the batch order matches the order in which they were moved, to the extent GitHub's data allows, instead of depending on the engine's iteration order.

**Why this priority**: This is the observable defect (`[A, P, B, C]` for A, B, C, P).

**Independent Test**: Move N items to Queued back to back; deliver them to the engine in a single reconcile/probe pass (in varying input orders); assert the resulting partition order is the same each time and equals move order wherever GitHub supplies distinguishable times.

**Acceptance Scenarios**:

1. **Given** items A, B, C, P moved to Queued in that order and observed in one pass, **When** the train forms a batch, **Then** the order is A, B, C, P regardless of the order the listing returns them.
2. **Given** the same four items observed in one pass but GitHub supplies no distinguishable time for them, **When** the batch forms, **Then** the order is deterministic (identical on every run for the same inputs) and the tie-break is documented.
3. **Given** an item observed individually through a webhook before the next listing, **When** a later listing reports the same Status, **Then** its entry time is not changed by the listing (a genuine transition is stamped once).

---

### User Story 3 - Unknown or untrustworthy times degrade safely (Priority: P2)

When GitHub's time for a move is unavailable, zero, or implausible (for example later than the observation time, or much earlier than a previously recorded entry), the engine falls back to observation time and the order remains deterministic.

**Why this priority**: The fix must not make ordering worse in cases where the new signal is missing, which includes direct-GraphQL mode and bootstrap.

**Independent Test**: Feed the stamping paths a mix of known, zero, and out-of-range GitHub times and assert fallback behaviour and determinism.

**Acceptance Scenarios**:

1. **Given** a Status change with no usable GitHub time, **When** it is applied, **Then** the entry time equals the observation time, as today.
2. **Given** a GitHub time later than the observation time (clock skew), **When** it is applied, **Then** the entry time is clamped or ignored so no item can sort ahead of genuinely later moves by virtue of a future timestamp.

---

### Edge Cases

- Two moves with identical GitHub times (second-level granularity): ordering falls through to a documented deterministic tie-break (currently issue number); the spec does not require recovering order GitHub did not record.
- An item moved to Queued, out, and back in: the entry time reflects the most recent entry into Queued, not an earlier one.
- Engine's own `LocalStatusUpdated` write (e.g. an item rerouted into Queued by the engine): the engine's own time remains authoritative for its own writes.
- Restart bootstrap: items already Queued get bootstrap stamps (ADR-1833 accepted limitation); any new source must not make restart ordering less deterministic than today, and may improve it where GitHub supplies a trustworthy time.
- Direct-GraphQL (non-cache) mode: no stamp is available today; behaviour must remain deterministic number order unless the investigation shows a cheap signal.
- Display-only project field writes (status line, last activity, last run) bump the project item's `updatedAt`; such bumps must never be mistaken for a Status move.
- A move observed first by webhook and then by a listing carrying a different (e.g. later) time for the same Status must not re-stamp the item.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The investigation MUST produce a written decision, per Status-observation path, on whether the entry time is taken from a GitHub-supplied time for the move or from observation time, with the evidence (what GitHub provides, its granularity, what else changes it) recorded in an ADR.
- **FR-002**: Where a trustworthy GitHub-supplied time for the move is available on a path, the entry time for a Queued (any Status) transition MUST use it; otherwise it MUST fall back to observation time.
- **FR-003**: Several Status moves observed in one pass MUST be ordered deterministically: the same set of moves yields the same partition order regardless of the order in which they are visited or returned by GitHub.
- **FR-004**: Where GitHub supplies distinguishable times for moves observed together, the resulting order MUST match the order of those times.
- **FR-005**: Where GitHub supplies equal or no times for moves observed together, ties MUST be broken by a stable, documented rule that does not depend on iteration order.
- **FR-006**: A genuine Status transition MUST still be stamped exactly once; no-op reconciles, repeat webhooks and later listings reporting the same Status MUST NOT change the recorded entry time.
- **FR-007**: A timestamp that is implausible (later than the observation time by more than a documented tolerance, or otherwise inconsistent with the item's previously recorded history) MUST NOT be used as the entry time; the fallback in FR-002 applies.
- **FR-008**: A change to a timestamp that is not caused by a Status move (display-only field writes, label or comment activity) MUST NOT alter an item's entry time.
- **FR-009**: The engine's own Status writes MUST keep their engine-side time as the entry time.
- **FR-010**: The batch cap, overlap admission, partitioning by base, and sort key shape (`StatusEnteredAt` then number, within a (repo, base) partition) MUST be unchanged; only the source and same-pass determinism of `StatusEnteredAt` may change.
- **FR-011**: Automated tests MUST cover: same-pass moves delivered in shuffled order, equal/absent GitHub times, out-of-range times, repeat observation of the same Status, and each observation path covered by the decision; the sim and unit layers MUST be able to reproduce the original `[A, P, B, C]` case against the pre-change behaviour.
- **FR-012**: The as-built documentation of Queued ordering MUST describe the final source and tie-break rules and reference the new ADR.

### Key Entities

- **Status entry time (`StatusEnteredAt`)**: the per-item time a current Status was entered, used as the primary sort key for Queued order.
- **Observation path**: one of the ways the engine learns of a Status change (webhook delta, light reconcile, probe, deep fetch, engine's own write, restart bootstrap).
- **GitHub-supplied move time**: any time field GitHub returns that may identify when the move happened (webhook payload time, project item or field `updatedAt`).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Replaying the v0.0.84 gate scenario (A, B, C, P queued back to back and observed in one pass) yields batch order A, B, C, P in every run, across at least 20 shuffled input orders, wherever GitHub provides distinguishable times.
- **SC-002**: With no usable GitHub times, the same scenario yields an identical order on every one of at least 20 shuffled input orders.
- **SC-003**: No existing merge-train ordering, batch-cap, overlap, bisection or churn tests change behaviour (they pass unmodified, other than tests that encoded the old observation-order assumption).
- **SC-004**: A display-only field write on a Queued item produces no change in its position within the partition.
- **SC-005**: The decision and its evidence are recorded in an ADR numbered after this issue, and the as-built docs reflect the result.

## Assumptions

- The issue's own scope applies: this is "investigation first"; if the investigation shows no GitHub time reliably identifies a Status move on a given path, the correct outcome for that path is to keep observation time and add only the same-pass determinism of FR-003/FR-005. A "no behaviour change on this path, with evidence" result is an acceptable deliverable.
- Same-pass determinism is in scope even if no GitHub time is adopted, since R2 requires it "whatever the source".
- The webhook delivery time and the project-item/field `updatedAt` are the candidate sources named in the issue; the investigation may examine others available without extra per-item queries.
- Cost constraints from ADR-1833 stand: no per-item, per-poll field-history GraphQL query.
- Skew tolerance for FR-007 is chosen during Plan and documented; it is not a user-facing setting.
- No new user-facing configuration or CLI flag is expected.
- The Backlog gating in the issue ("until agreed") is a pipeline-management note; the issue reached Specify, so the investigation proceeds on this spec.

## Out of Scope *(optional)*

- Changing the batch cap (`max_batch_size`) or overlap-aware admission.
- Making the entry time survive restarts durably (ADR-1833 accepted limitation) beyond whatever a trustworthy GitHub time gives for free.
- Changing ordering in direct-GraphQL mode beyond what a cheap signal provides.
- Reordering by anything other than entry time (priority, size, dependencies).
- Changes to live-test queueing order (already adapted in PR #2243), other than restoring strict move-order assertions if the fix makes them reliable.

## Source References *(optional)*

- ADR-1833 (`adrs/1833-deterministic-queued-batch-ordering.md`) — current record-on-write design and its accepted limitations.
- ADR-1314 — `LabelAppliedAt` record-on-write precedent.
- ADR-2159 / ADR-2178 — engine-write protection and probe apply rules that share the Status stamping paths.
- PR #2243 — live test adapted to observed queue order.
- v0.0.84 release gate bed log (21:43:17 light reconcile, 21:43:52 snapshot).
