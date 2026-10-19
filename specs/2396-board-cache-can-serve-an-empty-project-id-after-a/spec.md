# Feature Specification: Board cache never serves or writes with an empty project ID

**Feature Branch**: `fabrik/issue-2396`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description: the original issue body, quoted verbatim below.

> ## Problem
>
> After a start, the board cache can serve a board whose `ProjectID` is empty, and every board write then fails. For example:
>
> `could not advance: updating status for item … on project : GraphQL error: Could not resolve to a node with the global id of ''`
>
> It lasts until a reconcile happens to find drift, or the daemon is restarted fresh.
>
> Reported in #2388. A self-upgrade re-exec happened while a GHES host was unreachable, and as the host came back, every advance failed until a manual restart.
>
> **Mechanism, from code:**
> 1. The cache's `projectID` is set only by `BootstrapFromProbe` and by the full `Reconcile`, and `reconcileTick` runs the full `Reconcile` only on drift.
> 2. If the virgin-cache probe fails but the board fetch straight after it succeeds, `CacheImpl.FetchProjectBoard` serves that first board from the fallback (`len(snaps) == 0`) without storing its ID. The board carries the real ID, so `statusField` and `e.boardProjectID` are set.
> 3. That poll's deep fetches fill the store.
> 4. From then on `IsBootstrapped()` is true, so the poll takes the probe branch, and `runProbeAndDeepFetch` discards the probe's project ID (`engine/terminal.go`, `probeItems, _, err :=`). `FetchProjectBoard` then builds the board from the cache with `ProjectID: ""`.
> 5. `advanceToNextStage` and `moveItemToStatus` use `board.ProjectID` only.
>
> ## Requirements
>
> - **R1:** The cache records the project ID from every board or probe it sees: the fallback board in `FetchProjectBoard`, every `runProbeAndDeepFetch` probe, and `LightReconcile` at zero drift. An empty incoming ID never overwrites a known one.
> - **R2:** No board mutation is sent with an empty project ID. The write path falls back to `e.boardProjectID` when the board's ID is empty. If both are empty it fails with a named error ("project ID unknown"), defers, and logs once per episode, and it never sends `''` to GitHub.
> - **R3:** `FetchProjectBoard` never serves a cached board with an empty `ProjectID` while the store has items. It falls back to GitHub for that poll instead.
> - **R4:** Sim twin: the first probe fails, the board fetch succeeds, and the deep fetch fills the store. The next poll's advance writes the real project ID. Neutralising R1–R3 reproduces the `''` write.
>
> ## Scope
>
> `boardcache` project-ID bookkeeping and the engine's board-write call sites. Startup-check failure handling is out of scope: that check does not feed the ID.
>
> ## Acceptance
>
> - The sim scenario above advances on the next poll.
> - No `UpdateProjectItemStatus` call with an empty project ID is possible (sim assertion over every call).
>
> Mention #2388 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) next to it.

## Background

After a daemon start, the board cache can end up serving a board whose project ID is empty. Every board write that needs the project ID (moving an item's Status, for example) then fails with a GraphQL "could not resolve to a node with the global id of ''" error. The failure is silent in the sense that nothing repairs it. It persists until a reconcile happens to detect drift, or until the daemon is restarted fresh.

It was reported in #2388. A self-upgrade re-exec happened while a GHES host was unreachable. As the host came back, every advance failed until an operator restarted the daemon manually.

The mechanism, as established from code by the reporter:

1. The cache records the project ID only through its bootstrap-from-probe path and through the full reconcile. The periodic reconcile runs the full path only when it detects drift.
2. If the first (virgin-cache) probe fails but the board fetch immediately after it succeeds, the cache serves that first board from the fallback path without storing the ID it carried. The engine does learn the real ID from that board, so its own copy of the ID and the status field are set.
3. That poll's deep fetches populate the item store.
4. From then on the cache counts as bootstrapped, so subsequent polls take the probe path. That path discards the project ID the probe returned. The board the cache then builds from its own state has an empty project ID.
5. The engine's advance and move-to-status writes read the project ID from the board only, so they send the empty value.

Two defects combine here. The cache fails to remember an ID it was shown. The write path trusts the board's ID without checking it, even though the engine holds a good copy of its own.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Advances keep working after a degraded first poll (Priority: P1)

An operator's daemon restarts (for example after a self-upgrade) while GitHub is partly unreachable. The first probe fails, but the board fetch that follows succeeds. The daemon must advance items on the following polls with no manual restart.

**Why this priority**: This is the reported incident. Without it the pipeline stalls completely and needs human intervention.

**Independent Test**: In the sim bed, make the first probe fail, let the board fetch succeed and the deep fetch fill the store, then run the next poll with an item ready to advance. The advance must succeed and carry the real project ID.

**Acceptance Scenarios**:

1. **Given** a virgin cache whose first probe failed and whose board fetch succeeded, **When** the next poll advances an item, **Then** the Status update is sent with the real project ID and succeeds.
2. **Given** the same start-up, **When** any later poll runs, **Then** no board write is sent with an empty project ID.

---

### User Story 2 - A missing project ID can never reach GitHub (Priority: P1)

Whatever state the cache is in, the engine must not send a board mutation naming an empty project. If the ID truly cannot be determined, it must refuse clearly and try again later.

**Why this priority**: This is the safety net. It protects against this bug class even if some other path ever fails to record the ID.

**Independent Test**: Force both the board's project ID and the engine's own copy to be empty, then attempt an advance. A named error is returned, the write is deferred, and no mutation reaches the GitHub fake.

**Acceptance Scenarios**:

1. **Given** the board's project ID is empty and the engine knows the real ID, **When** a board write is attempted, **Then** the engine's known ID is used and the write succeeds.
2. **Given** both IDs are empty, **When** a board write is attempted, **Then** it fails with an error that names "project ID unknown", sends nothing to GitHub, and is deferred for a later poll.
3. **Given** both IDs stay empty across several polls, **When** the write is retried each poll, **Then** one log line is emitted per episode, not one per attempt.

---

### User Story 3 - The cache self-heals its project ID (Priority: P2)

The cache learns the project ID from every board or probe result it sees, and never forgets or blanks one it already knows.

**Why this priority**: This removes the root cause. The write-path guard in User Story 2 only contains the damage.

**Independent Test**: Feed the cache a fallback-served board, a probe result and a zero-drift light reconcile, each carrying the ID. Then feed it results with an empty ID. The cache's ID is set by the first group and unchanged by the second.

**Acceptance Scenarios**:

1. **Given** a cache with no project ID, **When** it serves a board from the fallback path, **Then** it records that board's project ID.
2. **Given** a cache with no project ID, **When** a probe returns an ID, **Then** it is recorded.
3. **Given** a cache with no project ID, **When** a light reconcile finds zero drift and carries an ID, **Then** it is recorded.
4. **Given** a cache that knows its project ID, **When** a board, probe or reconcile arrives with an empty ID, **Then** the known ID is kept.

---

### User Story 4 - A cached board is never served without a project ID (Priority: P2)

If the cache holds items but has no project ID, it must not hand the engine a board with an empty ID. It refreshes from GitHub for that poll instead.

**Why this priority**: This is a defence in depth that makes the bad board unobservable to every caller, not only the write paths.

**Independent Test**: Put items in the store, leave the cache's project ID empty, and request the board. The result comes from GitHub and carries a non-empty ID.

**Acceptance Scenarios**:

1. **Given** the store has items and the cache's project ID is empty, **When** the board is requested, **Then** it is fetched from GitHub for that poll and carries the real ID.
2. **Given** the cache's project ID is known, **When** the board is requested, **Then** it is served from the cache as before.

---

### Edge Cases

- The GitHub fallback fetch also fails while the cache has no ID. The poll fails as it does today, and nothing is written.
- The store is empty and the cache has no project ID, as in a genuinely fresh start. Behaviour is unchanged. Only a non-empty store triggers the fallback in FR-003.
- A probe, board or reconcile arrives with an empty ID after the ID is known. The known ID wins.
- The project ID legitimately changes (the daemon is pointed at a different board). That requires a restart, which constructs a fresh cache. This spec does not add in-place replacement.
- Several items fail to advance in the same poll while the ID is unknown. Each is deferred, and only one log line appears for the episode.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The board cache MUST record the project ID from every board or probe result it observes: a board served from the fallback path, every probe in the probe-and-deep-fetch loop, and a light reconcile that finds zero drift.
- **FR-002**: The board cache MUST NOT overwrite a known project ID with an empty one.
- **FR-003**: When the store holds items and the cache's project ID is empty, the board request MUST NOT be served from the cache with an empty project ID. It MUST be fetched from GitHub for that poll.
- **FR-004**: No board mutation MUST be sent to GitHub with an empty project ID. Mutations that take the project ID from the board (advancing an item, moving an item to a Status) MUST use the engine's own known project ID when the board's is empty.
- **FR-005**: When both the board's project ID and the engine's own are empty, the write MUST fail with an error that names "project ID unknown", MUST send nothing to GitHub, and MUST be deferred to a later poll.
- **FR-006**: The "project ID unknown" condition MUST be logged once per episode, not once per attempt, and the episode MUST end when a project ID becomes known.
- **FR-007**: A sim scenario MUST reproduce the incident: first probe fails, board fetch succeeds, deep fetch fills the store, next poll advances an item. It MUST pass with the fix and MUST reproduce the empty-ID write when FR-001 to FR-003 are neutralised.
- **FR-008**: The sim bed MUST be able to assert, across every Status-update call it receives, that none carried an empty project ID.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a start where the first probe fails and the board fetch succeeds, the next poll's advance succeeds with the real project ID and needs no restart or reconcile.
- **SC-002**: Across the full sim suite, zero Status-update calls carry an empty project ID.
- **SC-003**: With FR-001 to FR-003 neutralised, the new sim scenario fails by reproducing the empty-ID write, so the scenario demonstrably guards the fix.
- **SC-004**: When no project ID can be determined, zero mutations reach GitHub and the operator sees exactly one "project ID unknown" log line per episode.

## Assumptions

- The project ID is stable for the life of a daemon process. Changing boards means a restart, which builds a fresh cache.
- The engine's own copy of the project ID, set when it sees a real board, is trustworthy as a write-path fallback.
- A one-poll fallback to GitHub for a cache that holds items but no ID is acceptable. It is rare and self-limiting, because the fallback board's ID is then recorded under FR-001.
- Write sites that already take the project ID from somewhere other than the board need no change.

## Out of Scope

- Startup-check failure handling. That check does not feed the project ID, so it is not part of this defect.
- Changing how the reconcile decides when to run, for example running the full reconcile more often.
- Replacing a known project ID in place when the board changes.
- Reworking probe error handling beyond retaining the ID the probe returns.

## Source References

- #2388: the original incident report (a self-upgrade re-exec with an unreachable GHES host). Mentioned for context only.
