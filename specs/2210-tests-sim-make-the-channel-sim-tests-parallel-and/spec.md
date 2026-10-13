# Feature Specification: Parallel, drain-driven channel sim tests (restore the tests/sim CI margin)

**Feature Branch**: `fabrik/issue-2210`
**Created**: 2026-10-13
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `tests/sim` is at the CI per-package timeout again, three days after #2069 doubled it. PR #2209 (#2122) timed out at 600 s in both CI test jobs on all three attempts, with no failing test.
>
> `tests/sim` time in CI (Test and vet job):
>
> | Date (UTC) | Run | Duration | Note |
> |---|---|---|---|
> | 2026-10-09 | — | 270–300 s | #2069 raised the cap from 5m to 10m |
> | 10-11 23:56 | #2180 | 362 s | |
> | 10-12 01:32 | #2120 | 414 s | |
> | 10-12 01:49 | #2170 | 451 s | |
> | 10-12 03:06 | #2188 | 481 s | |
> | 10-12 04:42 | #2169 | 551 s | |
> | 10-12 06:26 | #2121 | 562 s | |
> | 10-12 07:13 | #2171 | 579 s | |
> | 10-12 | #2141 | 600.2 s | green run at the edge |
>
> **The existing tests did not slow down.** I ran eight common sim tests with `-race -p 1 -parallel 1` on `7299adf1` (before this growth) and on `c832f6ca` (main now). Every one was within noise, for example `TestSmoke_FullPipelineToDone` at 23.0 → 22.6 s and `TestCruiseFullPipeline` at 22.4 → 22.4 s. The per-poll cost is unchanged.
>
> **The growth is new tests that run serially.** 34 sim tests were added since `7299adf1`, totalling 266 s of test time locally. Most are the MCP channel tests: `channel_resettle_cause_test.go`, `channel_situation_test.go`, `channel_anomaly_test.go`, `channel_decision_test.go`, `channel_default_family_test.go` and others. The slowest:
>
> | Test | Time |
> |---|---|
> | `TestChannelReSettleCauseRevalidated` | 27.7 s |
> | `TestSimSettlementWithdrawnWhenTheBaseMoves` | 26.6 s |
> | `TestChannelReSettleCauseAutonomyChangedByHuman` | 20.7 s |
> | `TestChannelReSettleCauseNonVacuous` | 20.5 s |
> | `TestSituationDigest_TwoQuietTicksPublishOneAChangePublishesASecond` | 18.5 s |
> | `TestSituationDigest_NoCadenceMeansNoScheduledDigest` | 17.2 s |
> | `TestSituationCatchUp_IsTheDigest` | 17.0 s |
> | `TestItemWedged_FeedbackHoldWithNoDispatch` | 16.3 s |
> | `TestChannelDefaultFamilyExcludesLabelEvents` | 15.8 s |
> | `TestRetryStarved_WallTimeKillWaitingForASlot` | 14.9 s |
>
> There are two compounding causes:
>
> 1. **None of the channel sim test files call `t.Parallel()`.** 73 of 88 sim test files do. The channel tests cannot simply be marked parallel, because their seams are process-global:
>    - `engine.SetChannelTimingForTest`;
>    - the `Set*DisabledForTest` neutralisation toggles in `engine/channel_seam.go`, such as `SetSituationDedupDisabledForTest` and `SetSettleCauseDisabledForTest`.
>
>    A non-vacuity test that disables a fix globally would corrupt a parallel positive test.
> 2. **`settleWait()` is a real `time.Sleep(150 * time.Millisecond)`** (`tests/sim/channel_events_test.go`). It is called inside `AdvanceUntil` conditions, 68 call sites across 9 files, so a wait of up to 80 polls can spend up to 12 s sleeping.
>
> ## Requirements
>
> - **R1 — Engine-scoped channel seams.** Move the channel test seams in `engine/channel_seam.go` and the related `Set*ForTest` toggles from process-global variables to per-`Engine` (or per-hub) settings, so two engines in one test binary cannot see each other's neutralisations or timing. Keep each seam's behaviour and its existing non-vacuity tests. Every neutralised test must still fail under its neutralisation.
> - **R2 — Parallel channel sim tests.** With R1 in place, mark the channel, situation, anomaly, decision and default-family sim tests `t.Parallel()`, matching the rest of the package. A test that still needs a process-global must say why in a comment.
> - **R3 — No sleep-bound waits.** Replace `settleWait()` with a deterministic wait on the channel consumer having drained what the poll enqueued (for example a test seam that blocks until the consumer has processed the queue up to the current change sequence, with a bounded real-time cap). Keep a short real-time fallback only where no drain point exists, and document it.
> - **R4 — Restore the margin.** With R1–R3, CI's `tests/sim` duration on `main` returns to at most 420 s, measured on the PR's own CI runs and quoted in the PR description.
> - **R5 — Guard.** Add a cheap guard so a new sim test file without `t.Parallel()` fails `go test ./tests/sim`, unless the file is on an explicit, reasoned exemption list. Model it on the existing static scans in the repo.
>
> ## Scope
>
> In scope:
> - `tests/sim` channel and overseer tests;
> - `engine/channel_seam.go` and the seams it exposes.
>
> Out of scope:
> - sharding the sim package (ADR-1857);
> - per-PR test selection;
> - the CI timeout itself (a separate stopgap PR handles that).
>
> ## Acceptance
>
> - Every non-vacuity test in the touched files still fails under its neutralisation. Each neutralisation is shown once in the PR.
> - `go test -race ./tests/sim` passes, with the channel tests running in parallel.
> - `tests/sim` in CI on the PR is at most 420 s.
> - The R5 guard fails on a fixture file without `t.Parallel()`.
> - `go test -race ./...` is clean.
>
> Mentioned for context: #2122 (the PR that first tipped over) and #1966. Mention #1966 by bare number only. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.

## Background

The `tests/sim` package is the fast, free pre-gate of the test pyramid and runs on every PR under a per-package CI timeout. That timeout was doubled from 5 to 10 minutes by #2069 on 2026-10-09; three days later the package's CI time rose from about 300 s to about 600 s, and PR #2209 (for #2122) timed out on all three attempts with no failing test. Maintainers can no longer tell a real hang from a slow-but-healthy run, and every further sim test added to the package will tip it over.

The cause is not slower existing tests: eight representative tests measured the same before and after the growth. It is new tests, 34 of them (about 266 s locally), mostly MCP channel tests, which all run serially for two compounding reasons:

1. None of the channel sim test files mark their tests parallel (73 of 88 sim test files do). They cannot simply be marked parallel, because their test seams (the channel timing setter and the `Set*DisabledForTest` neutralisation toggles) are process-global, so a non-vacuity test that disables a fix would corrupt a parallel positive test.
2. The shared `settleWait()` helper is a real 150 ms sleep inside `AdvanceUntil` conditions (68 call sites across 9 files), so a single wait of up to 80 polls can burn up to 12 s asleep.

The goal is to make the channel tests safe to run in parallel and event-driven rather than sleep-driven, restore a CI margin of at least roughly 180 s under the cap, and keep the problem from silently recurring. Related: #2122 (the PR that first tipped over) and #1966 (the channel feature family these tests cover).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Channel seams are scoped to one engine (Priority: P1)

A developer writing or running channel sim tests can neutralise a fix or change channel timing in one test without any other test in the same binary being affected.

**Why this priority**: This is the prerequisite for parallelism. Without isolation, marking tests parallel makes non-vacuity tests corrupt positive tests.

**Independent Test**: Run two engines in one test binary concurrently, with a neutralisation toggled on only one; the other must behave as if it were not neutralised, and timing changes on one must not alter the other.

**Acceptance Scenarios**:

1. **Given** two engines in one test process, **When** a neutralisation toggle is set on engine A, **Then** engine B's behaviour is unchanged.
2. **Given** two engines with different channel timing configured, **When** both run, **Then** each uses only its own timing.
3. **Given** an existing non-vacuity test, **When** it neutralises its fix on its own engine, **Then** the test still fails, proving it is not vacuous.

---

### User Story 2 - Channel sim tests run in parallel (Priority: P1)

A maintainer running `go test -race ./tests/sim` (locally or in CI) sees the channel, situation, anomaly, decision and default-family tests run concurrently with each other and with the rest of the package, as the other sim tests already do.

**Why this priority**: This is where most of the lost wall-clock time is recovered.

**Independent Test**: Run the sim package with the race detector and confirm the touched tests run in parallel and pass with no data races or cross-test interference.

**Acceptance Scenarios**:

1. **Given** the touched channel-family sim tests, **When** the sim package runs, **Then** each calls `t.Parallel()` unless a comment next to it states the specific process-global it still needs.
2. **Given** the parallel run under `-race`, **When** it completes, **Then** every test passes and no race is reported.

---

### User Story 3 - Waits are driven by the consumer draining, not by sleeping (Priority: P1)

A test author who needs "the channel consumer has processed what the last poll produced" blocks on a deterministic drain point and returns as soon as it is reached, rather than sleeping a fixed 150 ms per condition check.

**Why this priority**: Removes the second compounding cause and makes tests both faster and less flaky.

**Independent Test**: A test that waits for an event returns promptly once the consumer has processed the queue, and fails with a clear message (not a hang) if the drain does not occur within a bounded real-time cap.

**Acceptance Scenarios**:

1. **Given** a poll that enqueued channel changes, **When** the test waits on the drain point, **Then** it returns as soon as the consumer has processed the queue up to the current change sequence.
2. **Given** a consumer that never drains, **When** the bounded cap elapses, **Then** the wait fails the test with a message identifying the drain failure.
3. **Given** a place where no drain point exists (for example, waiting on a timer-driven tick), **When** a real-time fallback is used, **Then** it is short and its reason is documented where it is used.

---

### User Story 4 - A new sim test file cannot silently go serial (Priority: P2)

A contributor adding a sim test file without `t.Parallel()` gets a failing `go test ./tests/sim` that names the file, unless the file is on a reasoned exemption list.

**Why this priority**: Prevents the regression from recurring, but the package is already usable without it.

**Independent Test**: Point the guard at a fixture file with no `t.Parallel()` and confirm it fails; point it at an exempted file and confirm it passes.

**Acceptance Scenarios**:

1. **Given** a sim test file with no `t.Parallel()` that is not exempted, **When** `go test ./tests/sim` runs, **Then** the guard fails and names the file.
2. **Given** a file on the exemption list, **When** the guard runs, **Then** it passes, and every exemption entry carries a stated reason.
3. **Given** the current package, **When** the guard runs, **Then** it passes.

---

### Edge Cases

- A test that genuinely needs a process-global (a remaining global seam or shared resource) stays serial or exempt, with a comment saying why.
- A test that relies on the seam being global across engines it constructs itself (for example a restart that builds a second engine) must still see the setting it configured; scoping must carry across `RestartEnv`-style engine rebuilds where the test expects continuity.
- A neutralisation toggle that is never reset today must not leak: with per-engine scope there is nothing to reset, and no test may depend on a leak.
- The drain wait must not report "drained" while a poll's changes are still being enqueued; it must target the change sequence as of the call.
- Under heavy parallel load with `-race`, the bounded real-time cap must be generous enough not to produce false failures, yet short enough to fail fast on a real hang.
- Engines created without the channel hub started must not hang a drain wait.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The channel test seams currently exposed from `engine/channel_seam.go` (channel timing, and every `Set*DisabledForTest` neutralisation toggle that affects channel, situation, anomaly, decision, downtime, settle or default-family behaviour) MUST be scoped to a single engine (or single hub) instead of a process-global, so that two engines in one test binary cannot observe each other's settings.
- **FR-002**: Each scoped seam MUST keep its current behaviour for the engine it is set on.
- **FR-003**: Every existing non-vacuity test MUST still fail when its fix is neutralised through the scoped seam.
- **FR-004**: The channel, situation, anomaly, decision and default-family sim tests (and the related overseer tests in scope) MUST call `t.Parallel()`, except where a documented process-global is still required, in which case a comment at the test states exactly why.
- **FR-005**: `settleWait()`'s fixed real-time sleep MUST be replaced by a deterministic wait for the channel consumer to have processed everything enqueued up to the current change sequence, bounded by a real-time cap that fails the test with a clear message on expiry.
- **FR-006**: A short real-time wait MAY remain only where no drain point exists, and each such use MUST be documented with the reason.
- **FR-007**: A guard in `tests/sim` MUST fail `go test ./tests/sim` when a sim test file lacks `t.Parallel()` and is not on an explicit exemption list; each exemption MUST carry a stated reason.
- **FR-008**: The guard MUST be demonstrated to fail on a fixture file without `t.Parallel()` and pass on the current package.
- **FR-009**: The PR MUST show each neutralisation failing once, and quote the measured `tests/sim` CI durations from the PR's own runs.

### Key Entities

- **Channel test seam**: A test-only setting that alters channel behaviour or timing, or disables a specific fix so a non-vacuity test can prove its positive test is meaningful.
- **Drain point**: A moment at which the channel consumer has processed all changes enqueued up to a given change sequence.
- **Exemption list**: The explicit, reasoned list of sim test files allowed to omit `t.Parallel()`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `tests/sim` wall-clock time in CI, measured on the PR's own runs, is at most 420 s (against 551–600 s before).
- **SC-002**: `go test -race ./tests/sim` passes with the channel-family tests running in parallel and no race reported.
- **SC-003**: `go test -race ./...` is clean.
- **SC-004**: Every non-vacuity test in the touched files still fails under its neutralisation, each shown once in the PR.
- **SC-005**: The parallelism guard fails on a fixture file without `t.Parallel()`.
- **SC-006**: No fixed 150 ms sleep remains in `settleWait()`; any remaining real-time wait is documented.

## Assumptions

- "At most 420 s" is judged on the Test and vet job's `tests/sim` duration, as in the issue's table, taking the PR's runs as the measurement.
- Per-engine scoping is chosen at implementation time (per-`Engine` or per-hub); the spec requires only that engines in one process cannot see each other's settings.
- "Related overseer tests" means overseer sim tests that share these seams or the same wait helper; overseer tests with no such dependency are untouched beyond what the guard requires.
- The guard operates per file (as the issue states) and may be implemented by statically scanning test sources, as other static scans in the repo do.
- Sim tests outside the channel family that are already parallel are not changed.
- The CI timeout value is left as it is; a separate stopgap PR owns it.

## Out of Scope

- Sharding the sim package (ADR-1857).
- Per-PR test selection.
- Changing the CI timeout itself.
- Speeding up the per-poll cost of the sim engine, which was measured as unchanged.
- Changing channel product behaviour; this is test-infrastructure work only.

## Source References

- #2069 (raised the sim CI cap from 5m to 10m); #2122 and PR #2209 (the first to time out); #1966 (channel feature family, mentioned for context by bare number only; the PR must use no closing keyword near it).
- ADR-1857 (no per-PR test selection, sim package not sharded).
