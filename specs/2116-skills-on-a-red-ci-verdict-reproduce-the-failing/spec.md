# Feature Specification: Targeted reproduction on a red CI verdict; tell the worker what failed

**Feature Branch**: `fabrik/issue-2116`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "skills: on a red CI verdict reproduce the failing checks locally, not the full suite; tell the worker what failed

## Problem

The test-running skills re-run the whole test suite locally whenever CI is anything but green for the current head, even when CI has already run that suite and only one unrelated check failed. With several workers in parallel, those local `-race` runs saturate the host and produce failures CI doesn't. The worker then chases or reports those failures.

**Observed 2026-10-11 on the dev daemon (#2096, Validate CI-fix worker):**
- On head `5546abb3`, CI's `Test and vet` (the full `go test -race ./...`) passed. The one red check was `Test in a worker-shaped environment`, failing a single test, `TestMultiBedRunEndToEnd` in `tests/gate`, in 0.01s.
- The worker ran the whole sim suite locally (`/tmp/sim2096.out`) and then spent several minutes in a `sleep 10` loop waiting for it.
- That run failed four sim tests that pass in CI, including `TestMergeTrainTwoBasesConcurrent`, `TestMergeTrainEjection_MaxEjectionsPausesMember` and a `git rev-parse … signal: segmentation fault` under the race detector. These are host-load symptoms. At the same moment #2110's worker was running the full `go test -race -timeout 40m ./...`, #2112's was running `go test -race ./internal/... ./cmd/... ./tests/sim/...`, and an orphaned `sim.test` from #2097 was still running.
- The whole time, that worker held one of the five slots. #2114 shows what a full pool does to the poll loop.

The cause is the rule added by #1997 ("Skipping a redundant full-suite run", in `fabrik-implement`, `fabrik-review`, `fabrik-validate` and their `-comment` variants). It licenses a skip only on `verdict: green`. Any other verdict (`red`, `pending`, `none`, `unknown`) sends the worker back to running the full suite. The comment variants also say that after a code change the worker must run the full suite and "not fall back to targeted tests only", on the grounds that a push from comment processing has no CI gate behind it. That is not true of every comment path: a CI-fix reinvoke runs while the item holds `fabrik:awaiting-ci`, and the gate re-evaluates the new head.

## Requirements

**R1. A red verdict means targeted, not full.** When `ci-status.md` reports `red`, the worker reproduces only what CI says failed: the failing check's specific tests or the smallest unit containing them (a single test, file, module or package, using whatever test runner the project uses), never the full suite as a first step. After a fix, it re-runs those tests plus the tests covering the code it changed. The full-suite verdict is left to CI, except where R3 says no CI gate follows the push.

**R2. Tell the worker what failed.** `ci-status.md` gains, for a `red` verdict, the failing check runs: name, conclusion, details URL and the check run's `output.title`/`output.summary` where present. Plan decides whether the names of the failing tests can be extracted cheaply and safely, without assuming any particular language or test runner: from check-run annotations without needing the optional `actions` permission, or from job logs only when `actions: read` is granted, degrading quietly otherwise. All reads stay live and uncached, like the existing writer (`engine/ci_status.go`).

**R3. Say exactly which paths have a CI gate behind them.** Plan establishes, for each invocation path that runs these skills (stage dispatch; comment review; the CI-fix, review and rebase reinvokes; merge-train auto-repair), whether the engine re-gates the pushed head on CI before it can advance or land. The skill text then says, per path, when a local full-suite run is still required. Replace the comment variants' blanket "treat `ci_gated` as false … do not fall back to targeted tests only" with that.

**R4. Load awareness.** Before a local run broader than the failing tests (the full suite, or several packages or modules), the worker checks host load (`uptime` against the CPU count). A local failure that does not reproduce in isolation and passes in CI on the same head is reported as load-induced, naming the evidence, not treated as a defect to fix. A worker never waits on a local suite run in a `sleep` loop for longer than the skill's timeout.

**R5. Language-neutral skill text.** These skills ship to every Fabrik user, whatever their project's language. The new wording must not name `go test`, `-race`, Go packages or any other toolchain-specific command: describe the action ("run only the failing tests", "the project's test runner"), and if an example helps, give one across several ecosystems or none. While editing these skills, reword the existing Go-specific test lines (for example `go test -race -timeout 5m ./...` in `fabrik-review` and `fabrik-validate`) the same way. Fabrik's own Go specifics belong in this repo's `CLAUDE.md`, not the shipped skills.

**R6. Docs and tests.** Update `docs/stage-lifecycle.md`'s description of `ci-status.md` and regenerate `docs/llms-full.txt` if a canonical page changes. Skill-text tests (as in `plugin/specify_skill_test.go`) pin the R1/R3/R4/R5 wording in all six skills, including a check that none of them names `go test`. Engine tests cover the new `ci-status.md` fields, including a read failure degrading to today's file.

## Scope

- In scope: the six test-running skills, `engine/ci_status.go` and its tests, docs.
- Out of scope: the CI workflows themselves; the orphaned-process reaping (#1805, #1989); the reserved train slot (#2115) and poll dispatch (#2114), which are related but separate.

## Acceptance

- A Validate CI-fix invocation with `verdict: red` and one failing check sees that check's details in `ci-status.md`, and the skill directs a targeted reproduction, not a full-suite run.
- Skill-text tests fail if any of the six skills again licenses a full-suite run as the first response to a `red` verdict, or keeps the blanket comment-path rule.
- No shipped skill text names `go test` or another single toolchain's test command.
- The full test suite passes."

## Background

Since #1997, the test-running skills (`fabrik-implement`, `fabrik-review`, `fabrik-validate` and their `-comment` variants) may skip a redundant local full-suite run only when the engine's CI verdict for the current head is `green`. Any other verdict (`red`, `pending`, `none`, `unknown`) sends the worker back to running the full suite locally. That is wrong for `red` in particular: CI has usually already run the full suite, and often only one unrelated check failed.

With several workers running in parallel, redundant local full-suite runs saturate the host. They produce load-induced failures (timeouts, flaky concurrency tests, even segmentation faults) that CI does not show, and the worker then chases or reports those failures. A worker also holds a concurrency slot the whole time it waits.

Observed on 2026-10-11 on the dev daemon (#2096, Validate CI-fix worker). CI's full-suite check passed on the head. The one red check was a separate check that failed a single fast test. The worker still ran the entire sim suite locally and waited minutes in a sleep loop. That run failed four tests that pass in CI. At the same moment two other workers were running broad suites and an orphaned test process from another issue was still running.

A second defect sits in the `-comment` variants. They say that after a code change the worker must run the full suite and must not fall back to targeted tests only, on the grounds that a push from comment processing has no CI gate behind it. That is not true of every comment path. A CI-fix reinvoke runs while the item holds `fabrik:awaiting-ci`, and the gate re-evaluates the new head.

Skill text ships to every Fabrik user, whatever their project's language, so it must describe actions generically and never name a single toolchain's test command.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Targeted reproduction on a red verdict (Priority: P1)

A worker is invoked (for example Validate fixing CI) with `ci-status.md` reporting `verdict: red`. It reads which check failed and reproduces only that failure locally, fixes it, re-runs the failing tests plus tests covering the changed code, and leaves the full-suite verdict to CI.

**Why this priority**: This is the behaviour that caused the incident. It removes the redundant host load and the false-failure chase.

**Independent Test**: Render each of the six skills and check that, for a `red` verdict, they direct reproduction of the failing check's tests or smallest containing unit and forbid the full suite as a first step.

**Acceptance Scenarios**:

1. **Given** `ci-status.md` reports `red` with one failing check that is not the main test check, **When** the worker follows the skill, **Then** it runs only the tests that check names (or the smallest unit containing them), not the full suite.
2. **Given** the worker has fixed the failure, **When** it verifies, **Then** it re-runs the failing tests plus the tests covering the code it changed, and does not run the full suite where a CI gate follows the push.

---

### User Story 2 - The worker is told what failed (Priority: P1)

For a `red` verdict, `ci-status.md` lists each failing check run with its name, conclusion, details URL and the check run's output title and summary where present. Where it can be done cheaply and safely, the names of the failing tests are included.

**Why this priority**: Targeted reproduction (Story 1) is only practical if the worker knows what failed without hunting for it.

**Independent Test**: Run the engine's `ci-status.md` writer against a stubbed client returning a red head with one failing check run, and assert the file contains that check's details.

**Acceptance Scenarios**:

1. **Given** a head whose verdict is `red` with one failing check run, **When** the engine writes `ci-status.md`, **Then** the file lists that check run's name, conclusion, details URL and output title/summary.
2. **Given** the extra read of failing checks fails (API error, missing permission), **When** the engine writes `ci-status.md`, **Then** the file is the same as it is today and the failure is logged, not fatal.
3. **Given** the verdict is not `red`, **When** the engine writes `ci-status.md`, **Then** no failing-check section is added.

---

### User Story 3 - The skill says which paths have a CI gate (Priority: P2)

Each skill states, per invocation path (stage dispatch; comment review; the CI-fix, review and rebase reinvokes; merge-train auto-repair), whether the engine re-gates the pushed head on CI before it can advance or land. It states when a local full-suite run is still required. The comment variants lose the blanket "treat `ci_gated` as false … do not fall back to targeted tests only" rule.

**Why this priority**: The full-suite escape hatch must stay for the paths that truly have no CI gate behind them, and must go for those that do.

**Independent Test**: Check that all six skills carry the per-path statement, and that none of the comment variants still contains the blanket rule.

**Acceptance Scenarios**:

1. **Given** a CI-fix reinvoke (item holds `fabrik:awaiting-ci`), **When** the worker pushes a fix, **Then** the skill tells it that CI re-gates the new head and a full local suite is not required.
2. **Given** a path with no CI gate behind the push, **When** the worker changes code, **Then** the skill still requires a local run broad enough to stand in for the missing gate.

---

### User Story 4 - Load awareness (Priority: P2)

Before any local run broader than the failing tests, the worker checks host load against the CPU count. A local failure that does not reproduce in isolation and that passes in CI on the same head is reported as load-induced, with the evidence named, and is not treated as a defect. The worker never polls a running suite in a sleep loop for longer than the skill's timeout.

**Why this priority**: It limits the damage on the paths where a broad local run is still needed.

**Independent Test**: Check the six skills for the load check, the load-induced reporting rule and the bounded-wait rule.

**Acceptance Scenarios**:

1. **Given** the host load is high relative to CPU count, **When** the worker must run a broad local suite, **Then** the skill directs it to check load first and account for it when interpreting failures.
2. **Given** a local failure that passes in isolation and passes in CI on the same head, **When** the worker reports, **Then** it states the failure as load-induced with that evidence.

---

### User Story 5 - Language-neutral skills (Priority: P1)

No shipped test-running skill names `go test`, `-race`, Go packages or any other single toolchain's test command. Existing Go-specific test lines in those skills (for example in `fabrik-review` and `fabrik-validate`) are reworded the same way. Fabrik's own Go specifics live in this repository's `CLAUDE.md`.

**Why this priority**: The skills ship to every Fabrik user in any language.

**Independent Test**: A skill-text test fails if any of the six skills contains `go test`.

**Acceptance Scenarios**:

1. **Given** the six skills, **When** the skill-text test scans them, **Then** none contains `go test` or `-race`.

---

### Edge Cases

- `ci-status.md` reports `pending`, `none` or `unknown`: the requirement targets `red` only. Wording for the other verdicts must not regress to licensing a full-suite run as an unconditional first step, and the change must keep the existing `green` skip rule intact.
- More than one failing check, or a failing check with no output title/summary: list every failing check; omit absent fields.
- A very large number of failing checks or very long summaries: the file stays bounded and says it has been truncated.
- A failing check that does not map to specific tests (lint, build, format): the worker reproduces that check's own command or the smallest unit it covers, not the full test suite.
- The failing-check read fails or the needed permission is absent: `ci-status.md` degrades quietly to today's content.
- Test names cannot be extracted from annotations or logs: the check-level details are still given and the worker narrows from them.
- A fix touches code beyond the failing tests: "tests covering the changed code" is run in addition, still without the full suite where a CI gate follows.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When `ci-status.md` reports `red`, the six test-running skills (`fabrik-implement`, `fabrik-review`, `fabrik-validate` and their `-comment` variants) direct the worker to reproduce only what CI reports as failed: the failing check's specific tests or the smallest unit containing them, using the project's own test runner. They must not license a full-suite run as the first response.
- **FR-002**: After a fix, the skills direct the worker to re-run the failing tests plus the tests covering the code it changed, and to leave the full-suite verdict to CI wherever a CI gate follows the push (see FR-005).
- **FR-003**: For a `red` verdict, the engine adds to `ci-status.md` each failing check run's name, conclusion, details URL and `output.title`/`output.summary` where present.
- **FR-004**: The engine's reads for FR-003 are live and uncached, like the existing writer. A failure of any such read degrades the file to today's content and is logged, never fatal and never blocking the invocation. Whether failing test names are also extracted (from check-run annotations without the `actions` permission, or from job logs only when `actions: read` is granted) is decided in Plan; any extraction must not assume a language or test runner and must degrade quietly when unavailable.
- **FR-005**: Plan establishes, for each invocation path that runs these skills (stage dispatch; comment review; CI-fix, review and rebase reinvokes; merge-train auto-repair), whether the engine re-gates the pushed head on CI before it can advance or land. The skills state this per path and say when a local full-suite run is still required.
- **FR-006**: The comment variants' blanket rule (treat `ci_gated` as false and do not fall back to targeted tests only) is removed and replaced by the per-path statement from FR-005.
- **FR-007**: Before a local run broader than the failing tests, the skills direct the worker to check host load against CPU count.
- **FR-008**: The skills direct the worker to report a local failure that does not reproduce in isolation and passes in CI on the same head as load-induced, naming the evidence, and not to treat it as a defect to fix.
- **FR-009**: The skills forbid waiting on a local suite run in a sleep loop for longer than the skill's timeout.
- **FR-010**: No shipped test-running skill names `go test`, `-race`, Go packages or another single toolchain's test command. Existing Go-specific test lines in those skills are reworded to describe the action ("the project's test runner"); an example, if given, spans several ecosystems or none.
- **FR-011**: `docs/stage-lifecycle.md`'s description of `ci-status.md` is updated, and `docs/llms-full.txt` is regenerated in the same change when a canonical page changes.
- **FR-012**: Skill-text tests pin the FR-001, FR-005/FR-006, FR-007–FR-009 and FR-010 wording in all six skills, and fail if any skill again licenses a full-suite run as the first response to `red`, keeps the blanket comment-path rule, or names `go test`.
- **FR-013**: Engine tests cover the new `ci-status.md` fields for a red head, the no-extra-section case for other verdicts, and a read failure degrading to today's file.
- **FR-014**: Fabrik's own Go-specific test commands, where needed for contributors, are documented in this repository's `CLAUDE.md`, not in shipped skills.

### Key Entities *(if applicable)*

- **`ci-status.md`**: The context file the engine writes into the worktree when the item has a linked open PR: head SHA, the green-and-complete CI verdict (`green`/`red`/`pending`/`none`/`unknown`) and `ci_gated`. It gains a failing-check section for `red`.
- **Failing check run**: A check run on the head whose conclusion is a failure, with name, conclusion, details URL and optional output title/summary.
- **Invocation path**: One of the ways the engine runs these skills: stage dispatch, comment review, CI-fix reinvoke, review reinvoke, rebase reinvoke, merge-train auto-repair. Each either has a CI gate re-evaluating the pushed head or does not.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a Validate CI-fix invocation with `verdict: red` and one failing check, `ci-status.md` contains that check's name, conclusion, details URL and output summary, and the skill directs a targeted reproduction rather than a full-suite run.
- **SC-002**: A skill-text test fails if any of the six skills licenses a full-suite run as the first response to a `red` verdict or retains the blanket comment-path rule.
- **SC-003**: A skill-text test confirms none of the six skills names `go test` or `-race`.
- **SC-004**: When the failing-check read fails, `ci-status.md` is byte-identical to today's output for the same inputs.
- **SC-005**: Each of the six skills states, per invocation path, whether a CI gate follows the push and when a local full-suite run is still required.
- **SC-006**: The full test suite passes.

## Assumptions

- The change targets `red` only. `pending`, `none` and `unknown` keep their current behaviour, apart from the language-neutral rewording, unless Plan finds one of them licenses a full-suite run in a way that contradicts FR-001.
- The `green` skip rule from #1997 stays as is.
- Whether to extract failing test names (annotations, or logs when `actions: read` is held) is left to Plan, as the issue states. The minimum deliverable is check-level detail (name, conclusion, details URL, output title/summary).
- The check-run read needs no permission beyond what the engine already holds for check runs.
- "The skill's timeout" in FR-009 refers to the timeout each skill already specifies for local test runs, or one the skill states after rewording.
- The six skills are the full set of test-running skills affected.

## Out of Scope *(optional)*

- The CI workflows themselves.
- Orphaned-process reaping (#1805, #1989).
- The reserved train slot (#2115) and poll dispatch (#2114).
- Reducing worker concurrency or changing slot accounting.
- Changing the `green` verdict classification itself.

## Source References *(optional)*

- #1997 ("Skipping a redundant full-suite run"), the rule being amended.
- `engine/ci_status.go`, the existing `ci-status.md` writer.
- `plugin/specify_skill_test.go`, the model for skill-text tests.
- `docs/stage-lifecycle.md`, the `ci-status.md` description.
- Incident: #2096 Validate CI-fix worker, 2026-10-11.
