# Feature Specification: Display-only "Last activity" and "Last run" project fields

**Feature Branch**: `fabrik/issue-2049`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "board: display-only 'Last activity' and 'Last run' fields from values the engine already has

## Problem

From the board you can't sort or group items by how recently Fabrik worked on them, or by how their last run went. Spotting a stalled, expensive or thrashing item means reading comments one issue at a time, the daemon log or the TUI.

Report: #2043 (on the public triage board). Its scope is deliberately **narrowed** here: report only what the engine already has and what is cheap to write.

## Requirements

**R1. No new state.**
- Only write values the engine already computes for the TUI or the log: the `JobStartedEvent` / `JobCompletedEvent` / `TurnProgressEvent` fields, and counters already held in memory.
- No new tracking, persistence, read-back of board fields, or cumulative totals across restarts.
- A value that resets when the daemon restarts (e.g. an in-memory counter) shows what the engine currently holds. Document this, and don't fix it.

**R2. Candidate fields.** Plan picks the final set from what already exists. The expected set:

| Field | Type | Value (already available) |
|---|---|---|
| Last activity | date | the time of the most recent job start or completion for the item (`StartedAt` / `CompletedAt`) |
| Last run | text | one line from the most recent `JobCompletedEvent`, e.g. `Validate · completed · 42/250 turns · 18m`, or `Implement · turn-limited · 250/250 turns`, or `Review · blocked on input` |

- Only add more (e.g. the in-memory review-cycle or ejection counter) if it's already held and the write is trivial. Label it as current-session where it resets on restart.
- Cost in dollars or tokens is out of scope.

**R3. Rules (same as the status-line field from the previous issue).**
- Display only, never read back.
- Write on transition only, and skip unchanged values.
- Skip silently if the field is missing from the board.
- Reuse that issue's field writer and its webhook-echo suppression, so these writes cause no drift or deep fetch.

**R4. Config.**
- Add a `project_fields:` block mapping each logical field to a board field name, or `off`.
- Leave values in place at Done; they serve as history.

## Scope

- In scope: writing the chosen fields from existing TUI/log events through the shared field writer, plus config and docs.
- Out of scope: any new counters, persistence, cost tracking or gating.
- Mention #2043 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2043. This issue's own number gets `Closes`.

## Acceptance

- Unit tests:
  - a job completion writes `Last activity` and `Last run` from the event's existing fields;
  - an unchanged value writes nothing;
  - a missing field is skipped;
  - no new store fields or persistence are introduced (reviewable from the diff).
- `docs/USER_GUIDE.md` lists the fields, their sources and the restart caveat. Regenerate `docs/llms-full.txt` if a canonical doc changes."

## Background

From the project board there is no way to sort or group items by how recently Fabrik worked on them, or by how their last run ended. Finding a stalled, thrashing or turn-exhausted item means reading comments issue by issue, the daemon log or the TUI. The report is #2043 on the public triage board; its scope is deliberately narrowed here to values the engine already computes and that are cheap to write.

#2048 (merged) introduced Fabrik's first display-only board field: an optional ProjectV2 text field (`project_fields.status_line`, default name `Fabrik`) written through one general engine writer. That writer:

- writes only on a transition and only when the value differs from the last one it wrote for the item;
- skips silently, with one startup log line, when the field is absent, of the wrong type or switched `off`;
- logs and swallows a failed write;
- keeps its "last written" record in memory only;
- never reads anything back.

The board cache and reconcile drift check treat an edit of that field as a no-op, so a write causes no drift, deep fetch, invalidation or poll wake. This issue adds two more display-only fields on that foundation, with the same rules and no new engine state:

- **Last activity**: a date field.
- **Last run**: a one-line text field summarising the item's most recent finished run.

Both are projections of data the engine already emits for the TUI and log: `JobStartedEvent.StartedAt` and `JobCompletedEvent` (stage, outcome flags, turns, duration, completion time).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Sort and group the board by recent activity and last outcome (Priority: P1)

As a maintainer, I want each card to show when Fabrik last started or finished work on it and a one-line summary of how that run ended, so I can sort by staleness and spot items that ended turn-limited or blocked without opening anything.

**Why this priority**: This is the reported pain point and the whole deliverable.

**Independent Test**: With a test board that has a date field `Last activity` and a text field `Last run`, complete a stage job for an item and read both fields.

**Acceptance Scenarios**:

1. **Given** a stage job completes for an item, **When** its `JobCompletedEvent` is processed, **Then** `Last activity` holds the calendar date of the event's completion time and `Last run` holds a line built only from that event's fields (stage, outcome, turns used/budget, duration).
2. **Given** a stage job starts for an item, **When** its `JobStartedEvent` is processed, **Then** `Last activity` holds the date of the event's start time.
3. **Given** a run ended because the turn budget was exhausted, **When** it completes, **Then** `Last run` reads like `Implement · turn-limited · 250/250 turns`.
4. **Given** a run ended waiting for user input, **When** it completes, **Then** `Last run` reads like `Review · blocked on input`.
5. **Given** a run completed its stage, **When** it completes, **Then** `Last run` reads like `Validate · completed · 42/250 turns · 18m`.

---

### User Story 2 - The fields are quiet, optional and display-only (Priority: P1)

As an operator, I want the fields to cost nothing when absent, to write nothing when nothing changed, and never to influence engine behavior or the cache.

**Why this priority**: Without these guarantees the feature adds API traffic and reconcile churn.

**Independent Test**: Unit tests per scenario below, driven through the same test seams as the status-line writer tests.

**Acceptance Scenarios**:

1. **Given** the value to write equals the last one this process wrote for the item, **When** the next event fires, **Then** no write is made. A second event on the same calendar day therefore does not rewrite `Last activity`.
2. **Given** the board has no field of the configured name, or of the wrong type, or the config is `off`, **When** any event fires, **Then** nothing is written for that field, nothing is logged per event, and one startup log line per unavailable field says so.
3. **Given** a `projects_v2_item` edit of either field is received, or a reconcile runs after a write, **When** it is applied, **Then** it causes no drift count, no deep fetch, no item invalidation and no poll wake.
4. **Given** an item reaches Done, **When** the Done transition completes, **Then** `Last activity` and `Last run` keep their last values.
5. **Given** the daemon restarts, **When** the next event for an item fires, **Then** the fields may be rewritten once with an identical value (the in-memory record was lost), and no value is read back from the board.

---

### Edge Cases

- A comment-processing run (`IsComment`) is a job like any other: it updates `Last activity`, and `Last run` names it as comment review rather than as a stage run.
- A run that failed (no error-free exit) reads as `failed`; the line is still built from event fields only.
- Synthetic fallback `JobCompletedEvent`s (`Skipped: true`, emitted at deferral sites) do not represent a finished run and write nothing; only the authoritative event does.
- Merge-train batch job events are repo-level, not per-item, and write nothing to any item.
- `TurnProgressEvent` fires per turn and is not a write trigger; writing per turn is forbidden.
- A configured field exists but is the wrong type (text where a date is needed, or the reverse): treated as missing.
- A write fails (API error): logged and swallowed; the engine behaves as if it succeeded, and because the "last written" record is only updated on success the next event retries.
- `Last activity` is a date, so several events on one day produce one write.
- A line over about 60 characters is truncated with an ellipsis, exactly as for the status line.
- Review-cycle or ejection counters are not written (see Out of Scope); a restart-resetting counter would show only what the process currently holds.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Fabrik writes a ProjectV2 **date** field (logical name `last_activity`, default board field name `Last activity`) on each item, set to the calendar date of the most recent job start or job completion event for that item.
- **FR-002**: Fabrik writes a ProjectV2 **text** field (logical name `last_run`, default board field name `Last run`) on each item with one line built from the most recent authoritative (non-`Skipped`) `JobCompletedEvent` for that item: the stage (or comment review), the outcome (`completed`, `turn-limited`, `blocked on input`, `failed`), the turns used over the turn budget when a budget is known, and the run duration when the run completed. The examples in this issue's Input are the intended wording.
- **FR-003**: Values are derived only from fields already carried by `JobStartedEvent` / `JobCompletedEvent` (`StartedAt`, `CompletedAt`, `StageName`, `IsComment`, `Success`, `TurnLimited`, `Completed`, `BlockedOnInput`, `TurnsUsed`, `MaxTurns`, `Duration`). No new counters, timers, store fields or persistence are introduced, and `TurnProgressEvent` is not a write trigger.
- **FR-004**: Both fields are display-only: no engine decision reads their values back from the board, any cache, or the writer's own record of what it wrote (which exists only to skip unchanged writes).
- **FR-005**: A write is made only when the new value differs from the last value written for that item and field by this process; no write happens per turn or per poll.
- **FR-006**: Both fields are written through the existing status-line field writer's mechanism (field resolution, per-item serialisation, last-written record, error swallowing, self-write staleness handling), extended for a date-typed field, rather than through a second parallel writer.
- **FR-007**: Configuration is `project_fields.last_activity: <name>|off` and `project_fields.last_run: <name>|off` in `.fabrik/config.yaml`, alongside `project_fields.status_line`. An unset or blank key means the default name; `off` (any case) disables that field only. The keys are YAML-only, like `status_line`.
- **FR-008**: A field that is absent from the board, or present with the wrong type, makes every write for it a silent no-op, with exactly one startup log line per unavailable field. A missing field never affects the other field or the status line.
- **FR-009**: A failed write is logged and swallowed; it never alters, delays beyond the call, retries in a blocking way or fails the transition that triggered it.
- **FR-010**: A `projects_v2_item` edit that changes only one of these fields, and the corresponding `updatedAt` movement seen by reconcile, probe and the staleness baseline, is a no-op: no drift count, no deep fetch, no item invalidation, no poll wake. The behavior is the same as for the status-line field.
- **FR-011**: Values are left in place when an item reaches Done; nothing clears them.
- **FR-012**: The `Last run` line is truncated to about 60 characters with an ellipsis marker, counting characters rather than bytes.
- **FR-013**: No new label is introduced.
- **FR-014**: A wire-contract fixture in `github/testdata/` covers the date-field update mutation (and the lookup of a date field), validated by the existing wire-contract test layer.
- **FR-015**: `docs/USER_GUIDE.md` documents both fields, their configuration, the event each value comes from, that `Last activity` has day granularity, and the restart caveat (the writer's record resets on restart; values on the board are never read back and may be rewritten once with an identical value). `docs/state-machine.md` states the fields are display-only. `docs/llms-full.txt` is regenerated. An ADR numbered 2049 records the decisions.
- **FR-016**: Unit tests cover: a job completion writes `Last activity` and `Last run` from the event's existing fields; a job start writes `Last activity`; an unchanged value writes nothing; a missing or wrong-typed field is skipped silently; `Skipped` events, merge-train batch events and turn-progress events write nothing; Done leaves the values in place; a display-field-only edit causes no drift, deep fetch or wake for the new fields. The diff introduces no new store fields or persistence.

### Key Entities *(if applicable)*

- **Last activity**: A ProjectV2 date field holding the date Fabrik last started or finished a job on the item. Display-only.
- **Last run**: A ProjectV2 text field holding a one-line summary of the item's most recent finished job. Display-only.
- **Field writer**: The existing engine-wide display-field writer from #2048, extended to a second field type.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A maintainer can sort or group the board by `Last activity` and read `Last run` for any item that has had a job since the daemon started.
- **SC-002**: Across any number of events on one day for one item, `Last activity` is written at most once per distinct date, and `Last run` is written at most once per distinct line; none per turn or per poll.
- **SC-003**: Writes to either field produce zero drift counts, zero deep fetches, zero item invalidations and zero poll wakes.
- **SC-004**: On a board lacking a field, behavior is identical to today apart from one startup log line for that field.
- **SC-005**: Every `Last run` line is at most about 60 characters.
- **SC-006**: The diff adds no new `ItemState` or store fields and no persisted state, and the new tests pass in `go test -race ./...`.

## Assumptions

- The fields are created by the operator on the board (a date field and a text field); Fabrik does not create them.
- Defaults are on, as for `status_line`: with no `project_fields` entry the names `Last activity` and `Last run` are used, and a board without them simply skips.
- The date written is the UTC calendar date of the event time, so it does not depend on the host's time zone. A ProjectV2 date field has no time-of-day.
- A comment-processing run is worded like `Validate · comment review · <outcome>` in `Last run`.
- Exact optional segments follow the Input's examples: turns when a budget is known, duration when the run completed, neither otherwise. Plan may refine the exact segment rules while keeping those examples and the 60-character cap.
- Values written before a restart stay on the board; after a restart the first event for an item rewrites them from current events. This is documented, not fixed.
- Extra in-memory counters (review cycles, ejections) are not included in this issue. The Input allows them only if already held and trivial, and Plan may propose them as a follow-up.

## Out of Scope *(optional)*

- Cost in dollars or tokens, cumulative totals, per-turn values and any persistence or read-back of board fields.
- Any gating, filtering or decision logic that reads the fields.
- Review-cycle, ejection or other counters (follow-up).
- Clearing values at Done (they stay as history).
- Automatic creation of the fields on the board.
- Per-item writes for merge-train batch jobs.

## Source References *(optional)*

- Report: #2043 (public triage board). Mention only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2043 in the PR body. This issue's own number gets `Closes`.
- Builds on: #2048 / ADR 2048 (status-line writer and echo suppression), ADR 042 (mutation echo check), ADR 035 (status reconciliation).
