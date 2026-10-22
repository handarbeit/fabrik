# Feature Specification: Cut idle wall-clock from wait-dominated live e2e tests

**Feature Branch**: `fabrik/issue-2417`
**Created**: 2026-10-21
**Status**: Specified
**Input**: User description:

> ## Problem
>
> The live e2e gate is dominated by wall-clock waiting, not work. Summed mean pass time across the live suite is ~8 hours **per leg** (51 tests measured from the 0.0.88 and 0.0.89 gate logs), and a full-matrix release runs four legs. Most of what the live suite proves is that a state machine and a set of parallel state-change drivers (poll, webhooks, workers, merge train, settle scans) don't misbehave — that needs real GitHub and real timing, but rarely needs real *minutes* of it.
>
> This is **not** a proposal to move coverage to the sim bed: the sim is a pre-gate, never a replacement (ADR-1454), and every live scenario stays live. The target is idle wall-clock that buys no evidence — timeouts waited out at production values, negative windows with flat margins, mis-anchored deadlines.
>
> The slowest tests (mean / max seconds, n runs):
>
> | test | mean | max | n |
> |---|---|---|---|
> | TestExpectedReviewersDeclaredWaitsAndReprompts | 1952 | 2034 | 8 |
> | TestConvergenceRace | 1883 | 2339 | 10 |
> | TestConjunctiveCIReviewGate | 1872 | 1993 | 10 |
> | TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation | 1693 | 2030 | 10 |
> | TestCrossRepoSpawn | 1478 | 1629 | 16 |
> | TestMergeTrainConflictBisectPrefixRerere | 1364 | 1383 | 3 |
> | TestMergeTrainRunawayGuardPausesBatch | 1307 | 1329 | 3 |
> | TestMergeTrainBisectionEjectsPoisoner | 1018 | 1061 | 6 |
>
> Worked example — `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` (`tests/e2e/expected_reviewers_test.go`): ~2 minutes of real activity (seed, request reviewer, submit REQUEST_CHANGES, see the review-reinvoke dispatch), then a **negative-observation window** of `FABRIK_REVIEW_WAIT_TIMEOUT + 10` minutes polling to prove `fabrik:bot-reprompted` never appears. The bed `.env` files don't set `FABRIK_REVIEW_WAIT_TIMEOUT`, so the harness default (15) applies: a 25-minute wait. The window is also measured from *after* the reinvoke is observed, while its comment says it is timed from the `fabrik:awaiting-review` application — so it over-waits by several minutes on every run.
>
> `FABRIK_REVIEW_WAIT_TIMEOUT` is referenced 23 times across `tests/e2e`; wait-out windows and fixed sleeps cluster in `conjunctive_ci_review_gate_test.go`, `expected_reviewers_test.go`, `ci_fix_reinvoke_test.go`, `mid_stage_label_test.go`, `review_authority_test.go`.
>
> ## Requirements
>
> 1. **Audit every live test's wall-clock** into: real work (Claude workers, CI runs), engine timeouts waited out on purpose, negative-observation windows, and harness slack/margins. `gate report` (#1992) already totals per-test elapsed per cell from the archive; extend or reuse it rather than building another tool.
> 2. **Bed timeouts sized for the bed, not production.** The engine timeouts a test waits out (`FABRIK_REVIEW_WAIT_TIMEOUT`, `FABRIK_CI_WAIT_TIMEOUT`, and any other the audit finds) are set explicitly and short in both bed `.env`s (and wherever the gate launches beds), with tests reading the bed's value — never relying on the harness default.
> 3. **Negative windows are anchored correctly and sized to the property.** A "this must not happen" wait starts from the event that arms the bug (e.g. the label application), not from a later observation, and its margin is justified by the poll interval (a small multiple of `-poll`), not a flat +10 minutes.
> 4. **Prefer the poll seam over wall-clock where the subject is a state window** (`HoldPolls`/`TriggerPoll`, ADR-1978): a negative property can often be proven as "N polls ran past the arm point and none acted", which is deterministic and fast. Note the seam requires `exclusive` tests; weigh that against the shared phase's parallelism.
> 5. **Nothing loses coverage.** Each shortened test must still fail against the bug it guards: neutralise the fix (or the relevant seam) and show the shortened test fails, per the existing verify-why-a-test-passed practice.
> 6. Record before/after per-test and per-leg wall-clock in the PR.
>
> ## Scope
>
> In: `tests/e2e`, `tests/gate`, bed `.env` templates/launch env, the registry if classifications change. Out: engine behaviour changes (a test that can only be sped up by changing engine timing semantics gets its own issue); the sim suite.
>
> ## Acceptance
>
> - Audit table committed (doc or PR body) classifying the top ~20 tests by where their time goes.
> - Bed review/CI wait timeouts set explicitly and short; the expected-reviewers negative window anchored at the label application with a poll-derived margin.
> - Each changed test shown non-vacuous by neutralisation.
> - Measured per-leg wall-clock reduction on a gate run.

## Background

The live e2e suite exists to prove that Fabrik's state machine and its concurrent state-change drivers (poll, webhooks, workers, merge train, settle scans) behave correctly against real GitHub, real Claude and real timing. That needs real timing, but rarely real *minutes* of it. Measured from the 0.0.88 and 0.0.89 gate logs, the summed mean pass time of the 51 measured live tests is about 8 hours per leg, and a full-matrix release runs four legs. The eight slowest tests each take 17 to 33 minutes on average.

Much of that time is idle wall-clock that produces no evidence:

- **Production-sized engine timeouts waited out in a test bed.** The bed `.env` files do not set the review or CI wait timeouts, so the harness default (15 minutes for review) applies.
- **Negative-observation windows with flat margins.** `TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` spends about 2 minutes on real activity and then waits `review timeout + 10` minutes (25 minutes at the default) to prove that `fabrik:bot-reprompted` never appears.
- **Mis-anchored deadlines.** That same window is measured from after the review-reinvoke is observed, although its comment says it is timed from the `fabrik:awaiting-review` application. It therefore over-waits by several minutes on every run.
- **Scattered fixed sleeps and wait-outs.** They cluster in the conjunctive CI/review gate, expected-reviewers, CI-fix reinvoke, mid-stage label and review-authority tests.

This is not a proposal to move coverage to the sim bed. The sim is a pre-gate, never a replacement (ADR-1454), and every live scenario stays live. The target is only the wall-clock that buys no evidence.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Know where the live suite's time goes (Priority: P1)

A release engineer wants a committed, evidence-backed breakdown of where each slow live test spends its wall-clock, so that shortening work targets real waste rather than guesses.

**Why this priority**: Every other change depends on knowing which time is real work and which is idle. It also provides the "before" numbers for the final comparison.

**Independent Test**: Read the audit table for the ~20 slowest tests and check each row's per-category split against the test's archived timing and its source; the categories must sum to the measured elapsed time within a stated tolerance.

**Acceptance Scenarios**:

1. **Given** the archived gate runs for recent releases, **When** the audit is produced, **Then** each of the ~20 slowest live tests is classified into real work, deliberate engine-timeout wait, negative-observation window, and harness slack/margin.
2. **Given** `gate report` already totals per-test elapsed per cell, **When** the audit is produced, **Then** it reuses or extends that report rather than introducing a separate tool.

---

### User Story 2 - Bed timeouts sized for the bed (Priority: P1)

A release engineer wants the engine timeouts that tests deliberately wait out to be short, explicit bed settings that the tests read, so a timeout-driven scenario takes minutes instead of tens of minutes.

**Why this priority**: This removes the largest single source of idle time and is a prerequisite for sizing the negative windows.

**Independent Test**: Start a bed through each launch path and confirm the effective review and CI wait timeouts equal the explicit short values. Confirm that a test which waits out the timeout takes its duration from the bed's value, not from a harness default.

**Acceptance Scenarios**:

1. **Given** both bed `.env` templates and every gate or harness launch path, **When** a bed starts, **Then** the review wait timeout and CI wait timeout are set explicitly to short bed-appropriate values.
2. **Given** a test that waits out an engine timeout, **When** it computes its wait, **Then** it reads the value the bed actually runs with and does not fall back to a harness default.
3. **Given** the audit finds another engine timeout that tests wait out, **When** the bed is configured, **Then** that timeout is also set explicitly.

---

### User Story 3 - Correctly anchored, properly sized negative windows (Priority: P1)

A test author wants every "this must not happen" wait to start at the event that arms the bug and to end after a margin justified by the poll interval, so the window is as short as the property allows and still proves it.

**Why this priority**: These windows are the other major source of idle time. A mis-anchored or flatly padded window both wastes time and weakens the evidence.

**Independent Test**: For the expected-reviewers negative test, check that the window starts at the `fabrik:awaiting-review` application and that its margin is a stated small multiple of the poll interval, with no flat +10 minutes.

**Acceptance Scenarios**:

1. **Given** the review-authority negative test, **When** it observes that `fabrik:bot-reprompted` never appears, **Then** the window is anchored at the `fabrik:awaiting-review` application and ends after the bed's review timeout plus a poll-derived margin.
2. **Given** any other negative-observation window found by the audit, **When** it is revised, **Then** it is anchored at its arming event and its margin is justified in the test by the poll interval.

---

### User Story 4 - Prove negative properties by polls rather than minutes where the seam fits (Priority: P2)

A test author wants negative properties about a state window proven as "N polls ran past the arm point and none acted" using the poll hold/trigger seam, which is deterministic and fast.

**Why this priority**: It can remove most of a negative window's duration, but it requires `exclusive` tests, which cost parallelism in the shared phase. It is therefore beneficial only case by case.

**Independent Test**: For a candidate test converted to the seam, confirm it drives a counted number of polls past the arm point, asserts nothing acted, and is classified `exclusive` in the registry. Confirm that the leg's measured wall-clock does not get worse overall.

**Acceptance Scenarios**:

1. **Given** a negative property whose subject is a state window, **When** the audit evaluates the poll seam, **Then** the decision (adopt or not) and its parallelism cost are recorded per test.
2. **Given** a test converted to the seam, **When** the registry is updated, **Then** its classification matches the seam's `exclusive` requirement.

---

### User Story 5 - No coverage is lost (Priority: P1)

A reviewer wants proof that every shortened test still fails against the bug it guards.

**Why this priority**: Shortening a negative test can silently make it vacuous. The point of the work is lower cost at equal evidence.

**Independent Test**: For each changed test, neutralise the fix or the relevant seam, run the shortened test, and observe that it fails; the PR records that result.

**Acceptance Scenarios**:

1. **Given** a shortened test, **When** the guarded fix or seam is neutralised, **Then** the shortened test fails.
2. **Given** a test whose shortened window could not detect the regression, **When** this is discovered, **Then** the window is widened until it does, or the change to that test is dropped.

---

### User Story 6 - Measured before/after (Priority: P1)

A maintainer wants per-test and per-leg wall-clock before and after the change from a real gate run, to confirm the savings.

**Why this priority**: The work is justified by wall-clock saved, so it must be measured rather than estimated.

**Independent Test**: Compare the before and after per-test and per-leg figures in the PR against the archived gate runs.

**Acceptance Scenarios**:

1. **Given** a gate run on the changed suite, **When** the results are compared to the baseline, **Then** the PR records before/after elapsed time for each changed test and for each leg.

---

### Edge Cases

- A shortened bed CI timeout is a liveness-stall dwell, not a bound on total CI duration. It must stay long enough that real CI on the bed is not falsely paused as stalled, and tests that rely on real CI progress must still pass.
- A shortened review timeout must still exceed the bed's poll interval by enough that a timeout-driven scenario is deterministic rather than racing the poll.
- A test that waits out a timeout and assumes the harness default (15 minutes) must fail loudly if the bed's value is unreadable, not silently use a default.
- Tests running in the shared parallel phase must not be affected by a timeout set for another test's benefit. Per-bed timeouts apply to all tests on the bed.
- Under multi-bed runs, each bed's explicit timeouts come from its own launch env.
- A test that can only be sped up by changing the engine's timing semantics is not changed here and is filed separately.
- Converting a test to the poll seam makes it `exclusive` and moves it out of the shared phase. This can lengthen the leg even as the test shortens, so the net leg effect is checked.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A committed audit (document or PR body) classifies the wall-clock of the ~20 slowest live tests into real work, engine timeouts waited out on purpose, negative-observation windows, and harness slack/margins.
- **FR-002**: The audit is derived from the existing `gate report` per-test elapsed totals, which are reused or extended rather than replaced by a new tool.
- **FR-003**: The engine review wait timeout and CI wait timeout are set explicitly, and short relative to production defaults, in both bed `.env` templates and in every place the gate or harness launches a bed.
- **FR-004**: Any other engine timeout the audit finds that tests wait out on purpose is likewise set explicitly and short at the same places.
- **FR-005**: Tests that wait out an engine timeout derive their wait from the value the bed runs with and never rely on a harness default.
- **FR-006**: Every negative-observation window is anchored at the event that arms the bug and not at a later observation.
- **FR-007**: Every negative-observation window's margin is expressed as a small multiple of the bed's poll interval, with the justification stated in the test, replacing flat margins such as +10 minutes.
- **FR-008**: The expected-reviewers negative window (`TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation`) is anchored at the `fabrik:awaiting-review` application and uses a poll-derived margin.
- **FR-009**: For each negative property whose subject is a state window, the poll hold/trigger seam is evaluated, and used where it proves the property deterministically in fewer polls. Each test using it is classified `exclusive` in the registry.
- **FR-010**: Registry classifications are updated wherever a test's isolation or sensitivity classes change.
- **FR-011**: Each changed test is shown to fail when the fix or seam it guards is neutralised, and the result is recorded in the PR.
- **FR-012**: The PR records before/after elapsed time per changed test and per leg, from a measured gate run.
- **FR-013**: No change alters engine behaviour, and no live scenario is removed or moved to the sim suite.

### Key Entities

- **Bed timeout**: An engine wait setting (review wait, CI wait, others found by the audit) that a live test may wait out deliberately. It is configured per bed through `.env` and launch env.
- **Negative-observation window**: The period a test polls to show an event does not occur. It has an arming event (start) and a margin (end).
- **Audit table**: The classification of a test's elapsed time by category.
- **Poll seam**: The test-only hold/trigger facility (ADR-1978) that lets a test count polls instead of waiting wall-clock.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An audit table covering at least the ~20 slowest live tests is committed, with every row's time split across the four categories.
- **SC-002**: Starting a bed through each launch path yields explicit short review and CI wait timeouts, with no reliance on the harness default.
- **SC-003**: The expected-reviewers negative window starts at the label application, and its margin is documented as a multiple of the poll interval.
- **SC-004**: Every changed test has a recorded neutralisation run in which it fails.
- **SC-005**: A gate run on the changed suite shows a lower summed per-leg wall-clock than the ~8-hour baseline, with before/after figures for each changed test and each leg in the PR.
- **SC-006**: The per-test PASS coverage the gate requires (required live-test × leg pairs) is unchanged: no live test is dropped, skipped or downgraded to inconclusive by this change.

## Assumptions

- No numeric target for the per-leg reduction is fixed; the requirement is that it is measured, non-trivial and reported. The audit decides which tests are worth changing.
- "Short" bed timeouts are chosen from the audit and the bed's poll interval, not from a preset number. The chosen values and the reasoning are recorded with the change.
- The same explicit timeouts apply to both bed `.env` templates and all gate launch paths. No test needs a different engine timeout from its bed's.
- Real-work time (Claude workers, CI runs, merge-train CI) is not reduced by this issue; it is classified and left as it is.
- Tests that cannot be shortened without altering engine timing semantics are recorded in the audit as candidates for separate issues.
- The poll seam is adopted only where the audit shows a net benefit to the leg after accounting for `exclusive` scheduling.

## Out of Scope

- Engine behaviour or timing-semantics changes. A test that can only be sped up that way gets its own issue.
- Moving live coverage to the sim bed or removing live scenarios (ADR-1454).
- Changes to the sim suite.
- Reducing real-work time such as Claude worker runtime or CI duration.

## Source References

- ADR-1454 (sim is a pre-gate, never a replacement for the live suite)
- ADR-1978 (poll hold/trigger seam) and `docs/state-machine.md` §7.8a
- #1992 (`gate report` per-test timing)
- `tests/e2e/expected_reviewers_test.go`, `conjunctive_ci_review_gate_test.go`, `ci_fix_reinvoke_test.go`, `mid_stage_label_test.go`, `review_authority_test.go`
- Gate logs for 0.0.88 and 0.0.89
