# ADR 2049: Display-Only "Last activity" and "Last run" Project Fields

**Date**: 2026-10-10
**Status**: Accepted
**Issue**: #2049 — board: display-only 'Last activity' and 'Last run' fields from values the engine already has
**Builds on**: [ADR 2048](2048-display-only-status-line-field.md), [ADR 042](042-mutation-echo-check.md), [ADR 035](035-four-layer-status-reconciliation.md), [ADR 040](040-job-started-at-work-boundary.md), [ADR 038](038-dual-store-observer-wiring.md)

Mentioned for context: #2043 (the report on the public triage board).

## Context

From the board you cannot sort or group items by how recently Fabrik worked on them, or by how their
last run ended. Spotting a stalled, thrashing or turn-exhausted item meant reading comments one issue
at a time, the daemon log or the TUI. #2048 added the first display-only board field and its writer.
This issue adds two more on that foundation, deliberately narrowed to values the engine already
computes and that are cheap to write.

## Decision

Two optional ProjectV2 fields, both on by default and created by the operator (Fabrik never creates
them):

| Field | Type | Config key (default name) | Value |
|---|---|---|---|
| Last activity | date | `project_fields.last_activity` (`Last activity`) | UTC calendar date of the latest job start or completion |
| Last run | text | `project_fields.last_run` (`Last run`) | one line from the latest finished run |

`off` (any case) disables a field independently. Keys are YAML-only, like `status_line`.

### No new state

Only values already carried by `JobStartedEvent` / `JobCompletedEvent` are used (stage, `IsComment`,
`Completed`, `TurnLimited`, blocked / errored flags, `TurnsUsed`, `MaxTurns`, duration, the start and
completion instants). No `ItemState` field, no persistence, no counter, no read-back. The only memory
is the writer's skip-unchanged record, in memory, forgotten on restart: after a restart the first run
for an item may rewrite an identical value once. This is documented, not fixed. Review-cycle and
ejection counters are **not** included; they would be a follow-up. Cost and tokens are out of scope.

### One generalised writer, not a parallel one

`statusLineState` becomes `displayFieldState`, and the engine holds three instances (status line, Last
activity, Last run). Each has its own lookup, single startup line, failed-lookup back-off and per-item
write lock, so one missing or failing field cannot affect another. A `displayFieldSpec` carries the
field kind; the date kind calls `FetchDateField` / `UpdateProjectItemDateField` (`value: {date: $date}`,
`Date!` = `YYYY-MM-DD`). The status-line helpers keep their signatures, and the existing status-line
tests pass unmodified. Two explicit client methods were chosen over a generic typed updater: they mirror
the text pair and change no existing signature. Both lookups share one GraphQL query call site.

### Hooks are call sites, not the InvocationObserver

Writes sit at the sites that already hold the stage, the project item and the usage: beside the two
per-item `JobStartedEvent` emissions (after the lock tie-break, so ADR 040's work-boundary placement is
kept) and beside the two real-run `InvocationRecorded` applies (`finalizeStageOutcome` and comment
review). Rejected: hooking the `InvocationObserver` (ADR 038). It would also fire for the Done-cleanup
`InvocationRecorded` — not a run — and overwrite the last real `Last run` with `Done · completed`
(contradicting "values stay as history"), it reads the board column at observation time rather than the
stage that ran, and it is tied to the TUI event shape. Merge-train batch events (repo-level), synthetic
`Skipped` completions and `TurnProgressEvent` (per turn) are never hooked. Nothing clears either field
at Done.

### Date semantics

`Last activity` is the **UTC** date of the event instant, so it never depends on the host's zone; a
date field has no time of day, so it has day granularity and several events on one day write once. Start
and completion both use `time.Now` (as the TUI events do). An early version read the completion date from
the engine clock seam; under an injected clock the two sources disagreed about the day and the value
flip-flopped, so the completion hook passes its instant explicitly.

### Wording of Last run

`<Stage>[ · comment review] · <outcome>[ · <used>/<budget> turns][ · <duration>]`, capped at 60
characters (runes) with `…`. Outcome precedence: `turn-limited`, `blocked on input`, `completed`,
`failed` (errored without completing), else `incomplete` — a clean exit without the marker or a
tools-denied run. A completion marker beats a non-zero exit, matching the rest of the engine. Turns are
shown for completed and turn-limited runs (`N turns` when the stage has no budget); duration only for
completed runs (`45s`, `18m`, `1h5m`).

### Cache and drift

The webhook layer is already generic: `applyProjectsV2ItemDelta` ignores any edit whose `field_type` is
not `single_select`, and a date edit is `date`. The parse layer is extended: the board and probe queries
select `lastActivity` (`ProjectV2ItemFieldDateValue { updatedAt }`) and `lastRun`
(`ProjectV2ItemFieldTextValue { updatedAt }`) by alias, `@include`-gated per field, and
`projectItemUpdatedAt` discounts the project item's `updatedAt` against the **latest** present
display-field value `updatedAt` (2 s tolerance). These fields are never cleared, so each bump has a value
node to compare against; the status-line clear at Done remains ADR 2048's known uncovered case. No webhook
echo is registered, for the reason in ADR 2048.

## Consequences

- Operators can sort and group the board by staleness and last outcome with no log reading.
- Two extra mutations at most per completed run, usually one (the date is unchanged within a day), on the
  calling goroutine, as for the status line.
- Same unverified assumption as ADR 2048: whether GitHub bumps a project item's `updatedAt` for a text or
  date write. The sim and the recorded fixtures model the conservative case. The new date fixtures are
  hand-authored to the vendored schema; `scripts/wire-contract/record-fixtures.sh` re-records them against
  a sandbox board that would need a `Last activity` date field added.
- Adding a `GitHubClient` method meant touching every implementation (engine mock, `cmd` test double, simgh
  and its instrumented wrapper).

## Alternatives rejected

- Hooking the `InvocationObserver` / TUI channel (see above).
- A second, parallel writer for the new fields (violates one-writer reuse, duplicates the lookup and
  back-off logic).
- Skipping cleanup events or zero-duration runs inside the observer: fragile.
- Writing in a goroutine to avoid the inline latency: reorders writes against the skip-unchanged record.
- Review-cycle / ejection counters and cost: out of scope for this issue.
