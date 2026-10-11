# Feature Specification: `fabrik_promote` honours the legacy Backlog compat net

**Feature Branch**: `fabrik/issue-2113`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

`fabrik_promote` refuses the move it exists for on any install that predates
`stages/examples/backlog.yaml`. That includes this repo's own dev instance.

Observed against the live dev daemon (`.fabrik/stages/` has no `backlog.yaml`; Backlog is a board
column) on #2112, sitting in Backlog:

```
fabrik_promote {\"issue\":\"handarbeit/fabrik#2112\",\"to\":\"Specify\"}
→ refused: handarbeit/fabrik#2112 is in column \"Backlog\", which has no configured stage
  current state: { \"status\": \"Backlog\", \"valid_targets\": [\"Specify\", …], \"autonomy\": \"none\" }
```

The cause:
- `overseerActor.parkingRefusal` (`engine/overseer_actions.go`) recognises a parking column only
  through `stageByName(status).Unmanaged`.
- The rest of the engine treats `Backlog` as a parking column even without a configured stage,
  through the documented compat net for installs that ran `fabrik init` before `backlog.yaml`
  existed:
  - `engine/startup.go:147` (`recognized[\"Backlog\"] = true`);
  - `engine/spawn.go` (the literal `\"Backlog\"` check beside the unmanaged check);
  - `docs/USER_GUIDE.md` §refresh-stages, which says such installs \"keep working unchanged\".
- Promote is the one place that does not honour that net. `refresh-stages` never creates the
  file, so these installs stay in this state until someone re-runs `fabrik init`.

The refusal is also self-contradictory: it lists `valid_targets` for a move it will not make.

## Requirements

- **R1 — One parking-column predicate.** Define a single \"is this column an unmanaged parking
  column\" predicate and use it in `parkingRefusal` and in the existing compat-net sites. The
  predicate is: a configured stage with `unmanaged: true`, **or** the literal `\"Backlog\"` when no
  stage named `Backlog` is configured. A configured `Backlog` stage that is *not* unmanaged keeps
  its configured meaning.
- **R2 — Promote works on legacy installs.** With no `backlog.yaml`, promoting an item from the
  `Backlog` column to a pipeline column succeeds through the same write path and audit comment
  as a declared-unmanaged Backlog.
- **R3 — Refusals do not offer impossible targets.** When a promote is refused because the source
  column is not a parking column, the refusal does not present `valid_targets` as if the move
  were possible. Either omit them or label them as the targets available *from a parking
  column*.
- **R4 — Docs.**
  - Update the `fabrik_promote` row in `docs/USER_GUIDE.md` to say that legacy Backlog counts.
  - Update `docs/state-machine.md` §7.15 if it describes the guard.
  - Regenerate `docs/llms-full.txt`.

## Scope

In scope:
- the parking-column predicate and its call sites;
- the promote refusal shape.

Out of scope:
- making `refresh-stages` create missing stage files;
- new tools.

## Acceptance

- **Legacy install:**
  - An engine test has stage configs *without* a Backlog stage and an item in the `Backlog`
    column.
  - `Promote` to `Specify` succeeds: the Status is written and an audit comment posted.
  - It is shown non-vacuous: it fails with the old `parkingRefusal`.
- **Declared Backlog:** with `unmanaged: true` Backlog, promote still succeeds (unchanged).
- **Configured, managed `Backlog`:** a configured `Backlog` stage that is *not* unmanaged is
  refused, as a pipeline column.
- **Unknown column:** a column with no stage that is not `Backlog` is still refused.
- `go test -race ./...` is clean.

Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the
PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

Fabrik recognises "Backlog" as a parking column in two ways. A stage file such as `backlog.yaml` can declare it `unmanaged: true`. Alternatively, a documented compatibility net treats the literal board column `Backlog` as a parking column when no stage by that name is configured. The net exists so installs that ran `fabrik init` before `backlog.yaml` existed "keep working unchanged". `refresh-stages` never creates the missing file, so those installs, including this repo's own dev instance, stay in that state indefinitely.

The compat net is honoured in some places and not others:
- Honoured: startup board validation and the spawn placement fallback.
- Not honoured: the overseer `fabrik_promote` action, which recognises a parking column only through a configured unmanaged stage.

On a legacy install, promoting an item out of `Backlog` is refused with "which has no configured stage". That is the one move the tool exists for. The refusal also returns a `valid_targets` list for a move it will not make, which contradicts itself.

The fix is one shared notion of "parking column" used by promote and the existing compat-net sites, plus a refusal shape that stops advertising impossible targets.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Promote out of Backlog on a legacy install (Priority: P1)

An operator on an install with no `backlog.yaml` asks the overseer to promote an item sitting in the `Backlog` board column into a pipeline column (for example `Specify`).

**Why this priority**: This is the reported defect. The tool refuses its core job on every install that predates `backlog.yaml`.

**Independent Test**: Configure stages without any `Backlog` stage, place an item in the `Backlog` column, and call promote to `Specify`. Check that the board Status is written and one audit comment is posted.

**Acceptance Scenarios**:

1. **Given** no stage named `Backlog` is configured and an item is in the `Backlog` column, **When** the item is promoted to `Specify`, **Then** the board Status is moved to `Specify` and the standard `🏭 **Fabrik — overseer action: promote**` audit comment is posted, exactly as for a declared-unmanaged Backlog.
2. **Given** the same setup but the cached column is stale and the live column is no longer `Backlog`, **When** promote is called, **Then** it is refused based on the live column.

---

### User Story 2 - Configured Backlog stages keep their configured meaning (Priority: P1)

An operator whose `Backlog` stage is declared `unmanaged: true` sees no change. An operator whose `Backlog` stage is configured but not unmanaged has a pipeline column named Backlog, and promote treats it as one.

**Why this priority**: The compat net must not override explicit configuration, or it would silently change behaviour for installs that deliberately configured Backlog.

**Independent Test**: Run promote with (a) a Backlog stage with `unmanaged: true` and (b) a Backlog stage without it.

**Acceptance Scenarios**:

1. **Given** a `Backlog` stage with `unmanaged: true`, **When** an item in Backlog is promoted to a pipeline column, **Then** the promote succeeds as it does today.
2. **Given** a `Backlog` stage that is configured and not unmanaged, **When** an item in Backlog is promoted, **Then** promote refuses it as being in a pipeline column.

---

### User Story 3 - Refusals are honest about targets (Priority: P2)

An operator or agent whose promote is refused because the item is not in a parking column does not see a list of valid targets implying the move is possible.

**Why this priority**: The contradictory refusal misleads callers, but it does no harm beyond that.

**Independent Test**: Promote an item that is in a pipeline column, and one that is in an unknown column, and inspect the refusal payload and message.

**Acceptance Scenarios**:

1. **Given** an item in a pipeline column, **When** promote is refused, **Then** the refusal does not present `valid_targets` as moves available to this item.
2. **Given** an item in a column with no configured stage that is not `Backlog`, **When** promote is refused, **Then** the same holds.
3. **Given** an item that is in a parking column and a `to` that is not a valid target, **When** promote is refused, **Then** the valid targets are still listed, since the move is possible with a different `to`.

---

### Edge Cases

- A column named `Backlog` in a different case (for example `backlog`): the compat net matches the literal `"Backlog"` only, as at its existing sites, and this change does not widen it.
- An item with no known board column: still refused, as it is not established as parked.
- Board metadata not yet loaded: still refused with the existing "try again after the next poll" reason.
- A legacy install where `Backlog` is not a board option at all: no item can be in it, so there is nothing to promote.
- The cached column says `Backlog` but the live column differs, or the reverse: the live column decides, using the same predicate.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: There is a single predicate answering "is this board column an unmanaged parking column". It is true when a stage with that name is configured and marked `unmanaged: true`, or when the name is exactly `Backlog` and no stage named `Backlog` is configured. It is false for a configured `Backlog` stage that is not unmanaged.
- **FR-002**: The promote action uses that predicate to decide whether the item's source column is a parking column, for both the cached column and the live-confirmed column.
- **FR-003**: The existing sites that hand-code the legacy `Backlog` compat net (startup column recognition and spawn placement fallback) use the same predicate, so the rule is defined in one place. Their observable behaviour is unchanged.
- **FR-004**: Promoting an item out of the `Backlog` column on an install with no `Backlog` stage succeeds through the same Status write and the same audit comment as a declared-unmanaged Backlog.
- **FR-005**: Promoting an item out of a configured, non-unmanaged `Backlog` stage is refused as a pipeline column.
- **FR-006**: Promoting an item whose column has no configured stage and is not `Backlog` is refused.
- **FR-007**: A promote refused because the item's source column is not an established parking column does not present `valid_targets` as moves available to that item. The list is omitted, or clearly labelled as the targets available from a parking column.
- **FR-008**: A promote refused only because `to` is missing or is not a valid target, while the item is in a parking column, still lists the valid targets.
- **FR-009**: `docs/USER_GUIDE.md`'s `fabrik_promote` documentation states that a legacy `Backlog` column, with no configured stage, counts as a parking column.
- **FR-010**: `docs/state-machine.md` §7.15 is updated wherever it describes the parking-column guard, and `docs/llms-full.txt` is regenerated in the same change.
- **FR-011**: An engine test covers a legacy install (stage configs without a Backlog stage, an item in `Backlog`) with a successful promote to `Specify` that writes the Status and posts an audit comment. It must fail when run against the previous `parkingRefusal` behaviour.
- **FR-012**: Engine tests cover the declared-unmanaged `Backlog`, the configured non-unmanaged `Backlog`, and the unknown-column cases from FR-001 to FR-006.

### Key Entities *(if applicable)*

- **Parking column**: a board column whose items Fabrik runs no workflow for and which a human moves items out of. Either a configured unmanaged stage, or the legacy literal `Backlog` with no configured stage.
- **Promote refusal**: the structured refusal returned by `fabrik_promote`, carrying the item's current state, which includes `valid_targets`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On an install with no `backlog.yaml`, promoting an item from `Backlog` to a pipeline column succeeds in one call, including this repo's own dev instance.
- **SC-002**: Behaviour is unchanged for installs with a declared-unmanaged `Backlog`, for the startup and spawn compat-net sites, and for every other promote refusal reason (except the `valid_targets` presentation).
- **SC-003**: No promote refusal tied to the source column offers `valid_targets` as if the move were possible.
- **SC-004**: The legacy-install test fails against the old guard and passes with the fix, and `go test -race ./...` is clean.
- **SC-005**: The documentation bundle has no drift after regeneration.

## Assumptions

- The legacy compat match is the case-sensitive literal `Backlog`, as at the existing sites. Widening it is out of scope.
- Promote's other guards are unchanged: a valid `to`, loaded board metadata, the live column check, and the audit comment.
- Other consumers of "unmanaged" that read it directly from a stage (for example the attention classifier's idle codes) are not required to change. Adopting the shared predicate there is a judgement for Plan, so long as the sites named in FR-003 are covered and nothing regresses.
- For FR-007, omitting `valid_targets` or labelling them are both acceptable. Which one is a design choice for Plan.

## Out of Scope *(optional)*

- Making `refresh-stages` (or any command) create a missing `backlog.yaml`.
- New overseer tools or new promote parameters.
- Widening the compat net beyond the literal `Backlog`, or to other column names.
- Changing promote's other refusal reasons or the audit comment format.

## Source References *(optional)*

- `engine/overseer_actions.go`: `parkingRefusal` and `Promote`
- `engine/startup.go`: the legacy `Backlog` recognition
- `engine/spawn.go`: the `Backlog` fallback in spawn placement
- `docs/USER_GUIDE.md`: `fabrik_promote` and the refresh-stages notes
- `docs/state-machine.md` §7.15
- #1966, the human feature thread. It is mentioned for context only. The PR body must mention #1966 by bare number and must not use a closing keyword (`Closes`/`Fixes`/`Resolves`) near it.
