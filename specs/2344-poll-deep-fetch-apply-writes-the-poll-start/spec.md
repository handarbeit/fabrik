# Feature Specification: Deep-fetch apply must not overwrite current state or drop merge-queue facts

**Feature Branch**: `fabrik/issue-2344`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Each poll writes its poll-start snapshot back over current state, and a cache hit wipes merge-queue membership. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.4 and §1.5.
>
> **Write-back.** `selectDeepFetchCandidates` applies `ItemDeepFetched{FreshState: board.Items[i]}` after every successful `FetchItemDetails` (engine/poll.go:2582), cache hit included. The deep fetch never reads Status (github/project.go:1003), and a cache hit (`copyDeepFieldsFromState`, boardcache/boardcache.go:1536) does not touch Status, labels or closed. So those come from the listing taken at the start of the poll, and `applyProjectItem` (internal/itemstate/project_apply.go) writes them unprotected. The same pattern is at engine/ci_settle.go:183, engine/poll_settle.go:53, engine/dependencies.go:200 and boardcache/boardcache.go:953.
>
> Scenario: a webhook move or label change, or a worker's own `LocalStatusUpdated`/`LocalLabelAdded`, lands after the listing and before the item's turn in the deep-fetch loop. The Store reverts it, a spurious `StatusChanged` fires, `LastSeenSourceUpdatedAt` takes the stale snapshot time, and the poll may dispatch on the reverted value.
>
> **Merge queue.** `snapshotToProjectItem` (boardcache.go:216) and `copyDeepFieldsFromState` omit `LinkedPRIsInMergeQueue`, `LinkedPRIsMergeQueueEnabled` and `LinkedPRMergeQueueEntry`. The write-back sets them false (project_apply.go:119). `prInMergeQueue` gates rebase and push (engine/item.go:780, engine/comments.go:412, engine/merge_queue.go:56/75), so the engine can push to a PR in GitHub's merge queue and eject it.
>
> ## Requirements
>
> - **R1:** A deep-fetch apply writes only the facts the deep fetch (or cache hit) actually produced. Status, labels and closed from the poll-start listing are never written back by `ItemDeepFetched`; they reach the Store only through the listing paths and their existing protection.
> - **R2:** The same for every other `ItemDeepFetched` call site listed above.
> - **R3:** The cache-hit path round-trips every `LinkedPR` field the deep fetch carries, including the three merge-queue fields; a test enumerates the fields so a new one cannot be dropped silently.
> - **R4:** `LastSeenSourceUpdatedAt` is never moved backwards by a deep-fetch apply.
>
> ## Acceptance
>
> - A Store-level test: a write that lands between the listing and the deep-fetch apply survives the apply.
> - A cache-hit test: an item in GitHub's merge queue stays `IsInMergeQueue` after a poll served from cache.
> - A sim twin for the first; non-vacuity shown for both.

## Background

Each poll first takes a listing of the board (Status, labels, closed state), then deep-fetches the items that need work. The deep fetch gathers other facts (comments, linked-PR details, reviews and so on) and does not read Status. After a successful deep fetch, including one served from the cache, the engine applies the item's poll-start snapshot to the in-memory item state. That snapshot carries the listing's Status, labels and closed state, so the apply writes those values back without the protection the listing paths have against stale data.

If a change lands between the listing and the item's turn in the deep-fetch loop, the apply reverts it. Such a change can be a webhook move, a webhook label change, or the engine's own status or label write by a worker. The consequences are:

- A spurious status-changed event fires.
- The "last seen" source timestamp takes the stale snapshot's time.
- The poll may dispatch work on the reverted value.

The same write-back pattern exists at several other places where a deep-fetch result is applied (the CI settle scan, the poll settle path, dependency checking, and the board cache's own refresh).

A second, related defect: the cache-hit path does not carry the linked PR's merge-queue facts (in the merge queue, merge queue enabled, merge queue entry). The write-back therefore resets them to "not in queue". Those facts gate rebase and push decisions, so the engine can push to a PR that is in GitHub's merge queue and eject it from the queue.

Found by the engine state audit of 2026-10-15 (sections 1.4 and 1.5).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A change that lands mid-poll survives the deep-fetch apply (Priority: P1)

An operator or the engine itself changes an item's Status or labels (a board move, a label add or remove, a worker's own write) after the poll's listing was taken but before that item is deep-fetched. The change must still be in effect after the poll finishes processing the item.

**Why this priority**: This is the core correctness bug. Reverted state causes spurious events and dispatch on stale values.

**Independent Test**: At the item-state level, apply a Status, label or closed change after the listing snapshot, then apply a deep-fetch result built from that older snapshot, and check that the change is still present and no reverting status-changed event fires.

**Acceptance Scenarios**:

1. **Given** an item whose Status was changed after the poll-start listing, **When** the deep-fetch result for that item is applied, **Then** the item keeps the newer Status and no status-changed event reverts it.
2. **Given** a label added or removed after the poll-start listing, **When** the deep-fetch result is applied, **Then** the label set is unchanged by the apply.
3. **Given** an item closed after the poll-start listing, **When** the deep-fetch result is applied, **Then** the item stays closed.
4. **Given** the same sequence driven through a full sim poll, **When** the poll processes the item, **Then** it does not dispatch on the reverted value.

---

### User Story 2 - Merge-queue membership survives a cache-served poll (Priority: P1)

A linked PR is in GitHub's merge queue and the engine serves the item's deep data from its cache during a poll. The PR must still be recognised as in the merge queue afterwards, so the engine does not rebase or push to it.

**Why this priority**: Pushing to a PR in the merge queue ejects it, which is user-visible damage rather than just stale internal state.

**Independent Test**: Seed an item whose linked PR is in the merge queue, run a poll that is served from cache, and check that the item is still reported as in the merge queue.

**Acceptance Scenarios**:

1. **Given** a linked PR in the merge queue, **When** a poll serves the item from cache, **Then** the item is still in the merge queue and the merge-queue-enabled and entry facts are unchanged.
2. **Given** a linked PR in the merge queue, **When** the engine considers a rebase or push for it after a cache-served poll, **Then** it skips the rebase or push as it would after a live fetch.

---

### User Story 3 - Cache round-trip cannot silently drop a future field (Priority: P2)

A developer adds a new linked-PR field. If the cache-hit path does not carry it, a test fails.

**Why this priority**: The merge-queue bug existed because fields were dropped silently. This guards against repeats.

**Independent Test**: A test enumerates every linked-PR field the deep fetch produces and checks that a cache-served result carries each one.

**Acceptance Scenarios**:

1. **Given** a linked-PR field added without cache-hit support, **When** the test suite runs, **Then** the enumeration test fails naming the field.

---

### Edge Cases

- The listing is newer than the Store (for example the first time an item is seen): the listing paths still populate Status, labels and closed. This change must not stop listings from doing so.
- A cache hit on an item that is not in the merge queue: the merge-queue facts stay false and nothing changes.
- A deep-fetch apply carrying an older source timestamp than the one already recorded must not move it backwards.
- Each call site that applies a deep-fetch result (poll deep-fetch selection, CI settle scan, poll settle path, dependency checking, board cache refresh) behaves the same way.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Applying a deep-fetch result writes only the facts the deep fetch (or cache hit) produced. It never writes Status, labels or closed state taken from the poll-start listing.
- **FR-002**: Status, labels and closed state reach the item state only through the listing paths and their existing protection against stale data.
- **FR-003**: FR-001 and FR-002 hold at every place that applies a deep-fetch result: the poll deep-fetch selection, the CI settle scan, the poll settle path, dependency checking and the board cache's own refresh.
- **FR-004**: A deep-fetch result served from cache carries every linked-PR field a live deep fetch carries, including whether the PR is in the merge queue, whether the merge queue is enabled, and the merge-queue entry.
- **FR-005**: A test enumerates the linked-PR fields so that a field added later without cache-hit support fails the suite.
- **FR-006**: A deep-fetch apply never moves the recorded source-updated timestamp backwards.
- **FR-007**: A change to Status, labels or closed state made between the listing and the deep-fetch apply is still in effect after the apply, and the apply produces no reverting status-changed or label event.
- **FR-008**: An item whose linked PR is in the merge queue is still recognised as in the merge queue after a poll served from cache, so rebase and push decisions treat it as in the queue.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An item-state-level test shows a Status, label or closed change made after the listing survives the deep-fetch apply. The same test fails against the previous behaviour (non-vacuity).
- **SC-002**: A cache-hit test shows an item in the merge queue is still in the merge queue after a poll served from cache. The same test fails against the previous behaviour (non-vacuity).
- **SC-003**: A sim scenario reproduces the mid-poll change and shows the poll neither reverts it nor dispatches on the reverted value. It fails against the previous behaviour (non-vacuity).
- **SC-004**: The linked-PR field enumeration test fails when a field is added without cache-hit support.
- **SC-005**: No deep-fetch apply lowers the recorded source-updated timestamp.

## Assumptions

- The listing paths' existing protection against stale data is correct and sufficient, so Status, labels and closed state can rely on it alone.
- Facts a deep fetch does not produce (Status, labels, closed) should be left untouched by the apply. No new source for them is needed.
- The sim twin required by the acceptance criteria covers the first behaviour (mid-poll change survives). The merge-queue behaviour is covered by the cache-hit test.
- The audit document (`docs/design/engine-state-audit-2026-10.md`) is the source of the listed call sites. The list is treated as complete, and any further apply of a deep-fetch result found during implementation gets the same treatment.

## Out of Scope *(optional)*

- Other findings of the engine state audit not named in sections 1.4 and 1.5.
- Changing how the listing paths protect against stale data.
- Changing what the deep fetch itself reads from GitHub.
- Changing the rules for when the engine pushes or rebases for a PR in the merge queue, beyond making the input facts correct.

## Source References *(optional)*

- Engine state audit, `docs/design/engine-state-audit-2026-10.md` (2026-10-15), sections 1.4 and 1.5
