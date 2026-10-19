# Feature Specification: Decisions read live, apply to the Store, and decide from the Store

**Feature Branch**: `fabrik/issue-2350`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "## Problem

Decisions read one thing and act on another. Part of the state foundation from the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15). It must land before a second webhook source (#2143). See §3.

- **Same-poll snapshots (§3.1):** `dispatchCandidates` (engine/poll.go:1871) runs `itemNeedsWork` on copies captured in `selectDeepFetchCandidates`; Phase 1/2 and the settle scans mutate the same items in between (pause, revalidate, remove a completion label). Only `Worker()` is re-read. Example: Phase 1 pauses an item at a cycle limit and the same poll still dispatches a comment pass on it.
- **Live reads not written back (§3.2):** `processItem`'s lock verify reads live labels and keeps only the lock (engine/item.go:1090); the feedback gate's live `FetchItemDetails` is discarded (engine/feedback_gate.go:48), and its gap repair (feedback_dispatch_view.go:70) covers threads and review bodies but not issue comments; `classifyLinkedPRTerminal` and `itemPRAlreadyLanded` read merged state live and leave the Store stale for up to 45 s.
- **Two review sources (§3.3):** `checkReviewGate` uses cached requests and reviews (engine/reviews.go:178), `reviewGateBlocksLanding` reads live (:779).
- **blockedBy (§3.4):** `checkDependencies` reads live only when the item is already blocked (engine/dependencies.go:147).

## Requirements

- **R1:** One helper for \"read live, apply to the Store, decide from the Store\", used by every gate that reads GitHub live.
- **R2:** `dispatchCandidates` re-reads each candidate from the Store after the poll's own writes, so a decision made earlier in the same poll is respected.
- **R3:** The advance and landing review gates read the same source.
- **R4:** The dependency check reads blockedBy freshly enough that a newly added blocker stops a dispatch (with #1995's webhook once it lands).

## Acceptance

- Tests: an item paused by Phase 1 is not dispatched in the same poll; a live read by one gate is visible to the next decision without a refetch; a reviewer requested after the cache filled blocks the advance.
- Depends on the ordering rule so applied live reads cannot be reverted."

## Background

The engine keeps a per-item in-memory Store as its picture of GitHub. Several decision points disagree with that picture, or with each other, in ways the engine state audit (`docs/design/engine-state-audit-2026-10.md`, §3) catalogued. Each is a case of a decision reading one source and acting on another:

- **Same-poll snapshots (audit §3.1).** The poll captures a copy of each candidate item early, then runs several phases (cycle-limit pauses, revalidation, completion-label removal, settle scans) that change the item. Dispatch finally decides on the early copy, re-reading only whether a worker is already running. Example: a phase pauses an item at its cycle limit and the same poll still dispatches a comment pass on it.
- **Live reads that are not written back (audit §3.2).** Several gates read GitHub live, decide, and throw the result away, so the next decision falls back to the stale cached value. Known cases: the lock verification at the start of item processing keeps only the lock label from its live label read; the feedback gate's live item read is discarded, and its gap repair covers review threads and review bodies but not issue comments; the "linked PR already merged" checks read the merged state live and leave the Store showing the PR as unmerged for up to about 45 seconds.
- **Two review sources (audit §3.3).** The gate that decides whether a stage may advance uses cached review requests and reviews; the gate that decides whether a PR may land reads them live. A reviewer requested after the cache filled is invisible to the advance and caught only at landing, wasting a Validate run.
- **blockedBy (audit §3.4).** The dependency check reads blockers live only when the item is already marked blocked. A blocker added to an unblocked item can go unseen, because no webhook carries the edge yet (#1995) and other cache effects can mask it. The landing re-checks live so a merge is guarded, but stage work is not.

This is part of the state foundation that must land before a second webhook source (#2143). It depends on the "observed at" ordering rule (#2348, merged), so that a live read applied to the Store cannot later be reverted by an older write.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A decision made earlier in the poll is respected at dispatch (Priority: P1)

An operator relies on a pause (for example at a cycle limit) to stop further work on an item. Once the engine has paused the item during a poll, no worker is dispatched for it in that same poll.

**Why this priority**: This is the only scenario where the engine takes an action that contradicts a decision it just made, with real cost (a worker run on a paused item).

**Independent Test**: In a sim poll, have an earlier phase pause an item that would otherwise be a dispatch candidate; assert no comment pass or stage run is dispatched for it in that poll.

**Acceptance Scenarios**:

1. **Given** an item that is a dispatch candidate at the start of a poll and has a pending comment, **When** an earlier phase of the same poll pauses it, **Then** dispatch does not start a worker for it in that poll.
2. **Given** an item whose completion label is removed (or whose revalidation is requested) by an earlier phase of the poll, **When** dispatch evaluates it, **Then** the decision reflects the label state after that phase.
3. **Given** an item no earlier phase touched, **When** dispatch evaluates it, **Then** behaviour is unchanged.

---

### User Story 2 - A live read by one gate is visible to the next decision (Priority: P1)

When a gate reads GitHub live, the result becomes part of the engine's state, so later decisions in the same poll and the following polls see it instead of regressing to a stale cached value.

**Why this priority**: This is the shared mechanism behind most of §3.1–3.4; without it each gate stays a one-off.

**Independent Test**: Make a gate perform a live read that differs from the cached state (a PR merged, an issue comment added, a label changed); assert that the next decision sees the live value and no second GitHub read is needed.

**Acceptance Scenarios**:

1. **Given** a linked PR that merged on GitHub while the Store still shows it open, **When** a gate reads its merged state live, **Then** the Store reflects the merge immediately and later decisions do not wait for the cache to age out.
2. **Given** the feedback gate's live read finds an issue comment the cache lacks, **When** the gate decides, **Then** the comment is applied to the Store and counted as feedback, as review threads and review bodies already are.
3. **Given** the lock verification's live label read shows other labels changed, **When** processing continues, **Then** those labels are applied to the Store, not only the lock.
4. **Given** a live read result older than a newer observation of the same fact, **When** it is applied, **Then** the ordering rule from #2348 drops it rather than reverting the newer value.

---

### User Story 3 - The advance gate sees reviewers the landing gate would see (Priority: P2)

A reviewer requested (or a review submitted) after the cache filled blocks the stage advance, rather than being discovered only when landing.

**Why this priority**: Closes a wasted-Validate-run gap; the landing already guards correctness, so impact is cost, not safety.

**Independent Test**: Request a reviewer on a PR after the cache has been populated and the stage has completed; assert the advance is held.

**Acceptance Scenarios**:

1. **Given** a completed stage with a review gate and a reviewer requested after the cache filled, **When** the advance gate runs, **Then** the advance is held.
2. **Given** the same PR state, **When** the landing gate runs, **Then** it reaches the same verdict from the same source of review state as the advance gate.

---

### User Story 4 - A newly added blocker stops dispatch (Priority: P2)

A blocker added to an item that is not currently marked blocked stops stage work on it, without waiting for a webhook that does not exist yet.

**Why this priority**: Stage work on an item with an open blocker is wasted and can mislead; the landing guard does not cover it.

**Independent Test**: Add a blockedBy edge on GitHub to an unblocked, cached item; assert that the next dispatch decision for it is blocked.

**Acceptance Scenarios**:

1. **Given** an unblocked item and a blocker added on GitHub after the item was cached, **When** the dependency check runs before dispatch, **Then** the item is marked blocked and no worker is dispatched.
2. **Given** a webhook for the edge exists (#1995) and has updated the Store, **When** the dependency check runs, **Then** it uses that data and the extra live read is not wasted beyond what the freshness rule requires.

---

### Edge Cases

- A live read fails (network error, rate limit): the gate keeps its existing fail-closed or fail-open behaviour; a failed read is never applied to the Store as an empty or "none" value.
- A live read returns data older than what the Store already holds (late response): it is dropped by the ordering rule and the gate decides from the Store as it now stands.
- Dispatch re-read of an item that has left the board, been closed, or been removed from the Store during the poll: it is not dispatched.
- An item changed by a webhook during the poll: dispatch sees the changed state.
- Engines without a board cache (the sim/`NewWithDeps` shape) behave the same as cached engines for these decisions.
- Applying a live read must not itself wake the poll loop or emit channel events that a poll-only write would not (no feedback loops).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: There is a single shared mechanism by which a gate performs a live GitHub read, applies the result to the item Store (subject to the observed-at ordering rule), and then makes its decision from the Store.
- **FR-002**: Every gate that currently reads GitHub live and discards or only partly keeps the result uses that mechanism. At minimum: the lock verification in item processing, the feedback gate, the linked-PR-terminal classification, the "PR already landed" check, and the landing review gate.
- **FR-003**: Dispatch re-reads each candidate's current state from the Store immediately before deciding whether it needs work, so changes made by earlier phases and settle scans of the same poll (pause, revalidation, label removal) are respected.
- **FR-004**: A candidate that, on the re-read, is paused, blocked, closed, no longer on the board, or otherwise no longer needs work is not dispatched; nothing is recorded against it as if it had been dispatched or deferred for lack of a slot.
- **FR-005**: The feedback gate's live read applies issue comments to the Store as it already does review threads and review bodies, so an issue comment seen only by the live read is processed rather than lost on the next cache read.
- **FR-006**: Merged or closed state of a linked PR learned from a live read is applied to the Store at once.
- **FR-007**: The stage-advance review gate and the landing review gate obtain review requests and reviews from the same source and apply the same freshness, so they cannot disagree on identical PR state.
- **FR-008**: A reviewer requested after the cache was populated holds the stage advance.
- **FR-009**: The dependency check reads the item's blockedBy edges live often enough that a blocker added to a not-yet-blocked item stops dispatch, whether or not a webhook delivers the edge; the cost is bounded (not an unconditional extra read on every item every poll).
- **FR-010**: A live read that fails is never applied as a value; the gate's existing behaviour on read failure is unchanged.
- **FR-011**: Applying a live read goes through the same ordering rule as every other Store write (#2348), so an older read cannot overwrite a newer fact and an applied read cannot be reverted by an older write.
- **FR-012**: Behaviour for items that no earlier phase touched, and for gates whose live read matches the Store, is unchanged.

### Key Entities *(if applicable)*

- **Item Store**: the engine's in-memory per-item state, the single place decisions are made from.
- **Live read**: a direct GitHub read performed by a gate to get fresher data than the Store holds.
- **Dispatch candidate**: an item admitted by the poll as possibly needing work, evaluated for dispatch after the poll's earlier phases.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An item paused by an earlier phase of a poll is never dispatched in that same poll (covered by a sim test that fails without the change).
- **SC-002**: After any gate performs a live read that differs from the cache, the next decision about that item, in the same poll, sees the live value with no additional GitHub call.
- **SC-003**: A reviewer requested after the cache filled holds the stage advance in a test that fails without the change.
- **SC-004**: A blocker added to an unblocked, cached item stops the next dispatch of that item in a test that fails without the change.
- **SC-005**: For items and polls where nothing changed mid-poll, the sequence of GitHub mutations is identical to before, and steady-state GitHub read volume increases only by the bounded blockedBy freshness read in FR-009.
- **SC-006**: A late (older) live read applied through the shared mechanism never overwrites a newer Store value (covered by a test using the ordering rule).

## Assumptions

- #2348 (the observed-at ordering rule) and #2349 are merged, as the dependency block states; this work builds on that rule rather than introducing its own ordering logic.
- The mechanism is internal engine behaviour; there is no operator-visible configuration or new label.
- For FR-009, "freshly enough" means a bounded-cost live read (for example, at the point work would be dispatched, and not on every poll for every item) rather than a per-poll read of all items; the exact cadence is a planning decision.
- Gates keep their existing outcomes (hold, pause, defer) on a stale-versus-live difference; this issue changes what they see, not what they do with it.
- Audit items §3.5 (READY check-run set trusted forever) is out of scope here; it is already covered by the CI suite hold.
- Where the dispatch re-read finds an item no longer needing work, silently skipping it (with a log line) is acceptable.

## Out of Scope *(optional)*

- Check-run freshness (audit §3.5) and the READY-set staleness.
- The `blockedBy` webhook itself (#1995) and the second webhook source (#2143, #2351, #2352).
- Persisting more engine-private state across restarts (#2349, done) and the ordering rule itself (#2348, done).
- The dispatch handlers still running on the poll-start `board.Items` copy beyond what is needed for the dispatch re-read (audit §3.1 as recorded in the existing deep-fetch note), unless required to satisfy FR-003.
- Changing gate policy (when to hold, pause or defer).

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md` §3 (3.1–3.4) and §6 item 11
- Issues #2348 (observed-at ordering rule), #2349 (durable decision state), #1995 (blockedBy webhook), #2143 (second webhook source)
