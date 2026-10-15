# Feature Specification: Reset the comment circuit breaker on every resume path

**Feature Branch**: `fabrik/issue-1575`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

Four engine paths resume an issue after a pause or invalidation, and all four clear accumulated
cycle state. Only two of them clear *both* circuit breakers:

| path (`engine/item.go`) | `EngineCyclesCleared` | `resetCommentBreaker` |
|---|---|---|
| `clearFailedStage` | yes | yes |
| `unblockAwaitingInput` | yes | yes (added by #1555) |
| `handleRevalidateLabel` | yes | **no** |
| `handleValidateSHAInvalidation` | yes | **no** |

`EngineCyclesCleared` resets the counter family in `itemstate` (`ReviewCycles`,
`ReviewBlockedCycles`, `CIFixCycles`, `RebaseCycles`, `EnqueueCycles`, and — since #1555 —
`NoOpCommentCycles`). `resetCommentBreaker` clears #1089's separate item-scoped `CommentBreaker`
invocation list, which `EngineCyclesCleared` does not touch.

So an operator applying `fabrik:revalidate`, or a Validate SHA invalidation firing, clears every
cycle counter but leaves #1089's breaker holding its accumulated invocation timestamps. An issue
resumed by either path can therefore re-trip the #1089 breaker on fewer fresh invocations than a
genuinely fresh start would need.

## Impact

Low, and bounded. #1089's breaker is a *rolling window* — it prunes entries older than
`effectiveCommentBreakerWindow` (30 minutes) on the next read — so the stale state self-clears with
the passage of time rather than persisting indefinitely. The observable symptom is confined to a
resume that happens within the window of a prior burst, and it is a premature trip (a pause), not a
missed one, so it fails safe.

This is **pre-existing** — it predates #1555 and is not a regression introduced by it.

## Why file it now

#1555 established "both breakers reset together" as the correct pattern by fixing exactly this hole
in `unblockAwaitingInput`, where it mattered more (that path is how the new success-agnostic breaker's
own pause is resumed, so leaving it half-reset caused an immediate re-trip). Having fixed two of the
four paths, the remaining asymmetry is now an inconsistency in an established convention rather than
an unremarked gap — which is the point at which it becomes cheap to close and confusing to leave.

Deliberately kept out of #1555's scope to avoid widening a PR that was already ~1900 lines and had a
separate blocking finding open.

## Requirements

- **R1** — `handleRevalidateLabel` calls `e.resetCommentBreaker(item)` alongside its existing
  `EngineCyclesCleared` application, matching `clearFailedStage`.
- **R2** — `handleValidateSHAInvalidation` does the same.
- **R3** — Both respect their existing deferral-on-error contract: these functions defer their store
  resets to the next poll when a label removal fails, so the breaker reset must sit on the same side
  of that branch as `EngineCyclesCleared` — not run unconditionally ahead of it. Do not change when
  the resets happen, only what is included in them.
- **R4** — `docs/state-machine.md` records that all four resume paths clear both breakers, so the
  invariant is stated once rather than inferred from four call sites.

## Scope

**In:** `engine/item.go` (the two named functions), their tests, `docs/state-machine.md`.

**Out:** any change to either breaker's trip logic, thresholds, or window; any change to
`EngineCyclesCleared`'s mutation semantics. This is purely about which reset calls accompany which
resume paths.

## Acceptance

1. A test drives `handleRevalidateLabel` on an item with a non-empty `CommentBreaker` invocation
   list and asserts the list is empty afterwards. Proved non-vacuous: removing the new
   `resetCommentBreaker` call makes it red.
2. The equivalent test exists for `handleValidateSHAInvalidation`.
3. A test covers R3 — on a failed label removal, neither the cycle reset nor the breaker reset is
   applied, and both are retried together on the next poll.
4. `go test -race ./...` and `go vet ./...` clean.

## Notes

Found by an independent review of #1555/PR #1561 that was scoped to look for *sibling* routes with
the same hole as the one fixed there. The other three routes checked (manual label removal via
`clearFailedStage`, merge-train ejection, stage advance) were clean."

## Background

Fabrik has several paths that resume an item after it was paused, failed, or had its Validate completion invalidated. Each of them wipes the item's accumulated cycle state so the resumed item gets a fresh budget. There are two independent pieces of state to wipe:

- the **cycle counters** (review, review-blocked, CI-fix, rebase, enqueue and no-op-comment cycles), and
- the **comment circuit breaker** from #1089, a separate per-item rolling-window list of recent comment-invocation timestamps (30-minute window).

Issue #1555 established the convention that a resume clears *both*, and applied it to the awaiting-input resume path, where leaving the breaker half-reset caused an immediate re-trip. Two sibling paths still clear only the cycle counters:

| resume path | clears cycle counters | clears comment breaker |
|---|---|---|
| manual unpause of a failed stage | yes | yes |
| awaiting-input resume | yes | yes (added by #1555) |
| `fabrik:revalidate` operator label | yes | **no** |
| Validate SHA invalidation | yes | **no** |

Impact is low and fails safe. The breaker is a rolling window, so stale entries age out by themselves. The symptom is limited to a resume within the window of a prior comment burst, and it is a premature pause rather than a missed one. The defect predates #1555. It was found by a review of #1555 that looked specifically for sibling routes with the same gap. The other routes checked (merge-train ejection, stage advance) were clean. Closing it now removes an inconsistency in an established convention.

The Validate re-entry logic is also reused by the merge train's red-singleton auto-repair (#2045), which performs the same clearing without an operator-applied trigger label.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator-triggered revalidate gives a fresh comment budget (Priority: P1)

An operator applies `fabrik:revalidate` to an item that recently saw a burst of comment-processing invocations. After the engine completes the re-entry, the item's comment breaker holds no prior invocations, exactly as it would after a manual unpause.

**Why this priority**: This is the primary operator-facing resume path with the gap.

**Independent Test**: Seed an item's comment breaker with several recent invocation timestamps, run the revalidate handling to completion, and check that the item's breaker has none left.

**Acceptance Scenarios**:

1. **Given** an item with a non-empty comment-breaker invocation list and `fabrik:revalidate` applied, **When** the engine handles the label and every label removal succeeds, **Then** the item's comment-breaker invocation list is empty and the cycle counters are also cleared.
2. **Given** the same item, **When** a blocking-label or trigger-label removal fails with a non-404 error, **Then** neither the cycle counters nor the comment breaker are reset, and both are reset together on the poll in which the handling completes.

---

### User Story 2 - Validate SHA invalidation gives a fresh comment budget (Priority: P1)

When the linked PR's head SHA changes after Validate completed and the engine invalidates the stale completion, the item's comment breaker is cleared along with the cycle counters.

**Why this priority**: It is the other half of the same asymmetry, and the issue calls for closing both.

**Independent Test**: Seed the breaker, run the SHA-invalidation handling to completion, and check that the breaker's list is empty.

**Acceptance Scenarios**:

1. **Given** an item with a non-empty comment-breaker invocation list and a Validate completion recorded at an older SHA, **When** the engine handles the invalidation and every label removal succeeds, **Then** the comment-breaker list is empty and the cycle counters are cleared.
2. **Given** the same item, **When** a label removal fails, **Then** neither reset is applied (the scan sentinel label stays in place), and both are applied together on the retry.

---

### User Story 3 - The invariant is documented once (Priority: P2)

A maintainer reading the state-machine documentation finds one statement that every resume path clears both the cycle counters and the comment breaker, so a future resume path knows to do the same.

**Why this priority**: Documentation does not change behavior, but it keeps the convention from drifting again.

**Independent Test**: Read the state-machine spec and confirm the invariant is stated and lists the resume paths.

**Acceptance Scenarios**:

1. **Given** the updated docs, **When** a reader looks up the resume paths, **Then** they find a single statement that all of them clear both the cycle counters and the comment breaker.

---

### Edge Cases

- A label removal fails partway through: the deferral-on-error contract is unchanged, and the breaker reset is deferred together with the cycle reset, never run ahead of it.
- A label that is already absent (404) is treated as success today. The breaker reset follows the same rule as the cycle reset in that case.
- The item has no recorded breaker invocations: the reset is a harmless no-op.
- The merge-train auto-repair reuses the revalidate re-entry logic. Its behavior with respect to the breaker must be consistent with the operator path (see Assumptions).
- The resume happens more than 30 minutes after the last comment burst: the breaker had already aged out, so the reset changes nothing observable.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When `fabrik:revalidate` handling completes (all blocking labels and the trigger label removed or already absent), the item's comment-breaker invocation list is cleared in the same step that clears its cycle counters.
- **FR-002**: When Validate SHA invalidation handling completes (all listed labels removed or already absent), the item's comment-breaker invocation list is cleared in the same step that clears its cycle counters.
- **FR-003**: In both paths, if any label removal fails and the handling defers to the next poll, the comment breaker is not reset; the breaker reset and the cycle-counter reset are applied together or not at all, and on the same poll as before. The timing of the existing resets does not change.
- **FR-004**: The state-machine documentation states that all resume paths (manual unpause of a failed stage, awaiting-input resume, `fabrik:revalidate`, Validate SHA invalidation) clear both the cycle counters and the comment breaker.
- **FR-005**: Tests cover FR-001 and FR-002, each shown to fail if the new breaker reset is removed. A test covers FR-003 for the failed-removal path and the subsequent retry.

### Key Entities *(if applicable)*

- **Comment breaker (#1089)**: Per-item rolling-window record of recent comment-processing invocations, used to pause an item that is being invoked too often. It is separate from the cycle counters.
- **Cycle counters**: Per-item counters for review, review-blocked, CI-fix, rebase, enqueue and no-op-comment cycles, cleared by the engine's cycles-cleared event.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After an operator-triggered revalidate completes, an item's comment-breaker invocation list is empty (0 entries), as for a manual unpause.
- **SC-002**: After a Validate SHA invalidation completes, an item's comment-breaker invocation list is empty (0 entries).
- **SC-003**: In 100% of failed-removal cases covered by tests, neither reset is applied on the failing poll, and both are applied together on the completing poll.
- **SC-004**: The state-machine documentation lists all four resume paths as clearing both the counters and the breaker, and the generated documentation bundle is in sync with it.
- **SC-005**: The full test suite with the race detector and the vet check both pass.

## Assumptions

- The Validate re-entry core is shared by the operator `fabrik:revalidate` handler and the merge train's red-singleton auto-repair. The breaker reset sits where the cycle reset sits, so both callers get it. The auto-repair is also a fresh re-entry of Validate that already clears the cycle counters, so the same convention applies. If a reviewer wants the auto-repair excluded, that is a small, reversible change.
- Tests count as in scope with the two functions. The existing test seams for seeding and reading the comment breaker are sufficient.
- Editing the canonical state-machine documentation also requires regenerating the bundled documentation file in the same commit, per repository convention.
- A failed label removal leaves the breaker untouched because the breaker reset follows the same early-return as the cycle reset.

## Out of Scope *(optional)*

- Any change to either breaker's trip logic, thresholds, or window.
- Any change to the mutation semantics of the cycles-cleared event.
- Resume or reset paths outside the four named ones. The review found merge-train ejection and stage advance clean.

## Source References *(optional)*

- #1555 / PR #1561: established the "both breakers reset together" convention and fixed the awaiting-input path.
- #1089: the comment circuit breaker.
- #2045: merge-train red-singleton auto-repair, which reuses the revalidate re-entry core.
