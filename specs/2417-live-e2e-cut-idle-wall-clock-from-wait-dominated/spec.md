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

The work is delivered by a pipeline worker, which has no bed and cannot run the multi-hour live gate. The PR therefore carries the audit (with before-figures from the archived gate logs), a written argument for each shortened test, and projected after-figures. Measured after-figures and live neutralisation runs come from the next release gate, which the maintainer runs. The bed `.env` files (the operator's `~/dev/fabrik-test/.env` and `~/dev/fabrik-test-2/.env`) are operator files outside this repository, so the short timeouts are set where the repository launches a bed.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Know where the live suite's time goes (Priority: P1)

A release engineer wants a committed, evidence-backed breakdown of where each slow live test spends its wall-clock, so that shortening work targets real waste rather than guesses.

**Why this priority**: Every other change depends on knowing which time is real work and which is idle. It also provides the "before" numbers for the final comparison.

**Independent Test**: Read the audit table for the ~20 slowest tests and check each row's per-category split against the test's archived timing and its source; the categories must sum to the measured elapsed time within a stated tolerance.

**Acceptance Scenarios**:

1. **Given** the archived gate runs for recent releases, **When** the audit is produced, **Then** each of the ~20 slowest live tests is classified into real work, deliberate engine-timeout wait, negative-observation window, and harness slack/margin.
2. **Given** `gate report` already totals per-test elapsed per cell, **When** the audit is produced, **Then** it reuses or extends that report rather than introducing a separate tool.

---

### User Story 2 - Bed timeouts sized for the bed, set where the bed is launched (Priority: P1)

A release engineer wants the engine timeouts that tests deliberately wait out to be short, explicit settings applied by the repository's own bed launch paths and read by the tests from one shared definition, so a bed and its tests can never disagree and a timeout-driven scenario takes minutes instead of tens of minutes.

**Why this priority**: This removes the largest single source of idle time and is a prerequisite for sizing the negative windows.

**Independent Test**: Launch a bed through each launch path and confirm the effective review and CI wait timeouts equal the shared definition's values, including when the operator's bed `.env` sets a different value. Confirm that a test which waits out the timeout takes its duration from that same definition.

**Acceptance Scenarios**:

1. **Given** the gate's bed launch and the harness's bed launch, **When** a bed starts, **Then** the review wait timeout and CI wait timeout are injected explicitly from one shared definition, alongside the other launch-injected settings.
2. **Given** a test that waits out an engine timeout, **When** it computes its wait, **Then** it reads the value from that shared definition, which is the value the bed was launched with, and does not fall back to a harness default or parse the bed's `.env`.
3. **Given** an operator bed `.env` that sets a different value for one of these timeouts, **When** the bed starts, **Then** the launch value is the one the engine runs with, and the `.env` value does not silently take effect.
4. **Given** the audit finds another engine timeout that tests wait out, **When** the launch settings are defined, **Then** that timeout is also injected from the shared definition.

---

### User Story 3 - Correctly anchored, properly sized negative windows (Priority: P1)

A test author wants every "this must not happen" wait to start at the event that arms the bug and to end after a margin justified by the poll interval, so the window is as short as the property allows and still proves it.

**Why this priority**: These windows are the other major source of idle time. A mis-anchored or flatly padded window both wastes time and weakens the evidence.

**Independent Test**: For the expected-reviewers negative test, check that the window starts at the `fabrik:awaiting-review` application and that its margin is a stated small multiple of the poll interval, with no flat +10 minutes.

**Acceptance Scenarios**:

1. **Given** the review-authority negative test, **When** it observes that `fabrik:bot-reprompted` never appears, **Then** the window is anchored at the `fabrik:awaiting-review` application and ends after the launch-defined review timeout plus a poll-derived margin.
2. **Given** any other negative-observation window found by the audit, **When** it is revised, **Then** it is anchored at its arming event and its margin is justified in the test by the poll interval.

---

### User Story 4 - Prove negative properties by polls rather than minutes where the seam fits (Priority: P2)

A test author wants negative properties about a state window proven as "N polls ran past the arm point and none acted" using the poll hold/trigger seam, which is deterministic and fast.

**Why this priority**: It can remove most of a negative window's duration, but it requires `exclusive` tests, which cost parallelism in the shared phase. It is therefore beneficial only case by case.

**Independent Test**: For a candidate test converted to the seam, confirm it drives a counted number of polls past the arm point, asserts nothing acted, and is classified `exclusive` in the registry. Confirm from the audit's figures that the projected leg wall-clock does not get worse overall.

**Acceptance Scenarios**:

1. **Given** a negative property whose subject is a state window, **When** the audit evaluates the poll seam, **Then** the decision (adopt or not) and its parallelism cost are recorded per test.
2. **Given** a test converted to the seam, **When** the registry is updated, **Then** its classification matches the seam's `exclusive` requirement.

---

### User Story 5 - Shortened tests are shown to still detect their bug (Priority: P1)

A reviewer wants each shortened test to come with evidence that it still catches the bug it guards, even though the worker cannot run it live.

**Why this priority**: Shortening a negative test can silently make it vacuous. The point of the work is lower cost at equal evidence.

**Independent Test**: For each changed test, read the written argument in the PR and check that it names the arming event, the poll interval and the margin, and that the margin covers the number of polls the bug needs in order to show. Where a sim twin or unit test covers the same property, confirm a neutralisation run there fails with the fix removed.

**Acceptance Scenarios**:

1. **Given** a shortened test, **When** the PR is reviewed, **Then** it carries a written argument of why the shortened window still detects the bug, naming the arming event, the poll interval and the margin.
2. **Given** a changed test whose property is also covered by a sim twin or unit test, **When** that covering test is neutralised (fix or seam removed), **Then** the covering test fails, and the result is recorded in the PR.
3. **Given** a test whose shortened window cannot be argued to detect the regression, **When** this is discovered, **Then** the window is widened until it can, or the change to that test is dropped.
4. **Given** the next release gate run by the maintainer, **When** it executes the changed tests, **Then** their live neutralisation runs are done there and recorded as a follow-up to this change, not as acceptance for it.

---

### User Story 6 - Projected before/after, measured at the next gate (Priority: P1)

A maintainer wants before-figures from real archived runs and projected after-figures per changed test and per leg, and then to confirm them against the next release gate.

**Why this priority**: The work is justified by wall-clock saved, so the savings must be quantified up front and checked against a real run.

**Independent Test**: Compare the PR's before-figures with the archived gate logs, and check that each projected after-figure follows from the changed timeout and margin values.

**Acceptance Scenarios**:

1. **Given** the archived gate logs, **When** the PR is prepared, **Then** it records the before-figure per changed test and per leg.
2. **Given** each changed test's new timeout and margin, **When** the PR is prepared, **Then** it records a projected after-figure per changed test and per leg, with the arithmetic that gives it.
3. **Given** the next release gate, **When** the maintainer runs it, **Then** the measured per-test and per-leg figures are recorded as a follow-up to this change, not as acceptance for it.

---

### Edge Cases

- A shortened bed CI timeout is a liveness-stall dwell, not a bound on total CI duration. It must stay long enough that real CI on the bed is not falsely paused as stalled, and tests that rely on real CI progress must still pass.
- A shortened review timeout must still exceed the bed's poll interval by enough that a timeout-driven scenario is deterministic rather than racing the poll.
- A test that waits out a timeout must obtain the value from the shared launch definition. If that value cannot be determined, it fails loudly rather than silently using a default.
- An operator's bed `.env` may set the same timeouts. The launch value wins, so the value tests use and the value the engine runs with are always the same.
- Tests running in the shared parallel phase must not be affected by a timeout set for another test's benefit. The launch timeouts apply to all tests on the bed.
- Under multi-bed runs, every bed is launched through the same shared definition, so all beds and their tests agree.
- A test that can only be sped up by changing the engine's timing semantics is not changed here and is filed separately.
- Converting a test to the poll seam makes it `exclusive` and moves it out of the shared phase. This can lengthen the leg even as the test shortens, so the projected net leg effect is checked.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A committed audit (document or PR body) classifies the wall-clock of the ~20 slowest live tests into real work, engine timeouts waited out on purpose, negative-observation windows, and harness slack/margins.
- **FR-002**: The audit is derived from the existing `gate report` per-test elapsed totals, which are reused or extended rather than replaced by a new tool.
- **FR-003**: The engine review wait timeout and CI wait timeout are injected explicitly, and short relative to production defaults, by the gate's bed launch (`tests/gate/bed.go`) and the harness's bed launch (`tests/e2e/lifecycle.go`), in the same way other launch settings are injected. The operator-owned bed `.env` files are not modified.
- **FR-004**: Any other engine timeout the audit finds that tests wait out on purpose is likewise injected explicitly and short at the same launch points.
- **FR-005**: The injected timeout values come from one shared definition. Tests that wait out an engine timeout read the value from that definition, never from a harness default or from the bed's `.env`, so a bed and its tests cannot disagree.
- **FR-006**: Every negative-observation window is anchored at the event that arms the bug and not at a later observation.
- **FR-007**: Every negative-observation window's margin is expressed as a small multiple of the bed's poll interval, with the justification stated in the test, replacing flat margins such as +10 minutes.
- **FR-008**: The expected-reviewers negative window (`TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation`) is anchored at the `fabrik:awaiting-review` application and uses a poll-derived margin.
- **FR-009**: For each negative property whose subject is a state window, the poll hold/trigger seam is evaluated, and used where it proves the property deterministically in fewer polls. Each test using it is classified `exclusive` in the registry.
- **FR-010**: Registry classifications are updated wherever a test's isolation or sensitivity classes change.
- **FR-011**: For each changed test, the PR carries a written argument of why the shortened window still detects the bug it guards, naming the arming event, the poll interval and the margin. Where a sim twin or unit test covers the same property, a neutralisation run (fix or seam removed, covering test fails) is recorded in the PR. Live neutralisation runs are done by the maintainer at the next release gate and are recorded as a follow-up, not as acceptance for this issue.
- **FR-012**: The PR records, per changed test and per leg, the before-figure from the archived gate logs and a projected after-figure with its arithmetic. Measured after-figures from the next release gate are recorded as a follow-up, not as acceptance for this issue.
- **FR-013**: No change alters engine behaviour, and no live scenario is removed or moved to the sim suite.
- **FR-014**: When an operator's bed `.env` sets one of the injected timeouts, the launch value wins and the `.env` value does not silently take effect.

### Key Entities

- **Bed timeout**: An engine wait setting (review wait, CI wait, others found by the audit) that a live test may wait out deliberately. It is injected at bed launch from a shared definition that the tests also read.
- **Negative-observation window**: The period a test polls to show an event does not occur. It has an arming event (start) and a margin (end).
- **Audit table**: The classification of a test's elapsed time by category.
- **Poll seam**: The test-only hold/trigger facility (ADR-1978) that lets a test count polls instead of waiting wall-clock.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An audit table covering at least the ~20 slowest live tests is committed, with every row's time split across the four categories.
- **SC-002**: Launching a bed through each repository launch path yields explicit short review and CI wait timeouts from the shared definition, including when the operator's bed `.env` sets different values, and tests read those same values.
- **SC-003**: The expected-reviewers negative window starts at the label application, and its margin is documented as a multiple of the poll interval.
- **SC-004**: Every changed test has a written argument in the PR naming its arming event, poll interval and margin, and every changed test covered by a sim twin or unit test has a recorded neutralisation run in which the covering test fails.
- **SC-005**: The PR records before-figures from the archived gate logs and projected after-figures per changed test and per leg, and the projected summed per-leg wall-clock is lower than the ~8-hour baseline.
- **SC-006**: The per-test PASS coverage the gate requires (required live-test × leg pairs) is unchanged: no live test is dropped, skipped or downgraded to inconclusive by this change.

## Assumptions

- No numeric target for the per-leg reduction is fixed; the requirement is that the projection is stated, non-trivial and later measured. The audit decides which tests are worth changing.
- "Short" bed timeouts are chosen from the audit and the bed's poll interval, not from a preset number. The chosen values and the reasoning are recorded with the change.
- The launch value wins over an operator's `.env` value (rather than a mismatch failing preflight), because the process environment set at launch already takes precedence over `.env` loading. This also keeps a bed and its tests in agreement without an extra preflight failure mode.
- The same injected timeouts apply to all beds and all launch paths. No test needs a different engine timeout from its bed's.
- Real-work time (Claude workers, CI runs, merge-train CI) is not reduced by this issue; it is classified and left as it is.
- Tests that cannot be shortened without altering engine timing semantics are recorded in the audit as candidates for separate issues.
- The poll seam is adopted only where the audit shows a net projected benefit to the leg after accounting for `exclusive` scheduling.
- A pipeline worker has no bed and cannot run the live gate, so measured figures and live neutralisation runs are produced by the maintainer at the next release gate.

## Out of Scope

- Engine behaviour or timing-semantics changes. A test that can only be sped up that way gets its own issue.
- Moving live coverage to the sim bed or removing live scenarios (ADR-1454).
- Changes to the sim suite.
- Reducing real-work time such as Claude worker runtime or CI duration.
- Editing the operator-owned bed `.env` files (`~/dev/fabrik-test/.env`, `~/dev/fabrik-test-2/.env`), which live outside this repository.
- Running the live gate, producing measured before/after figures, and running live neutralisation. These are the maintainer's follow-up at the next release gate.

## Source References

- ADR-1454 (sim is a pre-gate, never a replacement for the live suite)
- ADR-1978 (poll hold/trigger seam) and `docs/state-machine.md` §7.8a
- #1992 (`gate report` per-test timing)
- `tests/gate/bed.go`, `tests/e2e/lifecycle.go` (bed launch sites, where launch-injected settings such as `pollctl.Env` and `FABRIK_MAX_TRAIN_AUTO_REPAIR_ATTEMPTS=0` are set)
- `tests/e2e/expected_reviewers_test.go`, `conjunctive_ci_review_gate_test.go`, `ci_fix_reinvoke_test.go`, `mid_stage_label_test.go`, `review_authority_test.go`
- Gate logs for 0.0.88 and 0.0.89
