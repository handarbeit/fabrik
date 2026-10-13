# ADR 2110: Board Administration Creates the Display-Only Project Fields

**Date**: 2026-10-11
**Status**: Accepted
**Issue**: #2110 — init --create-board and repair-board create the display-only board fields (Fabrik, Last activity, Last run)
**Builds on**: [ADR 1714](1714-board-admin-create-and-repair.md), [ADR 2048](2048-display-only-status-line-field.md), [ADR 2049](2049-display-only-last-activity-and-last-run.md), [ADR 1453](1453-wire-contract-graphql-schema-validation.md)
**Amends**: ADR 2048 and ADR 2049, which say "Fabrik never creates the field(s)". Those records are left unedited.

## Context

The status line (`Fabrik`, TEXT), `Last activity` (DATE) and `Last run` (TEXT) only work if the
operator first adds the field in Project settings. A board made by `fabrik init --create-board`
therefore started without them, as did every board that predates them, and each feature silently
did nothing beyond one `[startup] … field unavailable` line.

## Decision

1. **The engine still never creates fields at runtime.** Its startup lookup and the unavailable line
   are unchanged. The two board-administration commands create them: `init --create-board` (new
   board) and `repair-board --apply` (existing board). `repair-board` without `--apply` only
   reports, keeping ADR 1714's AC4.
2. **One mutation primitive, create-only.** `Client.CreateProjectV2Field(projectID, name, dataType)`
   (TEXT or DATE; anything else is rejected before a request) issues `createProjectV2Field` and
   returns the new ID; an empty ID is an error. Field creation never goes through
   `updateProjectV2Field`, so ADR 1714's R4 option-ID trap cannot arise: no existing field is
   replaced, renamed, retyped or deleted by any code path.
3. **List and match in Go, not `field(name:)`.** The engine's lookup collapses "absent" and "wrong
   type" into one empty result, and changing it would put R7 (engine unchanged) at risk. The admin
   commands use a separate `FetchProjectFields` (paginated `fields(first: 100)`, real `dataType`
   for every field type). Names are matched case-insensitively, since GitHub's field-name
   uniqueness is not case-sensitive. A same-named field of another type, including a built-in such as
   `Labels`, is a **type mismatch**: reported (`expected TEXT, found SINGLE_SELECT`), never written.
4. **One shared definition.** `requiredDisplayFields` (`cmd/board_admin.go`) takes the resolved
   `config.ProjectFieldsConfig` and calls the same `Resolve*Field()` methods the engine uses, so
   names and `off` handling cannot drift. `planDisplayFields` / `applyDisplayFields` are shared by
   both commands. Two display fields configured with the same name: the second is reported as a
   name collision and never created.
5. **Exit status.** Only a failed creation (or a failed field listing) makes a command exit
   non-zero, and the failed field is named. A type mismatch or collision needs an operator decision
   and is shown in the output with exit 0. A failure on one field never stops the others or the
   Status repair; errors are joined.
6. **`init --create-board` keeps the board on a field failure.** `createBoardCore` returns the
   field error separately from the board result, so `runInit` still writes `.fabrik/config.yaml`
   and the git exclude, then exits non-zero naming the field and pointing at
   `fabrik repair-board --apply`. Field creation runs after the Status columns, when the board
   already exists.
7. **Same refusals as Status repair.** `refuseIfUserOwnedBoard` precedes every field mutation in
   both commands; PAT, App (`appBoardClient`) and GHES client construction are unchanged.
8. **Wire coverage.** Both new GraphQL documents are in the wire-contract registry and validated
   against the vendored schema (the payload ID is selected through `... on ProjectV2FieldCommon`).
   Per ADR 2049's precedent the payload tests are hand-authored `httptest` literals, not recordings.

## Consequences

- A fresh `--create-board` board has all enabled display fields with no manual step; existing boards
  are fixed with one `repair-board --apply`, and a second run is a no-op.
- `repairBoardCore` no longer returns early when Status is complete: it reports Status and fields
  together and repairs them independently. The all-present message still says `nothing to do`.
- The permission needed by `createProjectV2Field` is assumed to be no more than board creation and
  Status repair already need; if an installation lacks it, the per-field failure path reports it.
