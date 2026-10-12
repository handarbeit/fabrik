# Feature Specification: Stop engine self-writes from masking external changes in the board cache

**Feature Branch**: `fabrik/issue-2180`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: the original issue body, quoted verbatim below.

> ## Problem
>
> `SelfWriteObserved` (ADR-044 addendum 4, #1090) moves an item's cache baseline to the local clock at every engine self-write. The cache keeps serving an item while the board's `updatedAt` is not after that baseline. So any external change whose `updatedAt` predates the engine's latest self-write never looks new, and readers going through `e.readClient` see the pre-change copy until something else invalidates it.
>
> #2160 (ADR 2160) handled one instance: review bodies and threads held by the feedback gate. That fix is a bounded repair triggered from the gate. #1944 handled another: comments on paused items. ADR 2160 records the rest of the class as a known, out-of-scope gap:
> - **CI status** (check runs / suites) on a stage-complete item;
> - **review-thread comments** not reached by the feedback gate's repair;
> - **human comments on non-paused items**.
>
> Each of these can stall an item until the next unrelated invalidation or a restart. That is the #2125 shape, where a restart was the only thing that cleared it.
>
> ## Requirements
>
> - **R1 — Fix the class, not instances.** Research determines a cache-freshness rule under which an external change that lands near an engine self-write is not masked. Candidates to evaluate:
>   - (a) anchor the baseline to GitHub's own `updatedAt` from the self-write's response rather than the local clock;
>   - (b) keep a short post-self-write window in which the next board `updatedAt` change always refetches;
>   - (c) compare per-layer `updatedAt` (issue, PR, check suite) instead of one item value.
>
>   Any choice must keep #1090's guarantee: no self-write refetch loops.
> - **R2 — Retire the per-instance backstops where the rule subsumes them.** If R1 covers the #1944 and #2160 cases, say so and remove or keep each repair with a stated reason. Do not stack a third backstop.
> - **R3 — Bounded cost.** Measure and state the extra GraphQL spend per poll on a typical board (the dev daemon's), against the #1014/#1090 budget concerns.
> - **R4 — Docs.** Update `docs/state-machine.md` (cache freshness, D.x), ADR-044's addendum chain or a new ADR, and the cross-references in ADR 1953-a and ADR 2160.
>
> ## Scope
>
> - In scope: the `SelfWriteObserved` baseline and the cache-serve decision in `boardcache`/`itemstate`.
> - Out of scope: the engine-write protection against stale listings (#2159 and its probe follow-up), which is a different direction (a stale listing overwriting the engine's write, not an engine write masking an external change).
>
> ## Acceptance
>
> - A sim (or boardcache) test per masked layer (CI status, a thread comment, a human comment on a non-paused item): the change arrives with an `updatedAt` just before an engine self-write, and is seen by the dispatcher within one poll without a restart. Each fails today.
> - A test showing a self-write alone causes no refetch loop (the #1090 guarantee).
> - The full test suite passes.

## Background

To avoid wasteful refetch loops, the engine treats its own writes (labels, comments, board moves) as non-events. When the engine writes to an item, the cache baseline for that item moves to the engine's local clock (`SelfWriteObserved`, ADR-044 addendum 4, #1090). Because the write itself bumps the item's `updatedAt` on GitHub, this stops the engine re-reading its own change.

The cost is a blind spot. The cache serves an item while the board's `updatedAt` is not after that baseline. An external change that lands just before the engine's self-write, with an `updatedAt` earlier than the new baseline, never looks new. The engine keeps acting on the pre-change copy until something unrelated invalidates the item or the daemon restarts. In practice the engine can sit on an item that has already changed (a CI result, a reviewer's thread comment, a human's comment) and stay stalled with no visible cause. #2125 was this shape, and a restart was the only thing that cleared it.

Two instances have been fixed individually: comments on paused items (#1944) and review bodies and threads held by the feedback gate (#2160, ADR 2160). Each is a targeted repair triggered from one call site. ADR 2160 records the remaining instances as a known gap: CI status (check runs and suites) on a stage-complete item, review-thread comments the gate's repair does not reach, and human comments on items that are not paused.

The aim is to fix the class with one cache-freshness rule, not add a third per-instance backstop.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - CI result that lands just before an engine write is seen (Priority: P1)

An item is stage-complete and waiting on CI. A CI check completes. Within the same interval the engine writes to the item (for example a label or a status line), so the check's change time falls just before the engine's write time. The operator expects the engine to act on the new CI result without a restart.

**Why this priority**: A stalled CI gate blocks advancement and landing, and is the most likely masked layer to hold a stage-complete item indefinitely.

**Independent Test**: In a sim, make a check run finish with an `updatedAt` just before an engine self-write on the same item, and poll once. The dispatcher must see the new CI verdict. This test fails on the current behavior.

**Acceptance Scenarios**:

1. **Given** a stage-complete item awaiting CI and an engine self-write at time T, **When** a check run completes with a change time just before T, **Then** the dispatcher sees the updated CI state within one poll, with no restart.
2. **Given** the same item, **When** the engine self-writes and nothing external changes, **Then** the item is not refetched solely because of that self-write.

---

### User Story 2 - Review-thread comment that lands just before an engine write is seen (Priority: P1)

A reviewer adds a thread comment on a PR that the feedback gate's existing repair does not reach. The comment's time falls just before an engine self-write on the item. The engine should treat it as unprocessed feedback in the next poll.

**Why this priority**: Missed review feedback means the engine can advance or land past a reviewer's request.

**Independent Test**: In a sim or boardcache test, add a thread comment with an `updatedAt` just before an engine self-write, and poll once. The dispatcher must see the comment. This test fails on the current behavior.

**Acceptance Scenarios**:

1. **Given** an item on a path the feedback gate's repair does not cover, **When** a thread comment arrives with a change time just before an engine self-write, **Then** the dispatcher sees it within one poll.

---

### User Story 3 - Human comment on a non-paused item is seen (Priority: P1)

A human comments on an item that is not paused. The comment's time falls just before an engine self-write. The engine should pick the comment up in the next poll, as it already does for paused items (#1944).

**Why this priority**: Human comments are steering input. A masked one leaves the operator's instruction unacted on.

**Independent Test**: In a sim or boardcache test, post a human comment with an `updatedAt` just before an engine self-write on a non-paused item, and poll once. The dispatcher must see the comment. This test fails on the current behavior.

**Acceptance Scenarios**:

1. **Given** a non-paused item, **When** a human comment arrives with a change time just before an engine self-write, **Then** the dispatcher sees it within one poll, with no restart.

---

### User Story 4 - The engine's own writes still cause no refetch loop (Priority: P1)

The engine's own writes must stay non-events. The new rule must not bring back the refetch-after-every-write behavior that #1090 removed.

**Why this priority**: Without this guarantee the fix would trade a stall for GraphQL budget exhaustion (#1014, #1090).

**Independent Test**: A test performs engine self-writes on an item with no external change and asserts that polling does not refetch it repeatedly because of those writes.

**Acceptance Scenarios**:

1. **Given** an item and an engine self-write, **When** several polls run with no external change, **Then** the self-write causes no refetch, or at most a single bounded one, and the count does not grow with the number of polls.

---

### User Story 5 - One rule replaces the per-instance backstops (Priority: P2)

A maintainer wants the cache-freshness behavior to be one understandable rule, not a growing set of special-case repairs.

**Why this priority**: The #1944 and #2160 repairs exist because the general rule was wrong. If the new rule covers them, keeping them only adds code that has to be maintained and reasoned about.

**Independent Test**: For each of the #1944 and #2160 repairs, the change records whether the new rule subsumes it. If it does, the repair is removed and its existing tests still pass or are replaced by equivalent rule-level tests. If it is kept, a reason is stated.

**Acceptance Scenarios**:

1. **Given** the chosen rule, **When** the #1944 and #2160 repairs are examined, **Then** each is either removed or kept with a stated reason, and no new per-instance backstop is added.

---

### User Story 6 - Cost is known and bounded (Priority: P2)

An operator wants to know what the fix costs in GraphQL budget before accepting it.

**Why this priority**: Budget exhaustion was the reason for #1090, so a fix that raises the steady-state spend needs the number stated and judged.

**Independent Test**: The extra per-poll GraphQL spend is measured on a typical board (the dev daemon's) and recorded in the change's documentation.

**Acceptance Scenarios**:

1. **Given** the chosen rule, **When** it runs against a typical board, **Then** the documentation states the measured extra GraphQL spend per poll and compares it to the #1014/#1090 budget.

---

### Edge Cases

- The external change lands in the same instant as the engine write (equal timestamps), or after the write but before the engine has observed the write's response.
- Clock skew between the local clock and GitHub's: a baseline anchored to the local clock can sit ahead of GitHub's timestamps and mask changes, or sit behind them and cause a loop.
- Several engine self-writes to one item in quick succession, with an external change landing between them.
- An external change to a layer other than the one the engine wrote to (for example the engine writes a label while a check suite finishes), where one item-level `updatedAt` cannot tell the two apart.
- An engine write that fails or is rejected: the baseline must not advance as if it had succeeded.
- Webhook-delivered changes arriving alongside polled ones: the same change must not be applied twice or count as a self-write echo.
- Paused items, whose existing #1944 path must keep working until it is explicitly retired.
- Display-only field writes (status line, last activity, last run), which bump an item's `updatedAt` but are not state and must not cause refetches.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The cache-freshness rule must ensure that an external change to an item's CI status, review-thread comments, or comments is visible to the dispatcher within one poll even when the change's `updatedAt` is earlier than the engine's latest self-write to that item.
- **FR-002**: The change must define one general freshness rule for the cache-serve decision, not a fix per masked layer. Research must evaluate at least these candidates and record the reasons for the choice: (a) anchoring the baseline to GitHub's own `updatedAt` from the self-write's response instead of the local clock; (b) a short post-self-write window in which the next board `updatedAt` change always refetches; (c) comparing per-layer `updatedAt` (issue, PR, check suite) instead of a single item value.
- **FR-003**: The chosen rule must preserve #1090's guarantee: an engine self-write alone must not cause refetch loops, whether on one poll or across many.
- **FR-004**: For each of the #1944 (paused-item comments) and #2160 (feedback-gate review bodies and threads) repairs, the change must state whether the new rule subsumes it, and either remove it or keep it with a stated reason. It must not add a third per-instance backstop.
- **FR-005**: The change must measure and document the extra GraphQL spend per poll on a typical board (the dev daemon's), compared with the #1014/#1090 budget concerns.
- **FR-006**: The change must add a test for each masked layer (CI status, a review-thread comment, a human comment on a non-paused item). Each test delivers the change with an `updatedAt` just before an engine self-write and asserts the dispatcher sees it within one poll without a restart, and each test must fail on the current behavior.
- **FR-007**: The change must add a test showing that an engine self-write alone causes no refetch loop.
- **FR-008**: The full existing test suite must pass.
- **FR-009**: Documentation must be updated: `docs/state-machine.md` (cache freshness section), ADR-044's addendum chain or a new ADR recording the decision, and the cross-references in ADR 1953-a and ADR 2160. The documentation bundle `docs/llms-full.txt` must be regenerated alongside any canonical-doc change.
- **FR-010**: The change must be limited to the `SelfWriteObserved` baseline and the cache-serve decision in `boardcache`/`itemstate`; engine-write protection against stale listings is not part of it.

### Key Entities

- **Cache baseline**: The per-item timestamp the cache compares the board's `updatedAt` against to decide whether its copy is still fresh. The engine currently moves it to the local clock at every self-write.
- **Self-write**: Any write the engine itself makes to an item (labels, comments, board moves, display fields). The write bumps the item's `updatedAt` on GitHub and must not be read back as an external change.
- **Item layers**: The separately changing parts of what the engine reads for an item: the issue (body, labels, comments), the linked PR (reviews, threads), and CI (check runs and suites).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For each of the three masked layers (CI status, review-thread comment, human comment on a non-paused item), a change whose `updatedAt` precedes an engine self-write is seen by the dispatcher within one poll, with no restart. Each of the three tests fails before the change and passes after it.
- **SC-002**: With no external change, repeated engine self-writes to an item cause no refetch growth. Refetches attributable to self-writes stay at zero (or a fixed small bound) regardless of poll count.
- **SC-003**: The measured extra GraphQL spend per poll on the dev daemon's board is documented, and a reader can judge it against the #1014/#1090 budget.
- **SC-004**: The number of per-instance cache-masking backstops does not increase, and each of the two existing ones (#1944, #2160) has a documented removed-or-kept outcome.
- **SC-005**: The full test suite passes and the documentation set listed in FR-009 is updated and consistent.

## Assumptions

- The three masked layers named in the issue (CI status, review-thread comments, human comments on non-paused items) are the full known set. If Research finds more, the general rule should cover them too.
- The choice between the candidate rules is a Research and Plan decision. This spec only requires that the chosen rule meets FR-001 to FR-004.
- "Within one poll" means the first poll after the change is visible to GitHub's board listing, not after GitHub's own propagation delay.
- "Typical board" means the dev daemon's board, as the issue states.
- A single bounded refetch after a self-write is acceptable if the cost is documented. An unbounded or per-poll refetch is not.
- Webhook-delivered updates are handled by their own path. The rule is about the polled board-listing comparison and must not break webhook echo handling.

## Out of Scope *(optional)*

- Engine-write protection against stale listings (#2159 and its probe follow-up). That is the opposite direction: a stale listing overwriting an engine write.
- Changes to the polling interval, the probe loop's cadence, or the GraphQL query shapes beyond what the chosen rule needs.
- New operator-visible surfaces, such as a TUI or warnings-panel entry, for cache staleness.

## Source References *(optional)*

- ADR-044 (probe-driven poll loop), addendum 4: `SelfWriteObserved`
- #1090, #1014: the self-write refetch loop and GraphQL budget concerns
- #1944: comments on paused items
- #2160 (ADR 2160): feedback-gate repair for review bodies and threads
- ADR 1953-a: feedback gate
- #2125: the restart-only recovery incident
- #2159: stale-listing protection (out of scope)
