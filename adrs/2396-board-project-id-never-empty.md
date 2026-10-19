# ADR 2396: The board project ID is never empty at a write

## Status

Accepted.

## Context

After a start whose first (virgin-cache) probe failed but whose board fetch succeeded, `CacheImpl.FetchProjectBoard` served that board from the fallback without recording its project ID. Once the poll's deep fetches filled the store, the cache counted as bootstrapped, `runProbeAndDeepFetch` discarded the ID each probe returned, and `FetchProjectBoard` built every board from the cache with `ProjectID: ""`. Every board write that read `board.ProjectID` then failed with "Could not resolve to a node with the global id of ''" until a reconcile found drift or the daemon restarted (#2388 reported it after a self-upgrade re-exec against an unreachable GHES host). Two defects combined: the cache forgot an ID it had been shown, and the write path trusted the board's ID although the engine kept its own copy (`Engine.boardProjectID`).

## Decision

1. **Record from everywhere, never blank.** One guarded recorder (`CacheImpl.noteProjectID`, exported as `NoteProjectID`) is called for the fallback board in `FetchProjectBoard`, each probe in `runProbeAndDeepFetch`, `LightReconcile` after its fetch, `BootstrapFromProbe` and `Reconcile`. An empty ID is ignored, so a known ID is never overwritten by an empty one. A different non-empty ID still replaces: a changed board needs a restart, which builds a fresh cache.
2. **Never serve an empty ID.** When the store holds items and the cache has no ID, `FetchProjectBoard` returns the GitHub board for that poll (recording its ID) instead of a cached board. It gates on "store non-empty and ID empty", not on a new readiness flag (ADR 1772). The store is not reconciled from that board.
3. **Guard the write path.** `Engine.writeProjectID(board)` returns the board's ID, else `Engine.boardProjectID`, else `errProjectIDUnknown`; every board mutation that takes the ID from the board uses it, and `pollCycle` also fills an empty `board.ProjectID` from the engine's copy. The unknown error is a **deferral**: settle scans do not spend `MaxRetries`, `recordAdvanceOutcome` writes no marker or comment, and nothing escalates to `fabrik:paused`. It is logged once per episode through an explicit flag cleared when an ID is next resolved (`logfThrottled` re-emits every 30 s and cannot express an episode).
4. **Per-site helper, not a client wrapper.** A `GitHubClient` decorator would make an empty-ID call structurally impossible, but embedding the interface hides the optional interfaces code type-asserts on `e.client`, and a decorator outside the sim's `Instrumented` log would make the sim's "no empty-ID write" assertion vacuous. `TestBoardWritesTakeProjectIDFromTheGuard` keeps a future site from reading `board.ProjectID` directly.

## Consequences

- A degraded start no longer strands every advance. The cache ID is now set after such a start, so the Layer 2 status sweep runs and `projects_v2_item` webhook deliveries are no longer ignored — the intended end state, previously off silently.
- A virgin-probe branch that sees zero items still does not bootstrap and records nothing itself (the empty-store guard for `BootstrapFromProbe` is unchanged). The board fetch that follows in the same poll goes through the `FetchProjectBoard` fallback, which records the ID it carries — necessarily so, since that is exactly the incident path (store empty at fetch time, filled by the deep fetch afterwards). The next poll therefore takes the probe-refresh branch (`IsBootstrapped() || ProjectID() != ""`) against a possibly empty store; `runProbeAndDeepFetch` handles that (its 0-item guard applies only when the cache is bootstrapped, and unseen items are seeded and deep-fetched as new), and the same state already arose after any `Reconcile`. Recording only for a non-empty store was rejected: it would skip the incident case.
- Test seams (per instance): `CacheImpl.SetProjectIDRecordingDisabledForTest`, `CacheImpl.SetEmptyIDBoardGuardDisabledForTest`, `Engine.SetProjectIDWriteGuardDisabledForTest`. The sim twin (`tests/sim/project_id_empty_test.go`) shows the empty-ID write when all three are set, and every sim `Env` cleanup fails on any empty-ID `UpdateProjectItemStatus`.
