# Feature Specification: Board administration on GHES without an option ID — never clear item Statuses

**Feature Branch**: `fabrik/issue-2616`
**Created**: 2026-11-24
**Status**: Specified
**Input**: User description: "## Problem

`fabrik repair-board --apply` fails on GHES 3.19.10 when adding missing Status columns:

```
setting status field options for field …: GraphQL error: Variable $options of type [ProjectV2SingleSelectFieldOptionInput!]! was provided invalid value for 0.id (Field is not defined on ProjectV2SingleSelectFieldOptionInput), …
```

`SetStatusFieldOptions` (`github/project_admin.go`) calls `updateProjectV2Field` with `singleSelectOptions` and echoes each existing option's `id`, because on github.com an option sent without its `id` is regenerated and every item's Status is cleared (ADR 1714). On GHES 3.19.10 the input type has only `name`, `color` (non-null) and `description` (non-null): there is no way to keep an option's ID. `fabrik init --create-board` echoes the board's default options the same way and almost certainly fails too. The display-field creation and polling work.

## Requirements

- **R1.** Detect whether the server's `ProjectV2SingleSelectFieldOptionInput` accepts `id` (introspection, once per command; or a documented GHES-version rule — decide in Plan). Never send `id` where it is not accepted.
- **R2.** Where `id` is not accepted, a full option-list replacement regenerates every option. Perform it only when no item on the board has a Status value (an empty board, or `--create-board`), sending name, color and description (empty string when none).
- **R3.** Where `id` is not accepted and the board has items with a Status, refuse with a clear message naming the missing columns and the manual remedy (add them in the board UI), and change nothing. Never clear item Statuses.
- **R4.** github.com behaviour is unchanged (id echo kept).
- **R5.** Correct the User Guide's GHES section: the \"full API parity\" claim is wrong for this mutation; name the limitation and the remedy.

## Scope

In: `github/project_admin.go`, `cmd/repair_board.go`, `cmd/init.go` (`--create-board`), `docs/USER_GUIDE.md` GHES section (regenerate `docs/llms-full.txt`), the wire-contract fixtures, an ADR numbered after this issue (amends ADR 1714).
Out: other GHES schema differences.

## Acceptance

- Tests against recorded GHES-shaped schema/fixtures: empty board → name-only options sent, no `id`; board with items → refusal, no mutation sent; github.com shape → ids echoed as today.
- Wire-contract test pins both input shapes.

**Report linkage:** this restates community report #2610. Mention #2610 in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it, not even inside a negation (safe: \"mentioned for context: #2610\"). The report is closed by hand once a release containing the fix ships."

## Background

Fabrik's board-administration commands (`fabrik repair-board --apply` and `fabrik init --create-board`) add Status columns by replacing a board's whole single-select option list in one call. On github.com, an option sent without its existing identifier is regenerated as a new option, which silently clears the Status of every item on the board. ADR 1714 therefore has these commands send every existing option back together with its identifier, so existing items keep their Status.

On GitHub Enterprise Server 3.19.10 the option input type does not accept an identifier at all (it takes only name, color and description, the last two required). The identifier echo is rejected outright, so the commands fail with a schema error and the operator cannot add missing columns through Fabrik. On such a server there is also no way to keep an existing option's identity through a replacement, so a naive "just drop the identifier" fix would be silently destructive on any board that already has items with a Status.

The User Guide's GHES section currently claims "full API parity for everything Fabrik uses"; that is wrong for this mutation and needs correcting. The display-only field creation and polling already work on GHES; only the Status option replacement is affected. This work restates community report #2610; that report stays human-owned and is closed by hand after a release ships the fix.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Create a board on GHES (Priority: P1)

An operator on GHES runs `fabrik init --create-board` to create a fully configured board. The new board has no items, so its default Status options can safely be replaced with the pipeline's columns.

**Why this priority**: Without it, a GHES operator cannot bootstrap Fabrik with the supported board-creation path at all.

**Independent Test**: Run board creation against a GHES-shaped server whose option input has no identifier; confirm the options mutation is sent with name, color and description only and succeeds.

**Acceptance Scenarios**:

1. **Given** a GHES-shaped server whose option input has no identifier field, **When** `--create-board` replaces the new board's default Status options, **Then** every option sent carries name, color and description (empty string when there is none) and no identifier.
2. **Given** the same server, **When** board creation completes, **Then** the board has all of the pipeline's Status columns and the command's normal closing output is unchanged.

---

### User Story 2 - Repair an empty or Status-less board on GHES (Priority: P1)

An operator on GHES runs `fabrik repair-board --apply` on a board where no item holds a Status value (for example a freshly created or fully cleared board) and columns are missing. The replacement cannot damage anything, so it proceeds.

**Why this priority**: It is the exact failing case from the report, in its safe form.

**Independent Test**: Against a GHES-shaped server and a board with no Status-bearing items, run repair with apply and confirm the missing columns are added with identifier-free options.

**Acceptance Scenarios**:

1. **Given** a GHES-shaped server and a board where no item has a Status value, **When** `repair-board --apply` runs with columns missing, **Then** the option list is replaced with identifier-free options covering existing plus missing columns, and the command succeeds.
2. **Given** the same board, **When** the existing options carry a color and description, **Then** those values are preserved in the replacement.

---

### User Story 3 - Refuse safely on a populated GHES board (Priority: P1)

An operator on GHES runs `fabrik repair-board --apply` on a board with items that have a Status and some columns missing. Fabrik cannot add columns without wiping Statuses, so it declines and tells the operator how to proceed manually.

**Why this priority**: Preventing silent data loss (every item's Status cleared) is the core safety property of ADR 1714 and must hold on GHES too.

**Independent Test**: Against a GHES-shaped server and a board with at least one Status-bearing item, run repair with apply; confirm no options mutation is sent, the output names the missing columns and the manual remedy, and the exit is non-zero.

**Acceptance Scenarios**:

1. **Given** a GHES-shaped server and a board where at least one item has a Status, **When** `repair-board --apply` finds missing columns, **Then** no option-replacing mutation is sent and the message names the missing columns and says to add them in the board's UI.
2. **Given** the same board, **When** nothing is missing, **Then** the command reports there is nothing to do and is not affected by the limitation.
3. **Given** the same board, **When** the Status state of the board cannot be determined, **Then** Fabrik refuses rather than assuming the board is empty.

---

### User Story 4 - github.com is unchanged (Priority: P1)

An operator on github.com sees no behavior change: existing options are echoed with their identifiers so item Statuses survive.

**Why this priority**: The fix must not regress the primary, working path.

**Independent Test**: Against a github.com-shaped schema whose option input accepts an identifier, confirm identifiers are echoed exactly as today, including on boards with items.

**Acceptance Scenarios**:

1. **Given** a server whose option input accepts an identifier, **When** repair or board creation replaces options, **Then** each existing option is sent with its identifier, color and description, and new columns are appended without one.

---

### User Story 5 - Accurate GHES documentation (Priority: P2)

A GHES operator reads the User Guide and learns the real limitation and the remedy before hitting it.

**Why this priority**: The current text is misleading; correcting it is required but does not change behavior.

**Independent Test**: Read the GHES section and confirm it no longer claims full parity for this mutation, names the limitation, and states the manual remedy.

**Acceptance Scenarios**:

1. **Given** the User Guide, **When** the GHES section is read, **Then** it states that adding Status columns through Fabrik works on GHES only when no board item has a Status, and otherwise the columns must be added in the board UI.

---

### Edge Cases

- A board whose items exist but none has a Status value counts as having no Status-bearing items, so the replacement is allowed on GHES.
- A board with a mix of Status-bearing and Status-less items counts as populated, so it is refused on GHES.
- Items that are archived or live on a page beyond the first of the board listing must still be considered; an incomplete read of the board must not be treated as "empty".
- Existing options with no description are sent with an empty description where the server requires a non-null one.
- A server whose capability cannot be determined (introspection fails or is denied) must not fall through to sending identifiers or to destructive replacement without a documented, safe default.
- The dry-run form of `repair-board` (no `--apply`) reports the limitation in its diagnosis but never mutates.
- Display-only field creation in `repair-board` is a separate mutation and keeps working on GHES.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Fabrik determines, once per command invocation, whether the connected server's single-select option input accepts an identifier, and never sends an identifier where it is not accepted.
- **FR-002**: Where the server accepts an identifier, Fabrik behaves exactly as today: every existing option is echoed with its identifier, color and description, and only new columns are sent without one (github.com behavior unchanged).
- **FR-003**: Where the server does not accept an identifier and no item on the board has a Status value, Fabrik may replace the option list, sending for each option a name, a color and a description (empty string when none), preserving existing options' names, colors and descriptions.
- **FR-004**: Where the server does not accept an identifier and at least one item on the board has a Status value, Fabrik refuses to modify the Status options, sends no option-replacing mutation, names the missing columns, and states the manual remedy (add them in the board's UI).
- **FR-005**: The refusal in FR-004 is a failing outcome (non-zero exit) so scripts and operators cannot mistake it for success; it never clears or alters any item Status.
- **FR-006**: The determination of whether any item has a Status value reads the whole board (all pages); if it cannot be established, Fabrik treats the board as populated and refuses (fail closed).
- **FR-007**: `fabrik init --create-board` applies the same capability handling, so that on a server without identifier support the new board's default options are replaced with identifier-free options and board creation succeeds.
- **FR-008**: `fabrik repair-board` without `--apply` reports the same capability-dependent outcome (what would be added, or that GHES cannot add it safely) without sending any mutation.
- **FR-009**: Display-only field creation and every other board-administration behavior not involving Status option replacement is unchanged.
- **FR-010**: The User Guide's GHES section no longer claims full API parity for this mutation; it names the limitation, when it applies, and the remedy; `docs/llms-full.txt` is regenerated in the same change.
- **FR-011**: Tests, using recorded GHES-shaped and github.com-shaped schema fixtures, cover: empty board on a no-identifier server (name-only options, no identifier sent); populated board on a no-identifier server (refusal, no option mutation sent); github.com shape (identifiers echoed as today). A wire-contract test pins both option input shapes.
- **FR-012**: An ADR numbered after this issue records the decision and amends ADR 1714.

### Key Entities *(if applicable)*

- **Single-select option**: A Status column; has a name, color, description and, on servers that support it, a stable identifier that items' Status values reference.
- **Server capability**: Whether the option input type accepts an identifier; determines which path is safe.
- **Board Status population**: Whether any board item currently holds a Status value; determines whether an identifier-free replacement is safe.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a GHES-shaped server without identifier support, `fabrik init --create-board` completes and the new board contains every pipeline Status column.
- **SC-002**: On the same server, `repair-board --apply` on a board with no Status-bearing items adds all missing columns in one run with no schema error.
- **SC-003**: On the same server, `repair-board --apply` on a board with any Status-bearing item and missing columns sends zero option-replacing mutations, leaves every item's Status unchanged, and exits non-zero with a message naming the missing columns and the manual remedy.
- **SC-004**: On github.com-shaped input, the requests sent are identical to those sent before this change.
- **SC-005**: The User Guide GHES section contains no unqualified parity claim for board-administration and states the limitation and remedy.

## Assumptions

- How capability is detected (schema introspection once per command versus a documented GHES-version rule) is a design choice deferred to Plan; the requirement is only that identifiers are never sent where unsupported and that an undeterminable capability resolves to the safe (non-destructive) behavior.
- Only the option-input difference is in scope; GHES 3.19.10 is the reference server, and later GHES versions that accept an identifier take the github.com path automatically through capability detection.
- A refusal exits non-zero, because the requested change was not made.
- The Status-refusal does not block the independent display-field creation in `repair-board`; those are reported and applied on their own as today.
- "Status value" means the Status single-select field being set on an item; items with no value do not make a board "populated".
- Missing columns are reported by name in stage order, as the existing diagnosis already does.

## Out of Scope *(optional)*

- Other GHES schema or API differences.
- Preserving item Statuses across a replacement on a server that gives options no stable identity (e.g. by re-assigning each item's Status afterward).
- Automating the manual UI remedy.
- Changes to the closing-reference guard or report handling (report #2610 is closed by hand after a release).

## Source References *(optional)*

- Community report #2610 (mentioned for context).
- ADR 1714 (board-admin create and repair), which this work amends.
- User Guide, "GitHub Enterprise Server Support" section.
