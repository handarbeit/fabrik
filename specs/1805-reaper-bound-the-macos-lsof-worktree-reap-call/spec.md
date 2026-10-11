# Feature Specification: Bound the macOS lsof worktree-reap call with a timeout, and cut its per-call cost

**Feature Branch**: `fabrik/issue-1805`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "reaper: bound the macOS lsof worktree-reap call with a timeout, and cut its per-call cost. `reapWorktreeProcessesDarwin` (`engine/reaper_unix.go:152`, ADR-063) runs `exec.Command("lsof", "-a", "-d", "cwd", "-n", "-P", "-Fpcn")` with no context and no timeout, on every `CleanupWorktree` (`engine/worktree.go`). Two costs were observed during #1798: (1) It can block worktree cleanup indefinitely. A full `go test -race ./engine/...` run timed out at 300s with a goroutine blocked in exactly this call (`reaper_unix.go:155` ← `reapWorktreeProcesses` ← `CleanupWorktree`). (2) Each call is expensive. During a full sim-suite run, its `lsof` children sat at 98–99% CPU for 2–4 seconds each, repeatedly. Many sim scenarios reach Done and clean up a worktree, so the suite pays this over and over. On a host that is already loaded, which is when cleanup matters most, a slow `lsof` adds to the load. Other probes in the package are already bounded: `descendant_reap_unix.go` wraps its `ps` call in `context.WithTimeout(..., sentinelProbeTimeout)`. This one was missed. The original question from #1798 (why an orphaned `sim.test` busy-spun) is out of scope here. #1989's session reap and the gate's host-load and orphan probes (#1974) have since addressed the orphan source and its detection, so a multi-hour reproduction is not worth its cost. R1. Bound the call: the `lsof` invocation runs under a context timeout, following the `sentinelProbeTimeout` precedent (Plan picks the value and whether to reuse that constant). On timeout, the process is killed. This cleanup's process reap is skipped with one warning that names the worktree and the timeout, and worktree removal proceeds. The reap never blocks `CleanupWorktree` beyond the timeout. R2. Reduce the per-call cost where it is cheap and safe to do so. Research measures where the time goes. Plan adopts whatever is a meaningful, low-risk saving, or records why none is. Candidates: skipping the reap when nothing could be running in the worktree (for example, no worker or command session for the item is known to be live), as long as the reaper's purpose (orphans whose cwd is under the worktree) is not weakened; narrowing what `lsof` enumerates; reusing the process listing `internal/sessionreap` already takes (`kern.proc.all` on macOS), if a process's cwd can be obtained without cgo; a test seam so the sim and unit suites don't spawn a real `lsof` per cleanup, provided the real path keeps test coverage. Each candidate adopted needs a test showing the reaper still kills a process whose cwd is under the worktree. R3. Linux parity check: confirm the `/proc` path has no equivalent unbounded call. If it does, apply R1 there too. Scope — in scope: `engine/reaper_unix.go` and its tests, plus any seam or helper R2 needs; ADR-063 or `docs/stage-lifecycle.md` if behaviour they describe changes. Out of scope: reproducing the original `sim.test` spin; changes to `internal/sessionreap`'s kill semantics. Acceptance: a unit test with a fake `lsof` that never exits shows `CleanupWorktree` (or the reaper) returning within the timeout, logging the warning and skipping the reap; the existing reaper tests pass, and the reaper still kills a process rooted under the worktree; the PR reports the before/after cost of the reap per cleanup (time per call, and the sim suite's wall-clock if R2 changed it), or states that R2 found no worthwhile saving and why."

## Background

When a worktree is torn down, Fabrik runs a reaper (ADR-063) that kills any process whose current working directory is under that worktree, so orphaned processes cannot keep a deleted directory alive or keep consuming CPU. On macOS, the reaper discovers those processes by running `lsof` over every process's cwd. That child process is started with no context and no deadline, on every worktree cleanup.

This has two observed costs, both found while investigating #1798:

- **It can hang worktree cleanup indefinitely.** A full `go test -race ./engine/...` run timed out at 300 seconds with a goroutine stuck in exactly this call, reached via `reapWorktreeProcesses` from `CleanupWorktree`. A stuck probe blocks the cleanup it is part of.
- **It is expensive on every call.** During a full sim-suite run, the `lsof` children sat at 98–99% CPU for 2–4 seconds each, repeatedly, because many sim scenarios reach Done and clean up a worktree. On a host that is already loaded, which is when cleanup matters most, a slow `lsof` adds to the load.

Other probes in the same package are already bounded (the `ps` call in `descendant_reap_unix.go` uses a context timeout, `sentinelProbeTimeout`); this one was missed. The original #1798 question (why an orphaned `sim.test` busy-spun) is out of scope: #1989's session reap and the gate's host-load and orphan probes (#1974) have since addressed the orphan source and its detection.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Worktree cleanup can never hang on the process probe (Priority: P1)

An operator (or a test run) triggers worktree cleanup while the macOS process probe is slow, wedged or never returns. Cleanup completes anyway, within a bounded time.

**Why this priority**: It is the correctness defect: an unbounded external call inside a cleanup path can stall a worker or time out a whole test run.

**Independent Test**: Substitute a fake `lsof` that never exits, run the reaper (or `CleanupWorktree`) and assert it returns within the timeout, logs one warning naming the worktree and the timeout, kills the probe process, and lets worktree removal proceed.

**Acceptance Scenarios**:

1. **Given** an `lsof` that never exits, **When** the worktree reap runs, **Then** it returns within the timeout, the `lsof` process is killed, and exactly one warning naming the worktree and the timeout is logged.
2. **Given** that timeout, **When** `CleanupWorktree` runs, **Then** the worktree is still removed and no process reap is attempted for that cleanup.
3. **Given** an `lsof` that returns normally, **When** the reap runs, **Then** behaviour is unchanged: processes whose cwd is under the worktree are killed.

---

### User Story 2 - The reap costs less per cleanup where that is cheap and safe (Priority: P2)

Operators and the sim suite stop paying seconds of `lsof` CPU per cleanup when the saving is meaningful and low-risk, without weakening what the reaper catches.

**Why this priority**: Secondary to the hang; it reduces load on an already loaded host and shortens the sim suite, but is optional if no safe saving exists.

**Independent Test**: For each cost reduction adopted, a test shows the reaper still kills a process whose cwd is under the worktree. The PR reports the measured per-cleanup reap time before and after (and the sim suite wall-clock if it changed), or states that no worthwhile saving was found and why.

**Acceptance Scenarios**:

1. **Given** a cost reduction is adopted, **When** a process with its cwd under the worktree exists at cleanup, **Then** it is still killed.
2. **Given** the sim or unit suites run, **When** worktrees are cleaned up, **Then** they do not spawn a real `lsof` per cleanup unless the test is specifically exercising the real path, and the real path retains test coverage.
3. **Given** research finds no meaningful low-risk saving, **When** the PR is opened, **Then** it records that finding and why, and the reaper's behaviour is otherwise unchanged apart from the timeout.

---

### User Story 3 - Linux has no equivalent unbounded call (Priority: P3)

A maintainer can trust that the Linux `/proc`-based reap path does not have the same defect.

**Why this priority**: A parity check on the other platform; likely to be a confirmation only.

**Independent Test**: Review of the `/proc` path (and a test if a bound is needed) shows no unbounded external call; if one exists, it is bounded as in User Story 1.

**Acceptance Scenarios**:

1. **Given** the Linux reap path, **When** it is inspected, **Then** either it is confirmed to make no unbounded blocking call, or it is bounded the same way as the macOS path.

---

### Edge Cases

- `lsof` is missing or fails with a permission error: today this is logged and the reap skipped; that behaviour is preserved and is distinct from the timeout warning.
- `lsof` exits non-zero because no process matched, with usable output: not an error, as today.
- The probe has spawned children of its own, or the host is heavily loaded so a legitimate run takes longer than the timeout: the reap is skipped for that cleanup and the worktree is still removed; no process is left running from the probe.
- A skipped reap means an orphan with a cwd under the worktree may survive that one cleanup; this is accepted and is reported through the warning, not retried.
- Concurrent cleanups of several worktrees each get their own bounded probe.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The macOS worktree-reap `lsof` invocation MUST run under a context timeout, and on timeout the `lsof` process MUST be killed.
- **FR-002**: On timeout, the process reap for that cleanup MUST be skipped, exactly one warning naming the worktree and the timeout MUST be logged, and worktree removal MUST proceed.
- **FR-003**: The reap MUST NOT block `CleanupWorktree` beyond the timeout (plus process-kill overhead).
- **FR-004**: The timeout value follows the `sentinelProbeTimeout` precedent; whether to reuse that constant or define a separate one is a planning decision, and the chosen value MUST be documented where it is defined.
- **FR-005**: Research MUST measure where the per-call `lsof` time goes, and the outcome MUST be that either a meaningful, low-risk cost reduction is adopted or the PR records why none is.
- **FR-006**: Any cost reduction adopted MUST NOT weaken the reaper's purpose: processes whose cwd is under the worktree MUST still be killed. Candidates to evaluate are: skipping the reap when nothing could be running in the worktree; narrowing what `lsof` enumerates; reusing the process listing `internal/sessionreap` already takes, if cwd is obtainable without cgo; and a test seam so the sim and unit suites do not spawn a real `lsof` per cleanup while the real path keeps coverage.
- **FR-007**: Each cost reduction adopted MUST have a test showing a process whose cwd is under the worktree is still killed.
- **FR-008**: The Linux `/proc` path MUST be confirmed to have no equivalent unbounded call; if it has one, it MUST be bounded as in FR-001 and FR-002.
- **FR-009**: Documentation (ADR-063 and `docs/stage-lifecycle.md`) MUST be updated if the behaviour they describe changes, including the regenerated `docs/llms-full.txt` when a canonical doc page changes.
- **FR-010**: The PR MUST report the before/after reap cost per cleanup (time per call, and the sim suite wall-clock if R2 changed it), or state that no worthwhile saving was found and why.

### Key Entities

- **Worktree reap**: the teardown step that finds and kills processes whose cwd is under a worktree being removed (ADR-063).
- **Process probe**: the external command (`lsof` on macOS) used to enumerate process working directories.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a probe that never exits, worktree cleanup returns within the configured timeout in 100% of runs, and the worktree is removed.
- **SC-002**: The timeout path produces exactly one warning per affected cleanup, naming the worktree and the timeout.
- **SC-003**: All pre-existing reaper tests pass, and a test confirms a process rooted under the worktree is still killed on every path touched.
- **SC-004**: The PR states the measured per-cleanup reap time before and after the change (and sim suite wall-clock if it changed), or states that no worthwhile saving was found and why.
- **SC-005**: No unbounded external call remains in the worktree-reap path on either macOS or Linux.

## Assumptions

- The timeout is generous enough that a healthy `lsof` on a loaded host normally completes; a skipped reap on timeout is acceptable because orphan detection and session reaping (#1974, #1989) cover the main orphan source.
- Skipping the reap is a safe degradation: worktree removal matters more than catching an orphan on one cleanup.
- A cost reduction is only worth adopting if it is meaningful and low-risk; "no worthwhile saving" is an acceptable outcome of R2.
- Obtaining a process's cwd without cgo may not be feasible; that candidate is evaluated, not required.

## Out of Scope *(optional)*

- Reproducing the original #1798 `sim.test` busy-spin.
- Changes to `internal/sessionreap`'s kill semantics.
- Retrying a skipped reap, or adding a new background sweeper.
- Changing which processes the reaper considers orphans.

## Source References *(optional)*

- #1798 (R4, AC6): the investigation this came from
- `engine/reaper_unix.go`, `engine/descendant_reap_unix.go` (`sentinelProbeTimeout`), ADR-063 (`adrs/063-worktree-cwd-rooted-process-reaper.md`), `docs/stage-lifecycle.md`
