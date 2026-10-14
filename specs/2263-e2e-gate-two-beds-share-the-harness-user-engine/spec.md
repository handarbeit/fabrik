# Feature Specification: Split engine vs. harness identity use so two e2e beds can share the harness user

**Feature Branch**: `fabrik/issue-2263`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

#1990 adds a second test bed so the gate's non-baseline cells run while bed A runs the `app/on` baseline. With the current identity rule (#1976, ADR-1976 D4), that cannot happen.

Every cell charges `identitySet(auth)`: its engine identity plus the **harness token's user**. Both are held exclusively by `multiSched` for the whole leg. Both beds' harness token is `@arbeithand`. GitHub's ToS allows one free machine account per person, so a second machine user is not an option. So:
- bed A's baseline holds `user:arbeithand` for its whole 3h18m;
- every cell bed B could run also needs `user:arbeithand`;
- bed B therefore waits for the entire baseline, and a two-bed gate takes as long as a one-bed gate.

The exclusivity exists to protect each identity's 5,000/h GraphQL budget. The v0.0.84 gate's numbers (#1678) show it is far stronger than the budget needs for **harness** use. arbeithand's lowest point across all four cells was 3,800/5,000 (76%), and that was after `pat/on`, where arbeithand was also the *engine* identity. On app cells arbeithand is only the harness, and its spend there is a fraction of that.

## Requirements

- **R1: Split engine use from harness use.** A cell's identity set records how it uses each identity: `engine` (the identity the bed engine authenticates as on this leg) or `harness` (the bed token's user, when it is not also the engine identity).
  - An identity held for **engine** use is exclusive: no other cell may hold it in any role. Two engines must never share one identity.
  - An identity held for **harness** use is shared: any number of cells may hold it for harness use at once.
  - A `pat` cell's engine identity is the bed token's user, so it needs that user exclusively. It waits while another bed's cell uses the same user, even only as harness.
- **R2: Budget floor for shared identities.** Before a cell that shares an identity starts, the scheduler reads that identity's remaining GraphQL budget (the existing `Budget` probe). It starts the cell only if the remaining budget is at least `E2E_SHARED_IDENTITY_FLOOR`, default 40% of the limit. Otherwise it waits, logging the identity, its remaining budget and its reset time, and re-probes when the next leg finishes or after five minutes.
  - An unreadable budget is treated as \"below floor\" for starting a *second* holder, never as a failure.
  - A single holder needs no probe, so one-bed runs are unchanged.
- **R3: Exhaustion still voids.** RUN INVALID on a shared identity (the existing `exhausted` set) still stops every cell that charges it, in either role.
- **R4: Logs.** The `== waiting: …` line says whether the blocker is exclusive use or the budget floor. Each leg's start and end budget lines (R5 of #1976) mark each identity's role.
- **R5: Docs.** Amend ADR-1976 D4 (or add ADR for this issue) with the engine/harness split and the #1678 numbers behind it. Add the \"second bed\" section that #1990 R1 asks for to `tests/e2e/README.md` and `tests/gate/README.md`: the bed 2 prerequisites (App `fabrik-bed-2`, repos `handarbeit/fabrik-test-gamma`/`-delta`, board handarbeit #7, checkout `~/dev/fabrik-test-2`), how to verify each, the `FABRIK_TEST_*` keys in the bed `.env`, and how to retire a bed.

## Scope

`tests/gate` only (`identity.go`, `multibed.go`, their tests), plus the docs. No engine change. The single-bed run must be byte-for-byte unchanged: same schedule, same output, apart from R4's role marker.

## Acceptance

Scheduler tests with fake identity ops show:
- an app cell on bed B runs concurrently with bed A's `app/on`, sharing `user:arbeithand` as harness;
- a `pat` cell on bed B waits while bed A holds `user:arbeithand` in any role, and bed A's `pat` cell waits while bed B's app cell holds it as harness;
- a below-floor budget blocks a second holder and logs the floor reason, and the cell starts once the budget recovers;
- RUN INVALID on a shared identity stops both beds' cells that charge it.

Each test is shown to be non-vacuous by neutralising the role split. The live two-bed certification run itself is #1990."

## Background

The live e2e gate can run several beds concurrently (#1976, ADR-1976). The scheduling unit is the GitHub *identity*, not the bed. Every cell currently charges an identity set: the engine identity (`app:<installation>` on an app leg, or the bed token's user on a `pat` leg) plus the bed token's user as harness. All identities in the set are held exclusively for the whole leg. The rule exists to protect each identity's 5,000/h GraphQL budget and to keep two engines from ever polling as one identity.

Both beds' harness token is `@arbeithand`, and GitHub allows only one free machine account per person, so a second harness user is not available. Under the current rule bed B can run no cell while bed A's baseline (`app/on`, about 3h18m) holds `user:arbeithand`. A two-bed gate therefore takes as long as a one-bed gate and the second bed (#1990) buys nothing.

The v0.0.84 gate's measurements (#1678) show exclusivity is much stronger than the budget needs for harness use. Across all four cells arbeithand's lowest remaining budget was 3,800 of 5,000 (76%). That low point came after `pat/on`, where arbeithand was also the engine identity. On app cells it is only the harness, and its spend there is a fraction of that. Engine use needs exclusivity (two engines on one identity is never acceptable, and the competing-consumer check relies on it). Harness use does not, provided a budget floor is enforced.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Non-conflicting cells on two beds run concurrently (Priority: P1)

An operator runs the release gate with two beds. Bed A runs the `app/on` baseline. Bed B takes a cell that uses a different engine identity but the same harness user. Bed B starts immediately instead of waiting for bed A's whole leg.

**Why this priority**: This is the entire point of the change. Without it the second bed does not shorten a gate run.

**Independent Test**: With fake identity ops where both beds resolve to the same harness login, start bed A's `app/on` and offer bed B an app cell. Both hold `user:arbeithand` as harness and run concurrently.

**Acceptance Scenarios**:

1. **Given** bed A holds `app:<A>` (engine) and `user:arbeithand` (harness) and the shared identity's budget is at or above the floor, **When** bed B is offered an app cell with a different installation, **Then** it starts at once and holds `user:arbeithand` as harness.
2. **Given** the same state, **When** the single-bed run is executed, **Then** the schedule and output are unchanged except for the role marker in the budget lines.

---

### User Story 2 - An identity used by an engine is never shared (Priority: P1)

An engine must never share an identity with any other cell, in any role. A `pat` cell authenticates the bed engine as the bed token's user, so that user is held exclusively.

**Why this priority**: Two engines on one identity corrupts both runs and defeats the competing-consumer check. This is the safety invariant the split must preserve.

**Independent Test**: With fake identity ops, hold `user:arbeithand` as harness on one bed and offer a `pat` cell on the other bed, and the reverse. The `pat` cell waits in both directions.

**Acceptance Scenarios**:

1. **Given** bed A holds `user:arbeithand` in any role, **When** bed B is offered a `pat` cell (engine = `user:arbeithand`), **Then** it waits and logs that the blocker is exclusive use.
2. **Given** bed B's app cell holds `user:arbeithand` as harness, **When** bed A is offered its `pat` cell, **Then** it waits.
3. **Given** a `pat` cell holds `user:arbeithand` as engine, **When** any other cell charging that user in either role is offered, **Then** it waits.
4. **Given** two cells whose engine identities are the same `app:<N>`, **When** both are offered, **Then** the second waits.

---

### User Story 3 - A shared identity's budget floor protects the harness (Priority: P2)

Before a cell becomes a second holder of a shared identity, the scheduler checks that identity's remaining GraphQL budget. A cell that would share an identity below the floor waits.

**Why this priority**: The floor is what makes sharing safe. It is the replacement for blanket exclusivity, but it only comes into play once User Story 1 exists.

**Independent Test**: With a fake `Budget` that reports remaining budget below the floor, a second harness holder is blocked and the floor reason is logged. Raising the reported budget lets the cell start.

**Acceptance Scenarios**:

1. **Given** a shared identity already has one holder and its remaining budget is below `E2E_SHARED_IDENTITY_FLOOR` (default 40% of the limit), **When** a second holder is offered, **Then** it waits, logging the identity, remaining budget and reset time.
2. **Given** that wait, **When** the next leg finishes or five minutes pass, **Then** the budget is re-probed and the cell starts once it is at or above the floor.
3. **Given** the budget probe fails, **When** a second holder is offered, **Then** it is treated as below floor and waits, and no failure is raised.
4. **Given** an identity with no current holder, **When** a cell wants it, **Then** no budget probe is made.

---

### User Story 4 - Exhaustion on a shared identity stops every cell that charges it (Priority: P2)

If a leg ends RUN INVALID because a shared identity's budget was exhausted, no other cell charging that identity, in either role, starts afterwards, on either bed.

**Why this priority**: It preserves the existing failure semantics (#1976 D8) under the new sharing.

**Independent Test**: Mark a shared identity exhausted and check that neither bed starts a remaining cell that charges it.

**Acceptance Scenarios**:

1. **Given** `user:arbeithand` is marked exhausted, **When** either bed would start a cell charging it as engine or as harness, **Then** the cell is not started and appears in the closing summary as never started, with the reason.

---

### User Story 5 - Operators can see why a cell waits and how each identity is used (Priority: P3)

The wait line and the per-leg budget lines tell the operator the role and the blocker type.

**Why this priority**: Observability. It does not change scheduling but makes a long wait understandable.

**Independent Test**: Assert the log text for exclusive-use waits, floor waits, and the start/end budget lines.

**Acceptance Scenarios**:

1. **Given** a cell waits on exclusive use, **When** the wait is logged, **Then** the `== waiting: …` line says it is exclusive use and names the identity and holder.
2. **Given** a cell waits on the budget floor, **When** the wait is logged, **Then** the line says it is the budget floor and includes the identity, remaining budget and reset time.
3. **Given** a leg starts or ends, **When** the identity budget lines are logged, **Then** each marks its identity as `engine` or `harness`.

---

### User Story 6 - Documentation for the second bed (Priority: P3)

An operator can stand up, verify and retire bed 2 from the docs, and the engine/harness decision is recorded.

**Why this priority**: #1990's live certification run depends on these instructions, but they do not affect scheduler behavior.

**Independent Test**: Read the docs and follow them without outside knowledge. Check that the ADR carries the #1678 numbers.

**Acceptance Scenarios**:

1. **Given** the docs, **When** an operator follows the second-bed section, **Then** they find the prerequisites (App `fabrik-bed-2`, repos `handarbeit/fabrik-test-gamma` and `-delta`, board handarbeit #7, checkout `~/dev/fabrik-test-2`), how to verify each, the `FABRIK_TEST_*` keys in the bed `.env`, and how to retire a bed.
2. **Given** ADR-1976, **When** D4 is read, **Then** it is amended (or a new ADR for this issue links to it) with the engine/harness split and the #1678 numbers.

---

### Edge Cases

- A `pat` cell's engine identity and harness user are the same login: it is a single `engine` entry, never also `harness`, and is exclusive.
- An app cell where the bed token's user is also another bed's `pat` engine identity: the app cell holds the user as harness, so it is blocked while any `pat` cell holds it, and a `pat` cell is blocked while it holds it.
- Three or more holders of one shared identity: each additional holder runs the floor check before starting.
- Bed A's waiting cell reserves its identities (#1976). A reservation must conflict with other cells only where the roles would conflict. Bed A's reserved baseline `app/on` must not block bed B's harness-only sharing, but it must still keep another cell from taking its engine identity.
- Budget probe errors or an unresolvable reset time: treated as below floor for a second holder, logged, never a failure.
- Identity resolution falling back to `token:<hash>` (no resolver configured): equal tokens still collide, as exclusive use for engine and shared for harness by the same rules.
- `E2E_SHARED_IDENTITY_FLOOR` unset, empty, or invalid: the 40% default applies.
- A single-bed run, where nothing shares an identity: no budget probe is made and the schedule is identical.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Each identity in a cell's identity set carries a role, `engine` or `harness`. The engine identity is the identity the bed engine authenticates as on that leg. The harness role applies to the bed token's user when it is not also the engine identity.
- **FR-002**: An identity held in the `engine` role is exclusive. No other cell may hold it in either role while it is held.
- **FR-003**: An identity held in the `harness` role may be held by any number of cells in the `harness` role at once, subject to FR-005.
- **FR-004**: A `pat` cell's engine identity is the bed token's user. It is held exclusively and waits while any other cell holds that user in either role. A cell holding that user as harness likewise waits while a `pat` cell holds it as engine.
- **FR-005**: Before a cell becomes a second (or later) holder of a shared identity, the scheduler reads that identity's remaining GraphQL budget with the existing `Budget` probe. It starts the cell only if remaining is at least the floor given by `E2E_SHARED_IDENTITY_FLOOR`, default 40% of the identity's limit.
- **FR-006**: A cell blocked by the floor waits and logs the identity, remaining budget and reset time. It re-probes when the next leg finishes or after five minutes, whichever is first.
- **FR-007**: An unreadable budget is treated as below floor for starting a second holder. It is never raised as a failure.
- **FR-008**: A cell that would be the only holder of every identity it needs makes no budget probe. A single-bed run's schedule and output are unchanged apart from the role marker of FR-011.
- **FR-009**: A cell's whole identity set is acquired atomically, as today, applying the role rules of FR-002 to FR-005 to each identity.
- **FR-010**: RUN INVALID on a shared identity marks it exhausted. No cell charging it in either role starts afterwards, on any bed. Cells already running are handled as today.
- **FR-011**: The `== waiting: …` line states whether the blocker is exclusive use or the budget floor. Each leg's start and end identity budget lines mark every identity's role.
- **FR-012**: `E2E_SHARED_IDENTITY_FLOOR` accepts a value in a form the implementation documents, and an unset or invalid value falls back to 40%.
- **FR-013**: ADR-1976 D4 is amended, or a new ADR numbered for this issue is added, recording the engine/harness split and the #1678 numbers (arbeithand's lowest point 3,800 of 5,000, 76%, after `pat/on`).
- **FR-014**: `tests/e2e/README.md` and `tests/gate/README.md` each gain a second-bed section covering the bed 2 prerequisites (App `fabrik-bed-2`, repos `handarbeit/fabrik-test-gamma` and `-delta`, board handarbeit #7, checkout `~/dev/fabrik-test-2`), how to verify each, the `FABRIK_TEST_*` keys in the bed `.env`, and how to retire a bed.
- **FR-015**: Scheduler tests using fake identity ops cover the four acceptance cases: concurrent app cell sharing the harness user; `pat` waits in both directions; floor block and recovery; exhaustion stopping both beds' cells. Each test is shown to fail when the role split is neutralised.
- **FR-016**: Docs that change under the canonical doc set regenerate `docs/llms-full.txt` in the same change if any canonical page is touched. The gate READMEs and ADRs are not part of that set.

### Key Entities *(if applicable)*

- **Identity**: A GitHub principal whose 5,000/h GraphQL budget is spent, either an App installation (`app:<N>`) or a user (`user:<login>`, or `token:<hash>` when unresolved).
- **Identity role**: `engine` (the principal the bed engine authenticates as on a leg; exclusive) or `harness` (the principal the bed token uses to drive the test harness; shareable above the floor).
- **Identity set**: The set of (identity, role) pairs a cell charges for its whole leg.
- **Budget floor**: The minimum remaining fraction of an identity's GraphQL limit required before a second holder may start.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With two beds sharing a harness user, a non-conflicting cell on bed B starts within one scheduling pass of bed A's baseline, instead of after the baseline completes.
- **SC-002**: In every scheduler test, no two cells ever hold the same identity when either holds it in the `engine` role.
- **SC-003**: A second holder of a shared identity never starts while the identity's remaining budget is below the floor or unreadable, and starts once it is at or above the floor.
- **SC-004**: With one bed, the schedule and gate output are identical to before apart from the role marker on the budget lines.
- **SC-005**: Each acceptance test fails when the role split is neutralised, showing it is not vacuous.
- **SC-006**: An operator can stand up, verify and retire bed 2 using only the READMEs.

## Assumptions

- The default floor is 40% of the identity's rate-limit ceiling, taken from the `Budget` probe's reported limit rather than a hard-coded 5,000. The `E2E_SHARED_IDENTITY_FLOOR` value is read as a percentage; the implementation documents the exact accepted form.
- The budget check applies only when another cell already holds the identity. The first holder is never probed.
- The "next leg finishes" re-probe is a wake-up signal from the scheduler's existing leg completion. The five-minute timer is a fallback.
- App identities are always `engine`. The `harness` role is only ever the bed token's user when it differs from the engine identity.
- Reservations (bed A's next cell reserving its identities) keep the role semantics. A reserved `harness` identity does not block other harness sharing, while a reserved `engine` identity blocks everyone.
- The competing-consumer check (#1684) and the idle-engine stopping rule are unchanged. They are what keep an engine from spending an identity another bed's leg holds, and exclusive engine use is what keeps them valid.
- The new ADR, if used rather than an amendment, is numbered `adrs/2263-…` per the issue-number convention. The choice between amending ADR-1976 D4 and adding a new ADR is left to the implementer.
- Bed 2's prerequisites named in R5 are as given in #1990; this issue documents them and does not create them.

## Out of Scope *(optional)*

- Any engine change, including how the engine authenticates.
- Creating bed 2 (App, repos, board, checkout) and the live two-bed certification run (#1990).
- A second harness machine user (not permitted by GitHub's terms).
- Changing the 5,000/h budget accounting, the sparse matrix, or the RUN INVALID detection logic.
- Per-identity floors other than the single configured value.

## Source References *(optional)*

- #1976 / ADR-1976 D3, D4, D8: multi-bed identity scheduling
- #1678: v0.0.84 gate budget measurements
- #1990: second test bed and its live certification run
- `tests/gate/README.md` "Multi-bed runs" and `tests/e2e/README.md` multi-bed section
