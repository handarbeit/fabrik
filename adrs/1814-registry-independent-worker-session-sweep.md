# ADR 1814: Registry-independent worker-session sweep

## Status

Accepted. Extends [ADR-1798](1798-session-scoped-descendant-reaper.md) additively; nothing in that
ADR is superseded. Complements [ADR-063](063-worktree-cwd-rooted-process-reaper.md) and leaves
[ADR-054](054-sigint-kill-escalation-and-reason-propagation.md)'s kill escalation and `Setsid: true`
untouched. Rides the [ADR-055](055-worktree-janitor.md) janitor cadence.

## Context

ADR-1798's session-scoped reaper removed most orphan leakage, but orphans were still observed after
it was deployed (three occasions on 2026-09-18/19: live processes with `PPID 1`, a dead session
leader, and an empty `.fabrik/state/descendants.json`).

**Registry-driven discovery alone was insufficient, and that is structural, not an implementation
bug: a descendant that was never recorded is invisible to every reap path.** Both existing paths
start from a registry entry. `reapTrackedDescendants` loads entries *for that worker*;
`sweepStaleDescendants` loads the whole registry. Neither can find a live orphan that has no entry.
An entry only exists if `trackWorkerDescendants`'s `descendantScanInterval` tick (3s) saw the
process, its fingerprint call succeeded, and the upsert succeeded. A descendant spawned in the last
seconds before its worker exits, or during a transient `ps` failure, is never recorded, and nothing
ever reaps it. Shrinking the interval narrows the window but cannot close it. This assumption —
"the registry knows every descendant" — must not be re-adopted.

The information needed to find these orphans was always available: each is a live process whose
session ID (`unix.Getsid`) equals the PID of its dead worker, and `Setsid: true` guarantees every
descendant carries that PID for life. Nothing consulted it, because discovery was registry-driven.

## Decision

Invert the model from N racing per-descendant records to one durable record per invocation.

1. **Worker record at spawn.** `runClaude` writes a `workerRecord` (`.fabrik/state/workers.json`)
   synchronously after `cmd.Start()`, when the worker's PID belongs to exactly one process. It holds
   the worker's `comm`/`lstart` fingerprint, issue/repo/stage and `SpawnedAt`. If the fingerprint
   lookup fails it is left empty and retried in the background (`backfillWorkerFingerprint`).
2. **Registry-independent sweep by SID.** `sweepOrphanedWorkerSessions` enumerates the process table,
   calls `Getsid` on each entry once, and reaps any process whose SID equals the PID of a recorded
   worker that is confirmed dead. Ownership is by session lineage only: no `comm`/args allow-list,
   so a Bash-tool shell with zero children is reaped like any test binary (R3).
3. **Two call sites.** At invocation end (`sweepWorkerSessionAtInvocationEnd`, after
   `reapTrackedDescendants`) using a **fresh, uncached** scan — a cached snapshot could predate the
   very descendant that raced (R5); and periodically from `runProcessSweepJanitor` (the existing
   `JanitorIntervalHours` cadence, no new knob), reading all records from disk so an orphan from a
   previous engine run is still reapable (R4). The periodic sweep uses `sharedProcessTableScan`:
   every candidate is re-verified before any kill, so staleness can only cause a miss.
4. **Additive.** `trackWorkerDescendants`, `reapTrackedDescendants`, `sweepStaleDescendants` and
   `descendants.json` are unmodified (R6). Records live in a **separate file** with their own mutex
   (never held together with `descendantRegistryMu`); wrapping `descendants.json` would have broken
   its bare-array schema and the #1798 tests. The janitor logs a **second** summary line rather than
   changing the existing one.

### Ownership and fail-open rules (R2, R8, R9)

A process is signalled only when it is provably a descendant of a dead recorded worker. A dead
session leader alone is not evidence: arbitrary user processes have them. No record, no kill.

Records are identified by a unique ID (`<pid>-<unixnano>`), never by PID, so a recycled PID's new
worker cannot overwrite the dead worker's record.

`classifyWorker` decides per record; every ambiguity fails open:

| Worker PID state | Result |
|---|---|
| not alive | dead: sweep members |
| alive, fingerprint lookup errors | inconclusive: skip, keep record |
| alive, record fingerprint empty | skip (R9) |
| alive, `lstart` equal | live worker: skip |
| alive, `lstart` differs | recycled PID: sweep with the ordering discriminator |

Only `lstart` decides "recycled"; a `comm`-only change with equal `lstart` is the same worker,
since `comm` can legitimately change over a live process's life and misreading that would kill a
live invocation's descendants.

**PID reuse (R8).** A recycled PID that is now a new worker's session leader shares its SID value
with the dead worker's orphans, so SID equality alone cannot distinguish them. A member is reaped
only if its start time is `>=` the recorded worker's and strictly `<` the current holder's (a
process older than the holder cannot descend from it). Equal-second timestamps and unparseable
`lstart` skip the member. `ps` `lstart` has 1-second resolution, so the discriminator is
conservative by construction. The leader itself is never a candidate.

**Per-candidate re-verification** immediately before `SIGKILL`: a fresh fingerprint lookup must
succeed, `Getsid` must still equal the worker PID, and the start-time bounds must hold. A single
PID is signalled, as in ADR-1798; every descendant carries the worker's SID, so each is discovered
independently.

**Record lifecycle (R9).** A record is pruned only when the worker is confirmed dead, a
*successful* scan found zero live members, and nothing was skipped as inconclusive. A process can
only acquire SID `W` by forking from a member, so once the leader is dead and no member remains,
none can reappear. `ps` has no completeness signal for a truncated table, so pruning requires two
consecutive clean scans: at invocation end, two back-to-back fresh rescans; in the periodic sweep,
`EmptyScans >= 2` persisted across passes. A scan error or any skip resets the count and keeps the
record. The invocation-end sweep rescans within a short bound (2s) to catch members still visible
after `SIGKILL` and members forked between scan and kill; leftovers are caught by the periodic pass.

**Empty fingerprint.** If the worker's fingerprint cannot be captured at spawn, the record never
causes a signal while the PID is alive. Once the PID is dead it is swept, but members must have
started at or after `SpawnedAt - 1s`.

## Consequences

- A descendant spawned in a worker's final seconds, or during a transient `ps` failure, is reaped at
  invocation end (AC1); an orphan with no registry entry, including one from a previous engine run,
  is reaped within one janitor interval (AC2).
- Cost: one synchronous `ps` fingerprint and one small atomic write at each invocation start, two
  fresh process-table scans at invocation end, and one `Getsid` per live process per janitor cycle
  (O(processes), not O(processes x records)). Write failures are logged and non-fatal and degrade to
  ADR-1798's behaviour.
- Tests: about 50 existing tests drive `runClaude` without `t.Chdir`; a package-level
  `workerRecordsPathOverride`, set in `TestMain`, keeps their records out of the package directory
  where stale records with recycled PIDs could otherwise leak into later sweeps.
- AC6 ("no orphan with a dead session leader after a sim run") is exercised by a real-`runClaude`
  integration test with a `Getsid` enumeration as the oracle, not the sim bed: `simclaude` never
  goes through `runClaude`, so it has no worker, no `Setsid` and no SID sweep to observe.

### Known gaps (fail open, by design)

- **Orphans that predate worker records** have no record and are not covered.
- **Descendants that call `setsid()` themselves** diverge in SID permanently. The ADR-1798 gap; ADR-063
  covers it only partially.
- **Other Fabrik instances' orphans**, or orphans of workers this instance never recorded. Each
  daemon reaps its own; the 56-shell incident (a different daemon on a pre-#1798 binary) is not
  addressed.
- **Empty-fingerprint residual.** If the fingerprint could not be captured and the worker's PID is
  later recycled by a *non-Fabrik* session leader that then exits, its members that started after
  `SpawnedAt - 1s` would match. The synchronous capture, the background backfill and the `SpawnedAt`
  bound keep the window small but do not eliminate it.
- **Corrupt `workers.json`.** An unparseable file is renamed to `workers.json.corrupt` (logged) and
  the sweep continues with an empty set, so new workers are recorded again instead of every append
  and janitor pass failing forever. The records it held are lost, so their orphans are not reaped.
- **Live record whose PID is held by an unrelated process with no fingerprint** is never pruned
  while that process lives.
- **Fork during sweep.** Narrowed by the bounded rescan, not eliminated at invocation end; the
  periodic pass is the backstop.
- **Records lost** if `.fabrik/state/` is wiped while orphans still live.
- **Platform note:** Linux does not reuse a PID while a session or process-group member holds it, so
  the recycled-PID branch should be near-impossible there; macOS is not confirmed to behave the same,
  which is why the branch exists.
