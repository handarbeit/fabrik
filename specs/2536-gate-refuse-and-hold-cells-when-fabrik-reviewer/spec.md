# Feature Specification: Release gate guards the reviewer token's rate-limit budget

**Feature Branch**: `fabrik/issue-2536`
**Created**: 2026-11-05
**Status**: Specified
**Input**: User description: "## Problem

The release gate checks the engine token's budget and its competing consumers, but it never looks at `FABRIK_REVIEWER_TOKEN`. That is the second, non-author identity the live tests use to request reviews, submit verdicts and post \"human\" comments.

On the 0.0.91 gate (engine `000885c6`, 2026-10-30), the reviewer token's REST budget was already 0/5000 when the `pat/off` and `app/off` legs started:

- Every test that touched it failed at its first REST call (`TokenLogin` → `gh api user`) with `API rate limit exceeded for user ID 82237` (HTTP 403):
  - `TestConjunctiveCIReviewGate` (both beds)
  - `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation`
  - `TestAuthReviewGateIdentity`
- Because a leg failed, the four remaining cells never started.
- Coverage came out at 65 of 366, after roughly 1.5 hours of live spend.
- After the reset, something outside the gate was still using about 55 REST calls a minute on that account. The reviewer token belongs to a personal account whose budget other local sessions share, and the gate cannot see them.

The gate already has the right shape for this, applied to the engine token only:

- `CheckCompetingTokenConsumers` (#1684) refuses with exit 7.
- `logIdentityBudget` reports per-identity GraphQL budget at each leg's start and end.

## Requirements

- **R1, preflight refusal.** Before any live budget is spent, read each configured bed's `FABRIK_REVIEWER_TOKEN` REST `core` budget and GraphQL budget.
  - Refuse with `ExitPreconditionFailed` (7) when either is below a threshold. Each threshold has a sane default and can be overridden by an env variable.
  - The refusal names the token's login, the remaining/limit figures and the reset time, never the token value.
  - Beds that share one reviewer token are read once.
  - **The REST core figure is the one that matters here, and it is live.** `budget.go`'s note that REST `rate_limit` is a dead gauge applies to the GraphQL bucket only. On 2026-10-30, `rate_limit`'s `core` read 0/5000 exactly when GitHub refused the calls. Read `core` from `GET /rate_limit` (that endpoint costs no budget) or from the response headers.
- **R2, cell start check.** Before each cell starts, re-check the reviewer token the same way.
  - If its budget is below the threshold and resets within a bounded wait, hold the cell (logged, the same way an identity wait is logged today), then re-check.
  - Otherwise refuse to start it with the existing \"budget exhausted\" outcome, so its pairs stay missing and are not recorded as failures.
  - A running leg is never killed by this.
- **R3, budget reporting.** Include the reviewer identity in `logIdentityBudget`'s start and end lines, as REST core plus GraphQL. The leg archive then shows how much each leg spent and how much someone else spent meanwhile.
- **R4, competing-consumer check.** Extend the #1684 check to the reviewer token: list local processes whose environment carries it, excluding every bed's own engine, and refuse naming each PID and directory. `E2E_SKIP_TOKEN_CHECK` still skips it. Processes not visible from this machine stay out of scope; R1 and R2 are the cover for them.
- **R5, never a hang.** Every new read goes through the bounded `Exec` path (`GHAPITimeout`). A probe that cannot be read is reported as `unavailable` and never blocks, the same rule as the existing probes (#1676).
- **R6, no secrets.** Token values are never logged or archived; key names and logins only.

## Scope

- In: `tests/gate` (preflight, multi-bed scheduler cell start, identity budget logging, consumers), its unit tests with fake `Commander`/identity ops, `tests/gate/README.md`, and `tests/e2e/README.md` where it describes `FABRIK_REVIEWER_TOKEN`.
- Out: the engine, and moving the reviewer to a dedicated account (an operator change).

## Acceptance

- A unit test covers each of these, with a non-vacuity control that disables the new check and shows the old behaviour (the gate proceeds):
  - A gate run whose reviewer token reads `core` 0/5000 exits 7 before the pre-gate's live preflights spend anything, naming the login and the reset time.
  - A cell whose reviewer budget falls below the threshold between cells is held, or not started, and records no failures.
  - An unreadable reviewer probe is reported and does not block.
- The leg start and end budget lines include the reviewer identity.
- `go test -race ./tests/gate/...` passes."

## Background

The live e2e release gate (`tests/gate`) already protects the engine's GitHub identity: `CheckCompetingTokenConsumers` (#1684) refuses when another local process uses the engine token, and `logIdentityBudget` records each identity's GraphQL budget at the start and end of every leg. It is blind to `FABRIK_REVIEWER_TOKEN`, the second, non-author identity the live tests use to request reviews, submit verdicts and post "human" comments.

On the 0.0.91 gate (engine `000885c6`, 2026-10-30) that token's REST `core` budget was already 0/5000 when the `pat/off` and `app/off` legs began. Every test touching it failed at its first REST call (`gh api user` via `TokenLogin`) with HTTP 403 `API rate limit exceeded for user ID 82237`: `TestConjunctiveCIReviewGate` (both beds), `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` and `TestAuthReviewGateIdentity`. Because a leg failed, the four remaining cells never started, leaving coverage at 65 of 366 after about 1.5 hours of live spend. After the reset, another consumer on that account was still using about 55 REST calls a minute: the reviewer token belongs to a personal account whose budget other local sessions share, and the gate cannot see them.

The REST `core` figure is the signal that matters and it is live: the existing note that REST `rate_limit` is a dead gauge applies to the GraphQL bucket only. On 2026-10-30 `core` read 0/5000 exactly when GitHub refused the calls, and `GET /rate_limit` costs no budget.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Refuse to start a gate that cannot succeed (Priority: P1)

An operator starts the release gate while the reviewer token's budget is exhausted or nearly so. The gate stops before spending any live budget and tells the operator which account is out and when it recovers.

**Why this priority**: This is the failure that wasted about 1.5 hours of live spend and an entire gate run. Catching it up front is the core value.

**Independent Test**: Run the gate with a fake command layer that reports reviewer `core` 0/5000; assert exit code 7, that nothing live ran, and that the message names the login and reset time but not the token.

**Acceptance Scenarios**:

1. **Given** a reviewer token whose REST `core` budget is below the threshold, **When** the gate starts, **Then** it exits 7 before the pre-gate's live preflights spend anything, naming the token's login, remaining/limit and reset time.
2. **Given** a reviewer token whose GraphQL budget is below its threshold, **When** the gate starts, **Then** it exits 7 the same way.
3. **Given** several beds that share one reviewer token, **When** the gate starts, **Then** that token is read once.
4. **Given** a threshold override env variable, **When** the gate starts, **Then** the override replaces the default for that bucket.

---

### User Story 2 - Hold or skip a cell instead of failing it (Priority: P1)

Between cells, someone else drains the reviewer token. The next cell is held until the budget recovers if the reset is near, and otherwise is not started, so its (test, leg) pairs stay missing and are resumable instead of recorded as failures.

**Why this priority**: The coverage ledger is built on resumable gaps; a failure caused by an external quota must not poison it or abort later cells.

**Independent Test**: With a fake budget reader whose value drops below threshold before a cell, assert the cell is held (logged) and then started when the budget recovers within the wait, or is not started with the "budget exhausted" outcome and no recorded failure when it does not.

**Acceptance Scenarios**:

1. **Given** the budget is below threshold and resets within the bounded wait, **When** the next cell is about to start, **Then** the cell is held with a logged reason, then the budget is re-checked and the cell starts once it recovers.
2. **Given** the budget is below threshold and does not reset within the bounded wait, **When** the next cell is about to start, **Then** it is not started, the existing "budget exhausted" outcome is used, and its pairs remain missing rather than failed.
3. **Given** a leg is already running, **When** the reviewer budget drops, **Then** the running leg is not interrupted.

---

### User Story 3 - See reviewer spend per leg (Priority: P2)

An operator reading the leg archive can see the reviewer identity's REST `core` and GraphQL budget at each leg's start and end, and so tell how much the leg spent and how much someone else spent meanwhile.

**Why this priority**: Diagnostic value; it is how the next exhaustion is explained, but it prevents nothing by itself.

**Independent Test**: Run a leg with fake budget readers and assert both budget lines contain the reviewer identity, its `core` and GraphQL figures, and no token value.

**Acceptance Scenarios**:

1. **Given** a leg starts and ends, **When** `logIdentityBudget` runs, **Then** both lines include the reviewer identity as REST core plus GraphQL.

---

### User Story 4 - Name other local processes using the reviewer token (Priority: P2)

The existing competing-consumer check also covers the reviewer token, so a stray local session using it is named before the gate runs.

**Why this priority**: It explains the 55 calls a minute seen after the reset when the culprit is on the same machine; remote consumers stay covered only by User Stories 1 and 2.

**Independent Test**: With a fake process listing in which a non-bed process carries the reviewer token in its environment, assert refusal with exit 7 naming each PID and directory; with `E2E_SKIP_TOKEN_CHECK` set, assert it is skipped.

**Acceptance Scenarios**:

1. **Given** a local process (not any bed's own engine) whose environment carries the reviewer token, **When** the check runs, **Then** the gate refuses with exit 7 naming each PID and directory.
2. **Given** only the beds' own engines carry the token, **When** the check runs, **Then** it passes.
3. **Given** `E2E_SKIP_TOKEN_CHECK` is set, **When** the check runs, **Then** the reviewer-token part is skipped as well.

---

### Edge Cases

- The budget probe errors, times out or returns unparseable output: reported as `unavailable`, never blocks and never hangs (User Story 1 and 2 behave as if the check were absent for that read).
- No reviewer token is configured on a bed: nothing to check for that bed.
- Two beds configured with the same reviewer token: one read, one report.
- `core` has reset between preflight and cell start: the cell-start re-check sees the fresh value and proceeds.
- Budget is below threshold with a reset time already past or missing: treated as not recovering within the bounded wait, so the cell is not started.
- Process listing cannot read some processes' environments: those are skipped, as for the engine token.
- Output, logs and archive entries never contain a token value, only key names and logins.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Before any live budget is spent, the gate MUST read, for each configured bed, the `FABRIK_REVIEWER_TOKEN` account's REST `core` budget and GraphQL budget.
- **FR-002**: The gate MUST refuse with `ExitPreconditionFailed` (7) when either figure is below its threshold; each threshold MUST have a default and be overridable by an environment variable.
- **FR-003**: The refusal message MUST name the token's login, the remaining and limit figures and the reset time, and MUST NOT contain the token value.
- **FR-004**: Beds sharing one reviewer token MUST cause a single read of that token.
- **FR-005**: The REST `core` figure MUST be read from a source that costs no budget (`GET /rate_limit` or response headers) and MUST be treated as live; the existing "REST gauge is dead" treatment applies to the GraphQL bucket only.
- **FR-006**: Before each cell starts, the gate MUST re-check the reviewer token's budget in the same way.
- **FR-007**: When the budget is below threshold and resets within a bounded wait, the gate MUST hold the cell, log the hold in the same way an identity wait is logged today, and re-check afterwards.
- **FR-008**: When the budget is below threshold and does not reset within the bounded wait, the gate MUST NOT start the cell and MUST use the existing "budget exhausted" outcome, so the cell's pairs stay missing and are not recorded as failures.
- **FR-009**: The cell-start check MUST NOT interrupt a leg that is already running.
- **FR-010**: `logIdentityBudget`'s start and end lines MUST include the reviewer identity, as REST `core` plus GraphQL.
- **FR-011**: The competing-consumer check (#1684) MUST also list local processes whose environment carries the reviewer token, excluding every bed's own engine, and refuse naming each PID and directory; `E2E_SKIP_TOKEN_CHECK` MUST still skip it.
- **FR-012**: Every new read MUST go through the bounded `Exec` path (`GHAPITimeout`); a read that cannot be completed MUST be reported as `unavailable` and MUST NOT block the gate or a cell.
- **FR-013**: Token values MUST NOT be logged or archived; only variable names and logins may appear.
- **FR-014**: Each of the following MUST have a unit test using fake `Commander`/identity ops, each with a non-vacuity control that disables the new check and shows the old behaviour (the gate proceeds): a `core` 0/5000 reviewer token exits 7 before the pre-gate's live preflights spend anything, naming login and reset time; a cell whose reviewer budget falls below threshold between cells is held or not started and records no failures; an unreadable reviewer probe is reported and does not block.
- **FR-015**: `tests/gate/README.md`, and `tests/e2e/README.md` where it describes `FABRIK_REVIEWER_TOKEN`, MUST describe the new checks, their thresholds and the environment overrides.

### Key Entities *(if applicable)*

- **Reviewer identity**: the account behind a bed's `FABRIK_REVIEWER_TOKEN`; has a login, a REST `core` budget and a GraphQL budget, each with remaining, limit and reset time. Possibly shared by several beds.
- **Budget threshold**: the minimum remaining figure per bucket below which the gate refuses or holds; has a default and an env override.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A gate started with an exhausted reviewer token exits 7 with zero live GitHub or Claude calls spent, and the message identifies the account and its reset time.
- **SC-002**: A mid-run reviewer-budget drop never produces a recorded test failure; affected pairs remain reported as missing and are picked up by a later `--resume`.
- **SC-003**: Every leg archive's start and end budget lines show the reviewer identity's REST `core` and GraphQL figures.
- **SC-004**: An unreadable reviewer probe never blocks or delays the gate beyond the existing bounded timeout.
- **SC-005**: No token value appears in any log, archive or error message produced by the new checks.
- **SC-006**: `go test -race ./tests/gate/...` passes, including the non-vacuity controls.

## Assumptions

- Default thresholds are chosen conservatively relative to a full gate's reviewer usage; exact defaults are a planning decision, and are documented alongside their override variables.
- The bounded wait for a cell hold is a fixed, documented limit (not unbounded), analogous to the existing identity wait.
- The reviewer token is a personal-account token whose login is obtainable from a budget-free or already-bounded call; a failure to resolve the login is treated as `unavailable`, not a refusal.
- GraphQL budget is read the way the engine identity's is read today; only the REST `core` reading is new.
- Processes on other machines that use the account cannot be detected; User Stories 1 and 2 are the cover.

## Out of Scope *(optional)*

- Engine changes.
- Moving the reviewer to a dedicated GitHub account (an operator change).
- Detecting reviewer-token consumers outside this machine.
- Killing or pausing a leg that is already running because of a budget drop.

## Source References *(optional)*

- #1684: `CheckCompetingTokenConsumers`; #1676: bounded-probe rule (unreadable → `unavailable`).
- 0.0.91 gate run on engine `000885c6`, 2026-10-30.
