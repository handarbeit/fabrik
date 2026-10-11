# Feature Specification: init --create-board and repair-board create the display-only board fields (Fabrik, Last activity, Last run)

**Feature Branch**: `fabrik/issue-2110`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

Three display-only ProjectV2 fields need the operator to create them by hand. Fabrik never creates them:

- `Fabrik`: TEXT, the status line (#2048, `project_fields.status_line`)
- `Last activity`: DATE (#2049, `project_fields.last_activity`)
- `Last run`: TEXT (#2049, `project_fields.last_run`)

`docs/USER_GUIDE.md` says \"Fabrik never creates the field(s)\". As a result, a board created by `fabrik init --create-board` has none of these fields. A board that existed before these features also lacks them. Each feature then silently does nothing, apart from one `[startup] … field unavailable` line, until someone adds the field in Project settings. Nothing in the codebase issues `createProjectV2Field` today.

## Requirements

**R1 — create on new boards.** `fabrik init --create-board` creates each enabled display field with its configured name and its type: `Fabrik` TEXT, `Last activity` DATE, `Last run` TEXT. A field whose `project_fields.*` value is `off` is not created. Names come from the same config resolution the engine uses, so a custom name creates that name.

**R2 — repair existing boards.** `fabrik repair-board` reports each enabled display field that is missing, alongside the missing Status columns it already reports. Only with `--apply` does it create them. The dry-run stays the default, and nothing mutates without `apply == true` (ADR-1714's AC4).

**R3 — never replace, never retype.** If a field already exists with the configured name but the wrong data type (for example a single-select named `Fabrik`), report it as a type mismatch and leave it untouched. Never delete, rename or recreate a field. An existing field of the right type is reported as present, with no write.

**R4 — same refusals as Status repair.** Use the existing gates unchanged:
- `refuseIfUserOwnedBoard`, based on the live-resolved owner type. A user-owned board is refused, as for Status.
- PAT and App auth paths (`appBoardClient`).
- GHES handling.

**R5 — one mutation primitive.** Add one `github` method (for example `CreateProjectV2Field(projectID, name string, dataType)`) for TEXT and DATE. It sits beside `SetStatusFieldOptions` in `github/project_admin.go` and returns the new field ID. Field creation must go through `createProjectV2Field` and never through `updateProjectV2Field`, so the Status option-ID trap (ADR-1714 R4) cannot arise here.

**R6 — partial failure is reported, not fatal to the rest.** If one field fails to create, that does not stop the others or the Status repair. The command exits non-zero and names the field that failed.

**R7 — engine unchanged.** The engine still never creates fields at runtime. Its startup lookup and \"field unavailable\" line stay exactly as they are.

## Scope

- In scope: `cmd/init.go` (`createBoardCore`), `cmd/repair_board.go` (`repairBoardCore`), `cmd/board_admin.go` (a shared \"required display fields\" helper, so the two commands cannot diverge), `github/project_admin.go`, a wire-contract fixture/schema check for the new mutation, unit tests for both cores, and docs.
- Docs: update `docs/USER_GUIDE.md` §§ status line / Last activity and Last run, and the `fabrik init` / `repair-board` sections. The \"Fabrik never creates the field\" wording changes to say the engine never creates them at runtime, and that `init --create-board` and `repair-board --apply` do. Regenerate `docs/llms-full.txt` in the same commit. Add an ADR `adrs/<issue>-*.md`.
- Out of scope: any engine runtime change; any field other than these three; migrating or renaming existing fields.

## Acceptance

1. `--create-board` on an org board yields all three fields with the correct types, plus the existing Status setup.
2. `--create-board` with `project_fields.last_run: off` creates `Fabrik` and `Last activity` only.
3. `repair-board` dry-run on a board missing `Last run` lists it and makes no mutation.
4. `repair-board --apply` creates `Last run`. Running it again is a no-op that reports all fields as present.
5. `repair-board` against a board where `Fabrik` exists as a single-select reports a type mismatch and makes no write to that field.
6. A user-owned board is refused before any field mutation.
7. Tests prove they are not vacuous: neutralising the creation call fails tests 1 and 4."

## Background

Fabrik has three display-only ProjectV2 fields: the status line (`Fabrik`, TEXT, #2048), `Last activity` (DATE, #2049) and `Last run` (TEXT, #2049). Their names are configurable through `project_fields.status_line`, `project_fields.last_activity` and `project_fields.last_run`, and each can be disabled with `off`. The engine only writes to these fields. It never creates them, and nothing in the codebase issues the `createProjectV2Field` mutation today.

The operator has to add the fields in Project settings by hand. A board created by `fabrik init --create-board` therefore starts without them, and so does any board that predates these features. Each affected feature then does nothing except print one `[startup] … field unavailable` line. The fix belongs in the board-administration commands (`init --create-board`, `repair-board`), which already create and repair the Status columns under ADR-1714's rules: dry-run by default, never mutate without `--apply`, refuse user-owned boards. The engine's runtime behaviour must not change.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - New boards come with the display fields (Priority: P1)

An operator runs `fabrik init --create-board --owner <org>` and gets a board that already has every enabled display field, with the correct type and the configured name, in addition to the Status setup.

**Why this priority**: This is the entry point for every new user. Without it, status line, Last activity and Last run silently do nothing on a freshly provisioned board.

**Independent Test**: Run `createBoardCore` against a fake GitHub client for an org-owned board. Assert that `Fabrik` (TEXT), `Last activity` (DATE) and `Last run` (TEXT) were each created once with the right type, and that the Status options were still set.

**Acceptance Scenarios**:

1. **Given** an org-owned owner and default config, **When** `init --create-board` runs, **Then** the board is created with the Status columns and three new fields: `Fabrik` (TEXT), `Last activity` (DATE) and `Last run` (TEXT).
2. **Given** `project_fields.last_run: off`, **When** `init --create-board` runs, **Then** only `Fabrik` and `Last activity` are created.
3. **Given** `project_fields.status_line: "Status line"`, **When** `init --create-board` runs, **Then** a TEXT field named `Status line` is created and no field named `Fabrik` is.
4. **Given** a user-owned target, **When** `init --create-board` runs, **Then** it is refused before any board or field mutation, as today.

---

### User Story 2 - Existing boards are repaired (Priority: P1)

An operator whose board predates these features runs `fabrik repair-board` to see which display fields are missing, then `fabrik repair-board --apply` to create them.

**Why this priority**: Every existing deployment is in this state. Without it they have to create the fields by hand.

**Independent Test**: Run `repairBoardCore` against a fake client whose board lacks `Last run`. In dry-run the report lists it and no create call is made. With apply, the TEXT field `Last run` is created. A second apply makes no create call and reports all fields present.

**Acceptance Scenarios**:

1. **Given** a board missing `Last run`, **When** `repair-board` runs without `--apply`, **Then** the report lists `Last run` as missing and no mutation is made.
2. **Given** the same board, **When** `repair-board --apply` runs, **Then** `Last run` is created as a TEXT field and the report says so.
3. **Given** the board after step 2, **When** `repair-board --apply` runs again, **Then** no mutation is made and all enabled fields are reported present.
4. **Given** a board that is missing both Status columns and display fields, **When** `repair-board --apply` runs, **Then** both are repaired in the one invocation.
5. **Given** a field disabled with `off`, **When** `repair-board` runs, **Then** that field is neither reported missing nor created.

---

### User Story 3 - Existing fields are never touched (Priority: P1)

When a field with the configured name already exists, the commands never replace, retype, rename or delete it.

**Why this priority**: Destroying a field destroys the data in it across every item. This is the safety property that makes the feature acceptable to run against a live board.

**Independent Test**: Run `repairBoardCore` with apply against a board where `Fabrik` is a single-select field. Assert a type mismatch is reported and no create, update or delete call touches that field.

**Acceptance Scenarios**:

1. **Given** a field `Fabrik` that is a single-select, **When** `repair-board --apply` runs, **Then** the report names a type mismatch (expected TEXT, found single-select) and no write is made to that field.
2. **Given** a field `Last activity` that is a DATE, **When** `repair-board --apply` runs, **Then** it is reported present and no write is made.
3. **Given** a type mismatch on one field, **When** `repair-board --apply` runs, **Then** the other fields and the Status columns are still repaired.

---

### User Story 4 - Failures are visible and isolated (Priority: P2)

If creating one field fails, the remaining fields and the Status repair still run, and the operator is told which field failed.

**Why this priority**: A transient or permission failure on one field should not leave the board half-repaired with no explanation, and should not block unrelated repairs.

**Independent Test**: With a fake client that fails the create call for `Last activity` only, run `repairBoardCore` with apply. Assert the other fields and the Status repair completed, the error names `Last activity`, and the command result is non-zero.

**Acceptance Scenarios**:

1. **Given** the create call fails for `Last activity`, **When** `repair-board --apply` runs, **Then** `Fabrik` and `Last run` are still created, the Status repair still runs, and the command exits non-zero naming `Last activity`.
2. **Given** the same failure during `init --create-board`, **When** the command runs, **Then** the remaining fields are still attempted and the command exits non-zero naming the failed field.

---

### Edge Cases

- A display field set to `off` is skipped entirely: not created, not reported missing, not reported as a mismatch.
- Two display fields configured with the same name: only one field can exist with that name. The collision is reported and no create is attempted for the second.
- A field name matching an existing field case-insensitively: this is treated as existing, so no create is attempted. Field-name matching follows the same rule the engine's lookup uses.
- A board with no display fields and no Status gaps: dry-run lists only the missing fields; apply creates them.
- Create succeeds on GitHub but the returned ID is empty: this is reported as a failure for that field.
- A field created concurrently by someone else between the read and the create: the failure is reported for that field like any other create failure. Re-running reports it present.
- Unlinked (repo-less) boards created by `init --create-board` get the same fields.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `fabrik init --create-board` MUST create each enabled display field on the new board with its configured name and type: the status line as TEXT, `Last activity` as DATE, `Last run` as TEXT.
- **FR-002**: A display field whose `project_fields.*` value is `off` MUST NOT be created by either command, nor reported missing.
- **FR-003**: The field names used by both commands MUST come from the same config resolution the engine uses, so a custom configured name creates a field of that name.
- **FR-004**: `fabrik repair-board` MUST report, alongside the missing Status columns, each enabled display field that is missing from the board.
- **FR-005**: `fabrik repair-board` MUST create missing display fields only when `--apply` is given. Without it, no mutation of any kind is made, and dry-run remains the default.
- **FR-006**: If a field exists with the configured name and the wrong data type, both commands MUST report a type mismatch naming the field, the expected type and the found type, and MUST NOT write to that field.
- **FR-007**: Neither command MUST ever delete, rename, retype or recreate an existing field.
- **FR-008**: An existing field of the correct type MUST be reported as present with no write.
- **FR-009**: Both commands MUST apply the existing user-owned-board refusal, based on the live-resolved owner type, before any display-field mutation.
- **FR-010**: Both commands MUST work under both PAT and GitHub App auth and under GHES, through the same client construction the Status repair already uses.
- **FR-011**: The `github` package MUST expose one field-creation method covering TEXT and DATE, located beside `SetStatusFieldOptions`, that returns the new field's ID. It MUST issue the `createProjectV2Field` mutation and MUST NOT use `updateProjectV2Field`.
- **FR-012**: The list of display fields a board requires (name, type, enabled or not) MUST be defined once in a shared helper used by both commands, so they cannot diverge.
- **FR-013**: A failure to create one field MUST NOT prevent the other fields, or the Status repair, from being attempted.
- **FR-014**: When any field creation fails, the command MUST exit non-zero and the output MUST name each field that failed.
- **FR-015**: The engine's runtime behaviour MUST be unchanged: it never creates fields, and its startup lookup and `[startup] … field unavailable` line stay as they are.
- **FR-016**: A wire-contract fixture and schema check MUST cover the new mutation, in the same way the existing board-administration mutations are covered.
- **FR-017**: Unit tests for both command cores MUST fail if the field-creation call is neutralised: the new-board creation path and the repair apply path each need a test that asserts the create call was made with the right name and type.
- **FR-018**: `docs/USER_GUIDE.md` MUST be updated in the status line and Last activity / Last run sections and in the `fabrik init` and `repair-board` sections. The wording "Fabrik never creates the field" becomes: the engine never creates them at runtime, and `init --create-board` and `repair-board --apply` do. `docs/llms-full.txt` MUST be regenerated in the same commit.
- **FR-019**: An ADR numbered `adrs/2110-*.md` MUST record the decision.

### Key Entities *(if applicable)*

- **Display field**: A ProjectV2 field Fabrik writes for human display only. It has a configured name, a required data type (TEXT or DATE) and an enabled/disabled state (`off`). It is never read back as state.
- **Field report entry**: The per-field outcome shown to the operator: present, missing, created, type mismatch, disabled, or failed.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After `fabrik init --create-board` on an org-owned owner with default config, the board has exactly the three display fields, each with the correct type, with no manual step.
- **SC-002**: With one field set to `off`, `init --create-board` creates exactly the other two display fields.
- **SC-003**: `repair-board` without `--apply` makes zero mutating calls on any board.
- **SC-004**: `repair-board --apply` on a board missing a field creates it. A second consecutive run makes zero create calls and reports every enabled field present.
- **SC-005**: Across all tested scenarios, no existing field is ever the target of a write from either command, including when it has the wrong type.
- **SC-006**: A user-owned board is refused with zero field mutations.
- **SC-007**: With a field creation failure injected, the remaining repairs complete and the command's result is non-zero and names the failed field.
- **SC-008**: Removing the field-creation call from either command makes at least one test fail in each core.

## Assumptions

- The configured field names are resolved from `project_fields.*` with the same defaults as the engine (`Fabrik`, `Last activity`, `Last run`). The shared helper takes the already-resolved values rather than re-implementing the defaults.
- Field-name matching for "already exists" follows the rule the engine's own field lookup uses, so a field the engine would find is never created again.
- The dry-run report only adds lines for display fields. The existing Status report format is otherwise unchanged.
- A type mismatch is a reportable condition and makes no change to the exit status by itself, since it needs an operator decision. Only a failed creation makes the command exit non-zero. The mismatch is shown plainly in the output.
- The new mutation needs no permission beyond what board creation and Status repair already need. If the installation lacks it, the per-field failure path in FR-013 and FR-014 applies.
- In `init --create-board`, a field creation failure does not roll back the already-created board. The board exists, the failed field is named, and `repair-board --apply` can finish the job.

## Out of Scope *(optional)*

- Any change to the engine's runtime behaviour, including creating fields at runtime.
- Any field other than the three display fields.
- Migrating, renaming, retyping or deleting existing fields, including resolving a type mismatch automatically.
- Adding display fields to user-owned boards (still refused, as for Status).
- Creating fields from `fabrik init` against an existing board URL (only `--create-board` and `repair-board` are in scope).

## Source References *(optional)*

- ADR-1714 (`adrs/1714-board-admin-create-and-repair.md`): board creation and repair, dry-run default, AC4, the `updateProjectV2Field` option-ID trap (R4), user-owned refusal (R7).
- ADR 2048 (`adrs/2048-display-only-status-line-field.md`) and ADR 2049 (`adrs/2049-display-only-last-activity-and-last-run.md`): the display-only fields this issue provisions.
- `docs/USER_GUIDE.md` status line and Last activity / Last run sections; `docs/state-machine.md` §7.17 and §7.17.1.
