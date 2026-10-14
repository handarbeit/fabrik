# Feature Specification: `gate report` reads every per-phase test stream, and prints per-phase wall-clock

**Feature Branch**: `fabrik/issue-2257`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`gate report` (#1992, `tests/gate/report.go`) totals each test's `Elapsed` from `<archive>/<cell>/<invocation>/go-test.json`, merged with its `go-test.retry-N.json` files. Since #1977 split each leg into phases, the archive holds one stream per phase instead:

- `go-test.shared.json`
- `go-test.default-base-train.json`
- `go-test.exclusive.json`
- `go-test.retry-N.<phase>.json`

The report finds none of them and prints `0s` for every test, with \"50 registry test(s) have no archived timing\". This was seen when settling #1990 after the v0.0.84 gate.

`report_test.go` writes only `go-test.json` fixtures, so CI never sees the problem.

## Requirements

- R1: `readInvocationEvents` reads every phase stream in an invocation, and the retry streams for each phase, in both the phase and the pre-#1977 layout.
- R2: the report test covers a phased invocation with a retry, and fails against the current reader.
- R3: per cell, the report also prints each phase's wall-clock as recorded in `phases.json`, so a go/no-go like #1990's needs no log scraping.

## Acceptance

- `gate report --sha <sha>` on the v0.0.84 ledger (`.e2e-coverage/9ea9b839…`) shows non-zero timings for every covered test."

## Background

`gate report` (introduced by #1992) tells an operator how long each live e2e test took in each auth × train cell of a gate run, joined with the registry's `entry`/`traversal` fields. It is the data behind go/no-go decisions about the live suite's cost, such as the one made when settling #1990 after the v0.0.84 gate.

It reads the archived `go test -json` stream of each invocation of each cell. When #1977 split each leg into phases (shared, then default-base-train, then exclusive), the archive layout changed. An invocation now holds one stream per phase, and each INCONCLUSIVE retry adds a per-phase retry stream. Only the no-registry fallback, a single undivided `go test`, still writes the old bare `go-test.json`. The report was never updated, so on any real post-#1977 archive it finds no stream, prints `0s` for every test, and says that around 50 registry tests "have no archived timing".

CI did not catch this. The report's test fixtures were written only in the pre-#1977 layout, so the reader and its test agreed with each other while disagreeing with what the gate actually writes.

Each leg also archives a `phases.json` with per-phase wall-clock, peak concurrent tests and GraphQL spend. The report does not surface it. Whoever needs to know how long each phase took must scrape logs.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Per-test timings from a phased archive (Priority: P1)

An operator runs `gate report --sha <sha>` against a ledger produced by the current gate (phased layout) and sees each covered test's measured elapsed time per cell.

**Why this priority**: This is the bug. Without it the report is wrong for every gate run since #1977, and it reports a misleading figure (`0s`) rather than failing.

**Independent Test**: Build an archive fixture in the phased layout, with a stream per phase and a retry stream for at least one phase. Run the report builder. Every test in every stream gets its measured elapsed time, and a test that was retried is attributed the correct, merged outcome.

**Acceptance Scenarios**:

1. **Given** an invocation directory containing `go-test.shared.json`, `go-test.default-base-train.json` and `go-test.exclusive.json`, **When** the report is built, **Then** each test's elapsed time from its phase stream is included.
2. **Given** an invocation that also contains `go-test.retry-1.shared.json`, **When** the report is built, **Then** the retry stream is merged with the shared phase stream using the same attempt-merging rule as before, so a test inconclusive in the first attempt and passing on retry is reported once, with the retry's result.
3. **Given** an invocation in the pre-#1977 layout (`go-test.json` plus `go-test.retry-N.json`), **When** the report is built, **Then** the output is unchanged from today.
4. **Given** the v0.0.84 ledger (`.e2e-coverage/9ea9b839…`), **When** `gate report --sha <sha>` is run, **Then** every covered test shows a non-zero timing and the "no archived timing" count no longer reports tests that were in fact run.

---

### User Story 2 - A regression test that would have caught it (Priority: P1)

A maintainer changing the archive layout or the report reader gets a failing test in plain `go test ./...` if the two disagree.

**Why this priority**: The bug survived because the test fixtures matched the reader rather than the writer. Without a phased fixture the bug can recur silently.

**Independent Test**: Run the new phased-invocation-with-retry test against the pre-fix reader and confirm it fails. Run it against the fixed reader and confirm it passes.

**Acceptance Scenarios**:

1. **Given** a phased invocation fixture with a retry, **When** the report test runs against the unfixed reader, **Then** it fails (zero or missing timings).
2. **Given** the same fixture, **When** it runs against the fixed reader, **Then** it passes.

---

### User Story 3 - Per-phase wall-clock in the report (Priority: P2)

An operator reading the report for a cell sees how long each phase of that cell's leg took, as recorded in the archived `phases.json`, without scraping logs.

**Why this priority**: It supports the go/no-go use case the report exists for, but it is additive and does not fix wrong data.

**Independent Test**: Archive a cell with a `phases.json` and run the report. The printed output for that cell lists each recorded phase with its wall-clock.

**Acceptance Scenarios**:

1. **Given** a cell whose invocation has a `phases.json` recording shared, default-base-train and exclusive phases, **When** the report is printed, **Then** that cell's section lists each phase with its wall-clock duration.
2. **Given** a cell with no `phases.json` (pre-#1977 archive, or the no-registry fallback), **When** the report is printed, **Then** no phase section appears for that cell and the report still succeeds.
3. **Given** a cell with an unreadable or malformed `phases.json`, **When** the report is printed, **Then** the per-test timings are still reported and the problem is noted rather than aborting the report.

---

### Edge Cases

- An invocation holding both layouts (bare `go-test.json` and phase streams): all streams are read, and a test appearing in more than one is merged by the existing attempt-merging rule rather than double-counted.
- A phase with no tests, or an absent phase stream (for example the exclusive phase when no exclusive test was selected under `--resume`): skipped without error.
- Retry streams for one phase only: they merge with that phase's stream, not with the others.
- Multiple invocations of one cell (resumed runs): existing per-invocation handling is unchanged.
- Retry numbering with two or more digits (`retry-10`): ordered numerically, not lexically, consistent with how the existing reader orders retries.
- A phase name that does not match the three known phases: still read, so a future phase needs no change to the reader.
- Phase names are discovered from the files present, so the report does not depend on a hard-coded phase list that can drift from the gate's.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: For each invocation, the report reader reads every `go-test.<phase>.json` stream present, for any phase name, in addition to a bare `go-test.json` if present.
- **FR-002**: For each invocation, the report reader reads the retry streams of both layouts, `go-test.retry-<n>.json` and `go-test.retry-<n>.<phase>.json`, and merges each with the stream it retries using the existing attempt-merging rule.
- **FR-003**: A pre-#1977 invocation (bare `go-test.json` and `go-test.retry-<n>.json` only) produces the same report as before this change.
- **FR-004**: An invocation with no recognizable stream still yields no events and no error, as today.
- **FR-005**: The report test suite includes a fixture of a phased invocation with at least one retry stream. That fixture fails against the pre-fix reader and passes against the fixed one.
- **FR-006**: For each cell, the report prints each phase's wall-clock as recorded in that cell's `phases.json`.
- **FR-007**: A cell without a `phases.json`, or with an unreadable one, still gets its per-test timings. The absence or failure is not fatal and is not presented as a zero-length phase.
- **FR-008**: The report's descriptive text, which today names `go-test.json` as the source, describes the phase streams accurately.
- **FR-009**: Running `gate report --sha <sha>` on the v0.0.84 ledger reports non-zero timings for every covered test.

### Key Entities *(if applicable)*

- **Invocation archive**: one invocation of one cell, holding the `go test -json` streams (bare, per-phase, and retry variants of both) and `phases.json`.
- **Phase stream**: the `go test -json` event stream for one phase (`shared`, `default-base-train`, `exclusive`) of a leg.
- **Retry stream**: the re-run of INCONCLUSIVE tests in the same cell (#1973), per phase in the phased layout and bare in the old layout.
- **`phases.json`**: the per-leg record of phase wall-clock, peak concurrent tests, GraphQL spend and host peak load (#1977).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On the v0.0.84 ledger, `gate report --sha <sha>` shows a non-zero measured time for 100% of covered tests, and no covered test is reported as having no archived timing.
- **SC-002**: A new report test with a phased invocation and a retry fails on the pre-fix reader and passes on the fixed one, in plain `go test ./...` with no live budget.
- **SC-003**: The report output for a pre-#1977 archive fixture is byte-identical before and after the change, apart from the corrected source-description text.
- **SC-004**: For a cell with a `phases.json`, an operator can read every phase's wall-clock from the report alone, with no log scraping.

## Assumptions

- The phased layout documented in the archive's own description (`go-test.<phase>.json`, `go-test.retry-<n>.<phase>.json`, a bare `go-test.json` only in the no-registry fallback) is the authoritative description of what the gate writes.
- Each test runs in exactly one phase of one attempt, so reading all phase streams does not double-count a test beyond what retries already imply.
- Phase names are taken from the filenames rather than a hard-coded list, so a future phase is read without a change here.
- Phase wall-clock is shown as recorded; the report does not recompute it from test events. Peak concurrency, GraphQL spend and host load in `phases.json` may be shown if cheap, but only wall-clock is required.
- The v0.0.84 ledger acceptance check is a manual verification by the operator, since the ledger lives in a git-ignored directory on their machine.

## Out of Scope *(optional)*

- Changing how the gate writes the archive, or the `phases.json` format.
- Recording model-quota use (the report's labelled proxy stays as is).
- Back-filling or rewriting old archives.
- New report columns for peak concurrency or GraphQL spend beyond what falls out of printing `phases.json` naturally.
- Any change to the coverage ledger's own reading of outcomes, which is recorded from the event stream rather than from the archived files.

## Source References *(optional)*

- #1992 (`gate report`), #1977 (phased legs), #1973 (INCONCLUSIVE retries), #1990 (where the symptom was seen)
- `tests/gate/report.go`, `tests/gate/archive.go` (archive layout), `tests/gate/phases.go`
