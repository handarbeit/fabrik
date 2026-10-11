# Feature Specification: A reconcile must not undo the engine's own recent writes

**Feature Branch**: `fabrik/issue-2159`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description:

> ## Problem
>
> A light reconcile can overwrite the engine's own label writes in the store with a board listing fetched just before GitHub reflected those writes.
>
> `reconcileTick` (`engine/reconcile.go`) calls `LightReconcile`, which fetches a fresh board. On any drift it calls `cacheImpl.Reconcile(freshBoard)`, which replaces cached state with the listing. GitHub's board listing lags writes by seconds; the gate's board-lag probe (#1974) exists to measure exactly that. A reconcile that runs 1–2 s after an engine label write therefore sees the old labels, counts the difference as drift, and puts the old value back. The cache stays wrong until the next reconcile, about 3 minutes later.
>
> Observed on the dev daemon on 2026-10-11 (`dev(3d0a73f)`), reported by the MCP in-situ test session:
>
> | Time (Z) | Item | Engine write | Reconcile | Cache afterwards |
> |---|---|---|---|---|
> | 14:57:02 | #2091 | removed `fabrik:awaiting-ci` | 14:57:03 | label back |
> | 15:03:02 | #2097 | applied `fabrik:awaiting-landing-verification` | 15:03:03 | label missing |
> | 15:18:01 | #2116 | applied `stage:Review:complete` | 15:18:02 | label missing |
>
> Consequences:
> - During each window, every gate reading the cache sees the wrong state. For example, #2116 looked Review-incomplete, and `settleLandingVerification` could have skipped #2097.
> - The channel stream (#1968) emits a false label event with actor `unknown` for each revert.
> - At the 16:22Z shutdown, the engine tried to remove `fabrik:locked:<id>` from 11 issues that GitHub said no longer carried it (404). The cache was holding labels already removed on GitHub, which is probably the same mechanism.
>
> ## Requirements
>
> - **R1 — An engine write is not undone by an older listing.** The store must not let a reconcile replace a label (or other field) that the engine itself wrote more recently than the listing was fetched. One possible approach is to record per-item, per-label engine write times and have `Reconcile` keep a newer local write until a listing fetched after that write confirms or contradicts it. Plan chooses the mechanism.
> - **R2 — Drift detection discounts the same writes**, so an item whose only "drift" is a recent engine write does not mark the stream unhealthy or trigger a reconcile.
> - **R3 — Real drift still reconciles.** A change made outside the engine (a human removing a label in the UI, a missed webhook) is still corrected by the next reconcile, as today.
> - **R4 — No false channel events.** A reconcile that would only revert a recent engine write emits no label event.
> - **R5 — Docs.** Update `docs/state-machine.md` where reconcile and the cache are described.
>
> ## Acceptance
>
> - Unit/sim test: an engine label write followed by a reconcile against a pre-write listing leaves the cache with the written value and emits no channel event. It fails today.
> - Test: a later listing that genuinely contradicts the write (an external removal after the write) is applied.
> - Test: shutdown lock-label removal no longer targets labels the engine already removed.
> - The full test suite passes.

## Background

The engine keeps an in-memory cache of board state, including each item's labels. Labels are the engine's durable state machine, so gates (stage completion, landing verification, CI wait, locks) read the cache to decide what to do next. Besides webhooks, a periodic light reconcile fetches a fresh board listing, compares it to the cache, and on any difference replaces the cached state with the listing.

GitHub's board listing lags writes by seconds. A reconcile that fetches the listing shortly after an engine label write therefore receives a listing that predates the write. It sees the cache and the listing disagree, treats this as drift, and restores the stale value. The cache then stays wrong until the next reconcile, about 3 minutes later.

This was observed three times on the dev daemon on 2026-10-11, each time within about one second of an engine write (a removed `fabrik:awaiting-ci`, an applied `fabrik:awaiting-landing-verification`, an applied `stage:Review:complete`; items #2091, #2097, #2116). The consequences are:

- For the length of the window, every gate reading the cache sees the wrong state. #2116 looked Review-incomplete, and `settleLandingVerification` could have skipped #2097.
- The channel stream emits a false label event with actor `unknown` for each revert.
- At shutdown, the engine tried to remove `fabrik:locked:<id>` from 11 issues that GitHub said no longer carried it (404), which is probably the same mechanism.

The problem is a stale reading overriding a newer fact the engine already holds. The listing is authoritative about changes made outside the engine, but not about the engine's own writes made after the listing was taken.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An engine write survives a reconcile with an older listing (Priority: P1)

An operator running the daemon needs the engine's own label writes to stick. When the engine adds or removes a label and a reconcile runs shortly afterwards with a listing fetched before GitHub reflected that write, the cache must keep the written value.

**Why this priority**: This is the defect. Gates acting on reverted state can skip work or repeat it, and the false state lasts about 3 minutes.

**Independent Test**: Make an engine label write (add and, separately, remove), then run a reconcile against a listing that predates the write. Confirm the cache holds the written value.

**Acceptance Scenarios**:

1. **Given** the engine has just applied label L to an item, **When** a reconcile runs with a listing fetched before that write that lacks L, **Then** the cache still holds L.
2. **Given** the engine has just removed label L from an item, **When** a reconcile runs with a listing fetched before that write that still carries L, **Then** the cache still lacks L.
3. **Given** a listing fetched after the engine's write that reflects it, **When** a reconcile runs, **Then** the cache agrees with it and nothing further is held back.

---

### User Story 2 - Real drift is still corrected (Priority: P1)

A human removes a label in the GitHub UI, or a webhook is missed. The next reconcile must still correct the cache. Protecting engine writes must not turn the cache into something that ignores GitHub.

**Why this priority**: Without this, the fix for User Story 1 would swap one wrong-cache bug for another.

**Independent Test**: Make an engine write, then have an external actor change the same label afterwards. Reconcile against a listing fetched after the external change and confirm the cache takes the listing's value.

**Acceptance Scenarios**:

1. **Given** the engine applied label L, **When** a human removes L afterwards and a reconcile runs with a listing fetched after the removal, **Then** the cache lacks L.
2. **Given** a label differs between cache and listing and the engine never wrote it, **When** a reconcile runs, **Then** the cache takes the listing's value, exactly as today.
3. **Given** the engine wrote a label and a listing fetched after the write contradicts it, **When** a reconcile runs, **Then** the listing wins.

---

### User Story 3 - Drift detection and the event stream ignore what the reconcile would not apply (Priority: P2)

An operator or channel subscriber should not see signs of drift that is only a recent engine write. The stream should not turn unhealthy or trigger a reconcile for it, and no label event should be emitted for a revert that does not happen.

**Why this priority**: The cache fix stays incomplete if the health state and event stream still react to the discounted difference.

**Independent Test**: Make an engine write, run the drift check against a pre-write listing, and confirm it reports no drift and the stream stays healthy. Run a reconcile and confirm no label event is emitted.

**Acceptance Scenarios**:

1. **Given** an item whose only difference from the listing is a recent engine label write, **When** drift detection runs, **Then** it reports no drift, the stream stays healthy, and no reconcile is triggered.
2. **Given** the same state, **When** a reconcile runs for some other reason, **Then** no label event is emitted for that item.
3. **Given** an item that also has a genuine external difference, **When** drift detection runs, **Then** drift is reported for that difference.

---

### User Story 4 - Shutdown lock cleanup uses accurate state (Priority: P2)

At shutdown the engine removes the lock labels it holds. It must not try to remove labels it has already removed, which GitHub reports as 404.

**Why this priority**: This is the third observed symptom. It is probably a consequence of User Story 1 and verifies that the cache holds true state.

**Independent Test**: Make the engine remove a lock label, run a reconcile against a pre-removal listing, then run shutdown cleanup. Confirm it does not target that label.

**Acceptance Scenarios**:

1. **Given** the engine removed `fabrik:locked:<id>` from an item and a reconcile then ran with a pre-removal listing, **When** shutdown cleanup runs, **Then** it makes no removal call for that item.

---

### Edge Cases

- A listing fetched after the write that still lacks the write (GitHub lag longer than expected, or an external change) is treated as the later fact. Only a listing fetched before the write is discounted.
- Several engine writes to the same label in quick succession (add, then remove) are protected as the latest write.
- An engine write to one label must not protect other labels on the same item from reconciling.
- A daemon restart loses the record of recent writes. This is acceptable: the first reconcile after a restart behaves as today.
- A protected write must not stay protected forever. It stops mattering once a later listing has been seen, so a lost webhook cannot leave the cache stale indefinitely.
- An engine write that failed on GitHub (the REST call errored) must not be protected as though it succeeded.
- Fields other than labels are covered by the same principle (R1 says "or other field"), but only where the engine writes them and a reconcile can currently revert them.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A reconcile MUST NOT replace a label value the engine wrote more recently than the instant the listing was fetched. The written value is kept.
- **FR-002**: The same protection MUST apply to other cached fields the engine writes and that a reconcile can currently revert, where practical, and the covered set MUST be stated in the docs.
- **FR-003**: A write is protected only until a listing fetched after the write has been applied. That listing confirms or contradicts the write, and in either case it wins.
- **FR-004**: Drift detection MUST apply the same discount as the reconcile. An item whose only difference from the listing is a recent engine write MUST NOT count as drift, MUST NOT mark the webhook stream unhealthy, and MUST NOT trigger a reconcile.
- **FR-005**: Differences not explained by a recent engine write (a human's label change, a missed webhook, a new or removed item) MUST still be detected as drift and applied by the next reconcile, as today.
- **FR-006**: A reconcile that keeps a recent engine write instead of reverting it MUST NOT emit a label event, and no false event with actor `unknown` may be emitted for it.
- **FR-007**: A failed engine write MUST NOT be recorded as a protected write.
- **FR-008**: After a reconcile, shutdown lock-label cleanup MUST NOT target a lock label the engine has already removed.
- **FR-009**: Tests MUST cover: (a) an engine add and an engine remove each surviving a pre-write listing, with no channel event; (b) a later listing contradicting the write being applied; (c) drift detection discounting a recent write while still reporting genuine drift; (d) shutdown cleanup not targeting an already-removed lock label. Test (a) MUST fail before the change.
- **FR-010**: `docs/state-machine.md` MUST describe the protection where reconcile and the cache are described, including the rule for when a protected write stops being protected. `docs/llms-full.txt` MUST be regenerated in the same change.

### Key Entities

- **Engine write**: A label change (or other cached-field change) the engine made on GitHub and mirrored into the cache, with the time it was made.
- **Listing fetch time**: The instant a board listing was requested, used to decide whether the listing can have seen a given engine write.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a test reproducing the observed sequence (engine write, then reconcile against a pre-write listing), the cache holds the written value for 100% of label writes (add and remove). The test fails before the change and passes after it.
- **SC-002**: No label event is emitted by a reconcile that only declines to revert a recent engine write.
- **SC-003**: A change made outside the engine after an engine write is reflected in the cache after the next reconcile, with no more delay than today.
- **SC-004**: An item whose only difference from the listing is a recent engine write produces no drift report, no unhealthy stream state and no extra reconcile.
- **SC-005**: Shutdown lock-label cleanup in the test makes zero removal calls for labels the engine had already removed.
- **SC-006**: The full test suite passes, with the race detector enabled.

## Assumptions

- The 404s at shutdown come from the same stale-cache mechanism as the other symptoms. If a test shows another cause, that is a separate finding rather than a blocker here.
- Protection is held in memory only and lost on restart. This is acceptable because it only restores today's behavior for the first reconcile after a restart.
- A listing's fetch time is known to the engine, so a write can be compared against it.
- Webhook echoes of engine writes are already idempotent no-ops and need no change.
- The mechanism (per-label write times, a generation counter, or another approach) is chosen in Plan, as the issue says.
- The ordinary poll path and webhook deltas are not part of this problem. They are not observed to revert engine writes, and the issue is scoped to reconcile.

## Out of Scope

- Changing how often reconcile runs, or replacing reconcile with another mechanism.
- Making the GitHub board listing itself fresher, or adding lag measurement beyond the existing board-lag probe.
- Persisting recent-write records across restarts.
- Reworking the channel event actor model beyond suppressing false revert events.
- Fixing any shutdown 404 that has a cause other than the stale cache.

## Source References

- Issue #2159 (the original report)
- #1974: board-listing lag probe
- #1968: channel stream
- `docs/state-machine.md`: reconcile and cache sections, webhook stream health states, channel label event actors
