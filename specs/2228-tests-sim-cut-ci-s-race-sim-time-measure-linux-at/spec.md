# Feature Specification: Cut CI's -race sim time — measure Linux at higher parallelism, shard in CI only if it pays

**Feature Branch**: `fabrik/issue-2228`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`tests/sim` is the long pole of CI's `go test -race ./...` step. It takes about 629 s on CI's 4-vCPU Linux runner and runs on every PR. ADR-1857 rejected per-PR test selection and recorded a revisit trigger, \"tests/sim grows toward its timeout\". That trigger fired on 2026-10-09.

What changed since this issue was first filed (v0.0.84, #2248): the **local** sim (the gate's pre-gate, `scripts/sim/run.sh`) no longer runs under `-race`. It runs at `-parallel 16` and takes about 242 s on the release host. So the local half of the original problem is solved. What remains is CI, where `-race` stays and is the only race coverage the sim gets.

`-race -parallel 4` (`default_race_parallel`, #1677) is capped because higher in-process parallelism crashed on macOS: a wedged `fork/exec`, `git: signal: segmentation fault`, `CHECK failed: tsan_rtl.cpp:94`. It has never been tried at higher parallelism on Linux.

## Requirements

- **R1: Linux at higher parallelism (measure).** On a Linux host or an OrbStack VM on the release host, run `go test -race -count=1 -parallel {4,8,16} ./tests/sim/...` five times each. Record the crash rate and wall time. If Linux is clean, make `default_race_parallel` OS-dependent, with a higher value on Linux, and record that the macOS crash is macOS-specific.
- **R2: Shard the sim in CI if it pays.** Compare CI wall time and runner-minutes for the current single job against K shards (a matrix of `go test -race -run <partition>` jobs, K ∈ {2, 3, 4}).
  - Partition deterministically (`go test -list` plus a stable hash), so a test always lands in the same shard.
  - The union must equal the full suite, with no test lost or duplicated. A static check enforces this and fails on a fixture that drops a test.
  - The rest of the module stays in its own job.
  - Adopt sharding only if the cost comparison shows a clear wall-time win at acceptable minutes. Record the comparison either way.
- **R3: Record the decision.** Write an ADR amending ADR-1677 and ADR-1857 with R1's and R2's numbers and what was adopted.

## Scope

CI (`.github/workflows/ci.yml`), `default_race_parallel`, and a partition helper with its check. No change to the local pre-gate, which is already non-race.

## Acceptance

- R1's numbers recorded, whatever they are.
- If R2 adopts sharding: CI's sim wall time quoted against the 629 s baseline, five consecutive green sharded runs, and the partition-completeness check failing on a dropping fixture.
- If R2 rejects sharding: the comparison is in the ADR, and the issue closes with no CI change.

## Related

#2254 (skip Go suites on PRs that change no Go input), #2210, #2226, #2248, ADR-1624, ADR-1677, ADR-1857."

## Background

`tests/sim` is the slowest part of CI's `go test -race ./...` step: about 629 s on CI's 4-vCPU Linux runner, on every PR. ADR-1857 rejected per-PR test selection and named a revisit trigger, "tests/sim grows toward its timeout". That trigger fired on 2026-10-09.

Since this was first filed, v0.0.84 (#2248) moved the **local** sim (the release gate's pre-gate, `scripts/sim/run.sh`) off `-race`. It now runs at `-parallel 16` in about 242 s on the release host, so the local half of the problem is solved. CI is what remains. Its `-race` run is the only race coverage the sim gets, so `-race` stays there.

CI's `-race` run uses `default_race_parallel` (`scripts/lib/parallel.sh`, #1677), which caps in-process parallelism at 4. The cap exists because higher parallelism crashed on macOS: a wedged `fork/exec`, `git: signal: segmentation fault`, `CHECK failed: tsan_rtl.cpp:94`. That cap has never been tested on Linux, which is where CI runs. Two independent levers are therefore open: raise parallelism on Linux if it is stable, and split the sim across parallel CI jobs if that pays.

This is a measure-then-decide issue. The deliverable is the recorded numbers and decision. A CI change ships only if the numbers justify it.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Know whether Linux tolerates higher -race parallelism (Priority: P1)

A maintainer wants to know whether the macOS-motivated cap of 4 is needed on Linux, which is where CI runs.

**Why this priority**: It is the cheapest lever, with no CI structure change. It also changes the baseline that the sharding comparison in User Story 2 is measured against.

**Independent Test**: On a Linux host or OrbStack VM on the release host, run `go test -race -count=1 -parallel N ./tests/sim/...` for N ∈ {4, 8, 16}, five runs each. Check that crash counts and wall times for all 15 runs are recorded.

**Acceptance Scenarios**:

1. **Given** a Linux host, **When** the sim runs under `-race` at each of the three parallelism levels five times, **Then** the crash rate (count and signature of any failure) and wall time per run are recorded for every level.
2. **Given** a level with no crashes across its five runs, **When** a higher level is also clean, **Then** `default_race_parallel` returns a higher value on Linux than on macOS, and the macOS cap stays at 4.
3. **Given** crashes on Linux at some level, **When** results are recorded, **Then** `default_race_parallel` is not raised past the highest clean level, and the crash signatures are written down.

---

### User Story 2 - Decide whether to shard the sim in CI (Priority: P1)

A maintainer wants CI's sim wall time reduced without losing or duplicating any test, and only if the cost is justified.

**Why this priority**: The 629 s sim is the PR-latency long pole. Sharding is the only lever that scales beyond what one runner's parallelism can give.

**Independent Test**: Measure CI wall time and total runner-minutes for the single-job layout against K ∈ {2, 3, 4} shards, using the post-User-Story-1 parallelism. Check the comparison against the adoption rule below. If sharding is adopted, run the partition-completeness check against a fixture that drops a test and confirm it fails.

**Acceptance Scenarios**:

1. **Given** the sim's test list from `go test -list`, **When** it is partitioned into K shards by a stable hash of the test name, **Then** every test lands in exactly one shard, and the same test lands in the same shard on every run and for every K it is evaluated at.
2. **Given** a partition fixture from which one test has been dropped, **When** the static completeness check runs, **Then** it fails and names the missing test.
3. **Given** a partition fixture in which a test appears in two shards, **When** the static completeness check runs, **Then** it fails and names the duplicate.
4. **Given** measured wall time and runner-minutes for K ∈ {2, 3, 4}, **When** the comparison shows a clear wall-time win at acceptable runner-minutes, **Then** CI is changed to a sim shard matrix plus a separate job for the rest of the module, and five consecutive green sharded CI runs are recorded.
5. **Given** the comparison does not show a clear win, **When** the decision is recorded, **Then** the comparison is in the ADR and CI is unchanged.

---

### User Story 3 - Record the decision (Priority: P1)

A future maintainer revisiting CI cost needs the numbers and the reasoning in one place.

**Why this priority**: ADR-1857 set a revisit trigger. This issue answers that trigger, and the answer must be findable from ADR-1677 and ADR-1857.

**Independent Test**: Read the new ADR and confirm it carries R1's and R2's numbers and states what was adopted. Confirm ADR-1677 and ADR-1857 are amended to point to it.

**Acceptance Scenarios**:

1. **Given** the measurements, **When** the ADR is written, **Then** it contains the per-level crash rate and wall time for N ∈ {4, 8, 16}, the single-job versus K-shard comparison (wall time and runner-minutes), and an explicit statement of what was adopted and what was rejected.
2. **Given** the ADR exists, **When** ADR-1677 and ADR-1857 are read, **Then** each carries a note that it is amended by the new ADR.

---

### Edge Cases

- Every Linux level, including 4, shows some crashes. Record this, make no `default_race_parallel` change, and treat the comparison as run at the existing value.
- Linux is clean at 8 but crashes at 16. Raise to 8 only.
- A new sim test is added after sharding is adopted. The deterministic hash must place it in a shard with no manual list edit, and the completeness check must still pass.
- A sim test is renamed or removed. The completeness check compares against the live `go test -list` output, so it must not depend on a stored list that goes stale.
- Sharding multiplies runner setup cost (checkout, module download, build under `-race`). The runner-minutes figure must include that overhead, not just test time.
- Shard sizes are uneven because test durations differ. The comparison must report the slowest shard's wall time, because that sets PR latency.
- The sim has `TestMain`-level setup or package-wide tests that cannot be split by `-run`. They must still run exactly once across the union.
- A flaky or crashing shard must not be masked by a green sibling. CI must fail if any shard fails.
- Branch-protection required checks name the current CI job. Changing job names or adding a matrix must not leave a required check permanently pending.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The Linux `-race` sim run is measured at `-parallel` 4, 8 and 16, five runs each (`go test -race -count=1`), on a Linux host or an OrbStack VM on the release host. For each run the crash outcome (including failure signature) and wall time are recorded.
- **FR-002**: If Linux is clean at a level higher than 4, `default_race_parallel` returns an OS-dependent value, higher on Linux than on macOS, and the macOS value stays at 4. The macOS-specific nature of the crash is recorded in the code comment and the ADR.
- **FR-003**: If Linux is not clean above 4, `default_race_parallel` is unchanged and the failure evidence is recorded.
- **FR-004**: CI wall time and total runner-minutes are measured for the current single sim job and for K ∈ {2, 3, 4} sim shards, with the rest of the module in its own separate job. Runner-minutes include per-job setup overhead. Wall time is the slowest shard's.
- **FR-005**: Sim tests are partitioned across shards deterministically from `go test -list` output and a stable hash of the test name. A given test always lands in the same shard for a given K, with no stored manual assignment.
- **FR-006**: A static check verifies the shard union equals the full `go test -list` set, with no test missing and none duplicated. It fails on a fixture that drops a test, and on a fixture that duplicates one. The failure message names the offending test.
- **FR-007**: Sharding is adopted only if the FR-004 comparison shows a clear wall-time win at acceptable runner-minutes. The criteria used to judge "clear" and "acceptable" are written down in the ADR with the numbers.
- **FR-008**: If sharding is adopted, CI runs the sim shards as a matrix of `go test -race -run <partition>` jobs and runs the rest of the module in a separate job. A failure in any shard fails the PR. Required-check configuration is kept consistent so no check is left permanently pending.
- **FR-009**: If sharding is adopted, CI's sim wall time is quoted against the 629 s baseline, and five consecutive green sharded CI runs are recorded.
- **FR-010**: An ADR, numbered after this issue per the repository's ADR convention (`adrs/2228-…`), records FR-001's and FR-004's numbers and what was adopted. It amends ADR-1677 and ADR-1857, and each of those carries a pointer to it.
- **FR-011**: If sharding is rejected, no CI change is made. The comparison is in the ADR and the issue closes.
- **FR-012**: The local pre-gate (`scripts/sim/run.sh`, non-race, `-parallel 16`) is unchanged.
- **FR-013**: Canonical docs that describe CI's test layers or the `-race` parallelism cap are updated if their content becomes inaccurate. If any of `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md` is edited, `docs/llms-full.txt` is regenerated in the same commit.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The crash rate and wall time for Linux `-race` at parallelism 4, 8 and 16 are recorded as 15 runs (5 per level) in the ADR, whatever the results are.
- **SC-002**: A single-job versus K ∈ {2, 3, 4} shard comparison of CI wall time and runner-minutes is recorded in the ADR, whether or not sharding is adopted.
- **SC-003**: If sharding is adopted, CI's sim wall time is stated against the 629 s baseline, and five consecutive sharded CI runs are green.
- **SC-004**: If sharding is adopted, the partition-completeness check fails on a fixture that drops a test and on one that duplicates a test, and passes on the real suite.
- **SC-005**: No test is lost or run twice by any CI layout adopted. The set of sim tests executed in CI equals the set `go test -list` reports.
- **SC-006**: If Linux parallelism is raised, no new `-race` crash appears across the five consecutive green runs required by SC-003 (when sharding is adopted) or across the FR-001 runs at the adopted level.
- **SC-007**: If sharding is rejected, there is no diff to `.github/workflows/ci.yml` and the ADR still carries the comparison.

## Assumptions

- "Clear wall-time win at acceptable minutes" is judged by the maintainer from the recorded numbers. The ADR states the thresholds actually applied. No threshold is fixed in advance, because the runner-minutes budget is a project judgement that the data should inform.
- The measurement host for R1 is a Linux host or OrbStack VM available on the release host. A 4-vCPU shape matching CI's runner is preferred so wall times transfer.
- CI wall time and runner-minutes are measured on the real GitHub Actions runners via the workflow, not inferred from local runs.
- The Linux-versus-macOS split in `default_race_parallel` is by operating system only, and CI is Linux.
- A deterministic hash of the test name (not of its duration) is acceptable even though shards may be uneven. If unevenness defeats the wall-time win, that is a finding for the ADR rather than a reason to add stored assignments.
- The `-race` coverage of the sim in CI must not shrink: the same tests run under `-race` whether sharded or not.
- Overlap with #2254 (skip Go suites on PRs that change no Go input) is complementary. This issue neither depends on it nor changes what it skips.

## Out of Scope *(optional)*

- Any change to the local pre-gate or `scripts/sim/run.sh`, which is already non-race at `-parallel 16`.
- Per-PR test selection. ADR-1857 rejected it, and this issue does not reopen that decision beyond recording the trigger's outcome.
- Fixing the macOS `-race` crash itself.
- Sharding `tests/sim/simgh` or other packages. ADR-1857 records why it is not sharded.
- Skipping Go suites on PRs with no Go changes (#2254).
- Changes to the live e2e suite or the release gate.

## Source References *(optional)*

- ADR-1624 (sim suite concurrency and pre-gate dedup), ADR-1677 (release-gate hardening), ADR-1857 (no per-PR test selection)
- #1677 (`default_race_parallel`), #2248 (local sim non-race), #2254, #2210, #2226
- `scripts/lib/parallel.sh` (`default_race_parallel`), `.github/workflows/ci.yml`
