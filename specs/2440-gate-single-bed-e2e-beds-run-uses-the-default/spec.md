# Feature Specification: Single-bed `E2E_BEDS` gate run uses the bed's own repos and board

**Feature Branch**: `fabrik/issue-2440`
**Created**: 2026-10-25
**Status**: Specified
**Input**: User description: "## Problem

A single-bed gate run that names its bed with `E2E_BEDS` aims the live tests at the **default** repos and board, not the bed's own. `Gate.bedEnv()` (`tests/gate/beds.go`) returns only `FABRIK_TEST_DIR=<bed>` when `g.bed == nil` and `E2E_BEDS` is set; the repo pair and board that `resolveBedSpec` already reads from the bed's `.env` (`FABRIK_TEST_REPO_ALPHA`/`_BETA`, `FABRIK_TEST_PROJECT_OWNER`/`_NUMBER`) are injected only on a multi-bed run.

Seen 2026-10-25 running `E2E_EVENTS=webhook E2E_BEDS=$HOME/dev/fabrik-test-2 scripts/e2e/run.sh -run '^TestWebhookChangeSurvivesReconcile$'`: bed 2 watches `fabrik-test-gamma`/`fabrik-test-delta` on board #7, but the test filed `handarbeit/fabrik-test-alpha#9321`–`#9323` on bed 1's board, the bed never saw them, and the test ended INCONCLUSIVE three times. The delivery preflight also labelled its scratch issue on `fabrik-test-alpha` — it passed only because the bed's App is installed org-wide.

## Requirements

- A single-bed run whose bed comes from `E2E_BEDS` passes the bed's resolved repo pair and board to every leg's `go test` and to every gate-side live step (delivery preflight, board-lag probe, reset), exactly as a multi-bed run does — same precedence: bed `.env` > environment > defaults.
- A single-bed run via `FABRIK_TEST_DIR` (no `E2E_BEDS`) is unchanged.

## Acceptance

- A gate unit test (fake `Commander`) shows a single-bed `E2E_BEDS` run whose bed `.env` names other repos/board injects them into the leg env and the delivery-probe target; neutralising the fix makes it fail.

## Out of scope

- Multi-bed scheduling (already correct)."

## Background

The live e2e gate runner can drive more than one test bed. A bed is a separate checkout directory with its own `.env` that names the repository pair and the Projects board its engine watches. When several beds run concurrently, the gate resolves each bed's repo pair and board (bed `.env`, then environment, then defaults) and injects them into that bed's `go test` legs and gate-side live steps, so tests and probes target the bed that is actually listening.

A run that names exactly one bed through `E2E_BEDS` takes a different path: only the bed's directory is passed on. The live tests and the gate-side live steps therefore fall back to the default repos and board, which belong to a different bed. Nothing fails loudly: the test creates its fixtures where the chosen bed's engine never looks, so the test times out and is recorded INCONCLUSIVE. This was observed on 2026-10-25 with a webhook-coverage test run against bed 2. The test filed three scratch issues on bed 1's repo and board, bed 2 never saw them, and the test was INCONCLUSIVE three times. The gate's delivery preflight also labelled its scratch issue on the default repo. It only passed because the bed's GitHub App happens to be installed organisation-wide, so the misdirection was masked there.

The result is wasted live budget, misleading INCONCLUSIVE outcomes on the release gate's coverage ledger, and live fixtures written to a bed that did not ask for them.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A single named bed is exercised on its own repos and board (Priority: P1)

A release operator runs the live gate against one bed that is not the default bed by setting `E2E_BEDS` to that bed's directory. The bed's `.env` names its own repository pair and board. Every live leg and every gate-side live step must act on those, not on the defaults.

**Why this priority**: This is the defect. Without it a single-bed `E2E_BEDS` run cannot produce a valid result for any bed whose repos or board differ from the defaults, and it silently writes fixtures to another bed's board.

**Independent Test**: With a fake command runner, configure a single-bed `E2E_BEDS` run whose bed `.env` names repos and a board different from the defaults, run the gate, and inspect the environment handed to the leg's `go test` and the target of the delivery preflight. They must carry the bed's values.

**Acceptance Scenarios**:

1. **Given** `E2E_BEDS` names one bed whose `.env` sets a repo pair and board, **When** the gate runs a leg, **Then** the leg's `go test` environment carries that repo pair and board, not the defaults.
2. **Given** the same configuration, **When** the gate runs its delivery preflight, **Then** the scratch issue is created and labelled on the bed's own repo.
3. **Given** the same configuration, **When** the gate runs the board-listing lag probe, **Then** it adds and removes its draft item on the bed's own board.
4. **Given** the same configuration, **When** the gate resets the bed, **Then** the reset acts on the bed's own repos and board.

---

### User Story 2 - Resolution precedence matches a multi-bed run (Priority: P1)

The operator needs a single-bed `E2E_BEDS` run to resolve each setting the same way a multi-bed run does, so moving between one bed and several never changes which repos a bed means.

**Why this priority**: Two resolution rules for the same bed would reintroduce the divergence this issue removes.

**Independent Test**: For one bed, set a value in the bed `.env`, a different value in the environment, and leave a third setting unset. Confirm the resolved values for a single-bed run and for a multi-bed run containing that bed are identical, setting by setting.

**Acceptance Scenarios**:

1. **Given** a setting present in both the bed `.env` and the process environment, **When** the gate resolves the bed, **Then** the bed `.env` value wins.
2. **Given** a setting absent from the bed `.env` but present in the process environment, **When** the gate resolves the bed, **Then** the environment value is used.
3. **Given** a setting in neither, **When** the gate resolves the bed, **Then** the default is used.

---

### User Story 3 - A single-bed run without `E2E_BEDS` is unchanged (Priority: P2)

An operator who selects the single bed through `FABRIK_TEST_DIR` alone, with no `E2E_BEDS`, keeps today's behaviour exactly.

**Why this priority**: This is the common path today; it must not regress.

**Independent Test**: Run the existing gate tests for the `FABRIK_TEST_DIR`-only path unchanged and confirm they still pass with identical leg environments.

**Acceptance Scenarios**:

1. **Given** `E2E_BEDS` is unset and `FABRIK_TEST_DIR` names the bed, **When** the gate runs, **Then** the leg environment and gate-side live-step targets are the same as before this change.

---

### Edge Cases

- The bed `.env` sets only some of the settings, for example the repo pair but not the board. The unset ones follow the environment-then-default precedence individually.
- The bed `.env` is missing or unreadable. Behaviour matches what a multi-bed run does for the same bed.
- `E2E_BEDS` names a single bed whose `.env` values equal the defaults. The result is identical to today.
- `E2E_BEDS` lists more than one bed. Multi-bed behaviour is unchanged.
- Environment variables already set in the operator's shell that name repos or a board: a bed `.env` value still wins over them, as in a multi-bed run.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: In a single-bed run whose bed comes from `E2E_BEDS`, the gate MUST pass the bed's resolved repo pair and board (project owner and number) into the environment of every leg's `go test`.
- **FR-002**: In that same run, every gate-side live step that touches GitHub (the delivery preflight, the board-listing lag probe and the bed reset) MUST target the bed's resolved repo pair and board, not the defaults.
- **FR-003**: The resolved values MUST use the same precedence as a multi-bed run: bed `.env`, then process environment, then defaults, applied per setting.
- **FR-004**: A single-bed run that selects its bed through `FABRIK_TEST_DIR` without `E2E_BEDS` MUST behave exactly as it does today.
- **FR-005**: Multi-bed scheduling and per-bed resolution MUST NOT change.
- **FR-006**: A gate unit test using a fake command runner MUST show that a single-bed `E2E_BEDS` run, whose bed `.env` names a repo pair and board other than the defaults, injects them into the leg environment and into the delivery-preflight target. The test MUST fail when the fix is neutralised.

### Key Entities *(if applicable)*

- **Bed**: A test checkout directory with its own `.env`, engine, GitHub App identity and, through that `.env`, its own repository pair and board.
- **Bed spec**: The resolved view of a bed: its directory, repository pair (alpha and beta), and project owner and number, after applying the precedence rule.
- **Leg environment**: The environment handed to a leg's `go test` run, which the live tests read to decide where to create fixtures.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A single-bed `E2E_BEDS` run against a bed with non-default repos and board creates all live-test and probe fixtures on that bed's repos and board, and none on the defaults.
- **SC-002**: For any one bed, the resolved repo pair and board are identical whether the bed is run alone through `E2E_BEDS` or as a member of a multi-bed run.
- **SC-003**: The existing `FABRIK_TEST_DIR`-only gate behaviour is unchanged: its existing tests pass without modification.
- **SC-004**: The new gate test fails when the fix is removed and passes when it is present.

## Assumptions

- The bed `.env` keys already read by the gate for multi-bed runs (alpha and beta repo names, project owner, project number) are the complete set that needs to flow through; no new configuration keys are introduced.
- "Every gate-side live step" means those the gate runs itself against GitHub today: the delivery preflight, the board-listing lag probe and the reset. Any other step that already reads the bed's per-bed values on a multi-bed run is covered by the same rule.
- A single-bed run that comes from `E2E_BEDS` is treated by the gate as a one-element bed list, so it should resolve the way an element of a longer list does.

## Out of Scope *(optional)*

- Multi-bed scheduling, which is already correct.
- Changing how beds are selected, listed or validated.
- Changing the live tests themselves or how they pick default repos when no bed is named.
- Making the delivery preflight or tests fail loudly on a misdirected target beyond what correct resolution provides.

## Source References *(optional)*

- Observed failure on 2026-10-25: `E2E_EVENTS=webhook E2E_BEDS=$HOME/dev/fabrik-test-2 scripts/e2e/run.sh -run '^TestWebhookChangeSurvivesReconcile$'`, ending INCONCLUSIVE three times.
- Gate runner multi-bed design: `adrs/1976-*` and the "Multi-bed" description of `tests/gate/` in `CLAUDE.md`.
