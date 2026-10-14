# Feature Specification: SIGHUP drains in-flight workers before restarting

**Feature Branch**: `fabrik/issue-2259`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

SIGHUP is documented as \"restart in place\", and operators use it as the graceful restart. But the handler (`engine/sighup_unix.go`) cancels the run context on the first SIGHUP, so every in-flight worker is killed. `drainAndExit` then waits at most `drainDeadline()` (default 30 s) and re-execs.

Seen on 2026-10-13: a SIGHUP to the dev daemon cancelled #2210's comment pass 14 turns in, about $5.76 of Claude usage, and re-exec'd five seconds later. The comment kept 👀 without 🚀, so the whole pass ran again. The operator expected drain-then-restart.

## Requirements

- **R1: The first SIGHUP drains instead of cancelling.** On the first SIGHUP the engine stops dispatching new work: no stage runs, comment passes, reinvokes, merge-train steps or conflict resolutions start. In-flight workers keep their context and run to their own end. Once none are left, the engine re-execs exactly as it does today (webhook manager stopped, local API and channel hub closed, locks released).
  - The drain is not cut off by `drainDeadline()`. Each worker is already bounded by `max_wall_time` and the inactivity timeout, so the drain is bounded too.
  - A merge-train run that is between steps (a trial waiting on CI, ADR 2051) is not a worker. It does not hold the restart; its persisted run resumes after the exec.
  - Polling and settle scans may stop at once. Nothing needs to be written to GitHub to drain.
- **R2: A second SIGHUP restarts now.** A second SIGHUP during the drain cancels the remaining workers, runs today's bounded drain, and re-execs. It no longer force-quits with exit 1. SIGINT/SIGTERM behaviour is unchanged.
- **R3: Say it is draining.** On the first SIGHUP, log the in-flight set (issue, stage, kind, elapsed) and repeat that line every minute until it is empty. The TUI title and `fabrik_health` show the daemon as draining for a restart, with the count of workers left. Log the second-SIGHUP hint once (\"send SIGHUP again to restart now\").
- **R4: No pause, no lost work.** A drained restart must not pause any item or apply `fabrik:paused`, which matches today's SIGHUP. A worker that finishes during the drain completes normally: labels, comments, push, 🚀.
- **R5: Self-upgrade unchanged.** The idle-only self-upgrade re-exec path is not affected.

## Scope

The SIGHUP handler and `drainAndExit`'s SIGHUP branch, the dispatch gate, `daemonHealth`/TUI status, the docs (`docs/USER_GUIDE.md` signal section, `docs/state-machine.md`) and an ADR. No change to SIGINT/SIGTERM (ADR-1393).

## Acceptance

- A test with a scripted worker that is mid-run shows that the first SIGHUP leaves the worker's context uncancelled, no new dispatch happens, and the re-exec hook runs only after the worker returns.
- A test shows that a second SIGHUP cancels the worker and re-execs.
- Each test is shown to be non-vacuous by neutralising the change.
- The docs describe SIGHUP as drain-then-restart."

## Background

SIGHUP is the operator's graceful "restart in place" signal (also sent by `ctrl+r` in the TUI), and the documentation already describes it as draining in-flight workers. In practice the first SIGHUP cancels the run context, which kills every in-flight worker; the drain that follows waits at most 30 seconds (`drainDeadline()`) and then re-execs. Work in flight is thrown away and has to be redone.

On 2026-10-13 a SIGHUP to the dev daemon cancelled #2210's comment pass 14 turns in, discarding about $5.76 of Claude usage, and the process re-exec'd five seconds later. The comment kept its 👀 reaction without 🚀, so the entire pass ran again after the restart. The operator expected the restart to wait for the worker to finish.

The fix makes the first SIGHUP a true drain (stop starting work, let running work finish, then restart) and keeps an explicit "restart now" escape hatch as a second SIGHUP. Workers are already bounded by `max_wall_time` and the inactivity timeout, so an uncapped drain still terminates.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Graceful restart without losing in-flight work (Priority: P1)

An operator sends SIGHUP (or presses `ctrl+r`) while one or more workers are mid-run. The daemon stops starting new work, lets the running workers finish normally (labels, comments, push, 🚀), and only then re-execs.

**Why this priority**: This is the reported defect: the graceful restart destroys paid-for work and forces a full redo.

**Independent Test**: Start an engine with a scripted worker blocked mid-run, deliver one SIGHUP, and observe that the worker's context is not cancelled, no further dispatch happens, and the re-exec hook has not run; release the worker and observe the re-exec hook run afterwards.

**Acceptance Scenarios**:

1. **Given** a worker is mid-run, **When** the first SIGHUP arrives, **Then** the worker's context stays uncancelled and the worker keeps running.
2. **Given** a drain is in progress and an item becomes eligible for a stage run, comment pass, reinvoke, merge-train step or conflict resolution, **When** the next dispatch opportunity occurs, **Then** nothing new is started.
3. **Given** a drain is in progress, **When** the last worker returns, **Then** the engine re-execs as it does today (webhook manager stopped, local API and channel hub closed, locks released).
4. **Given** a worker finishes normally during the drain, **When** it completes, **Then** its labels, comments, push and 🚀 reaction are applied as in a normal run, and no item is paused and `fabrik:paused` is not applied.

---

### User Story 2 - Second SIGHUP restarts now (Priority: P1)

An operator who does not want to wait for a long-running worker sends SIGHUP a second time. The remaining workers are cancelled, today's bounded drain runs, and the engine re-execs.

**Why this priority**: Without an escape hatch, an unbounded drain would be a regression for wedged or very long workers.

**Independent Test**: With a scripted worker blocked mid-run, deliver two SIGHUPs and observe the worker's context cancelled, the bounded drain run, and the re-exec hook run — with the process not exiting with status 1.

**Acceptance Scenarios**:

1. **Given** a drain is in progress with a worker still running, **When** a second SIGHUP arrives, **Then** the remaining workers are cancelled, the bounded drain (`drainDeadline()`) runs, and the engine re-execs.
2. **Given** a second SIGHUP, **When** it is handled, **Then** the process does not force-quit with exit code 1.

---

### User Story 3 - Visible drain status (Priority: P2)

While draining, the operator can see that the daemon is draining for a restart and how many workers remain, in the log, the TUI title and `fabrik_health`.

**Why this priority**: An uncapped drain with no feedback looks like a hang; the operator needs to know what is being waited for and that SIGHUP again forces the restart.

**Independent Test**: Deliver one SIGHUP with workers in flight and check the log line, the TUI title state and the `fabrik_health` output; advance time by a minute and check the repeated log line; let the last worker finish and check that the repeats stop.

**Acceptance Scenarios**:

1. **Given** workers are in flight, **When** the first SIGHUP arrives, **Then** the log lists the in-flight set (issue, stage, kind, elapsed) and the second-SIGHUP hint ("send SIGHUP again to restart now") is logged once.
2. **Given** the drain is still in progress, **When** a minute passes, **Then** the in-flight line is logged again, repeating every minute until the set is empty.
3. **Given** a drain is in progress, **When** the TUI title or `fabrik_health` is read, **Then** each shows the daemon as draining for a restart with the count of workers left.

---

### User Story 4 - Merge-train runs between steps do not hold the restart (Priority: P2)

A merge-train run waiting on a trial's CI is not a worker. A drain does not wait for it; its persisted run resumes after the exec.

**Why this priority**: Waiting on CI could be much longer than any worker, and ADR 2051 already makes the run durable across restarts.

**Independent Test**: With a persisted merge-train run whose trial is waiting on CI and no workers in flight, deliver SIGHUP and observe an immediate re-exec; verify the run resumes afterwards.

**Acceptance Scenarios**:

1. **Given** a merge-train run is between steps and no workers are in flight, **When** SIGHUP arrives, **Then** the engine re-execs without waiting on the run.
2. **Given** a merge-train step goroutine or conflict-resolution worker is executing, **When** SIGHUP arrives, **Then** it counts as in flight and the drain waits for it.

---

### Edge Cases

- SIGHUP with no workers in flight: the engine re-execs immediately (same as today, no wait).
- SIGINT or SIGTERM during a SIGHUP drain: behaves as it does today (cancel and stop); ADR-1393 is not changed.
- A third or later SIGHUP after the second: no additional effect beyond the restart already under way.
- A worker that never finishes: bounded by `max_wall_time` (if set) and the 15-minute inactivity timeout, or ended by the second SIGHUP.
- A worker finishing during the drain would normally trigger follow-up work (advance, reinvoke, next stage): follow-up dispatch is suppressed and picked up by the new process after the exec.
- Windows: SIGHUP does not exist there; behaviour is unchanged.
- Instance-lock loss during a drain keeps its existing handling (no SIGHUP restart branch).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: On the first SIGHUP the engine MUST stop starting new work of every kind: stage runs, comment passes, reinvokes (review, CI-fix, rebase), merge-train steps and merge-train conflict resolutions.
- **FR-002**: On the first SIGHUP, in-flight workers MUST keep their context uncancelled and run to their own end.
- **FR-003**: Once no workers remain in flight, the engine MUST re-exec exactly as the current SIGHUP path does (webhook manager stopped, local API and channel hub closed, instance locks released before the exec).
- **FR-004**: The first-SIGHUP drain MUST NOT be cut off by `drainDeadline()`.
- **FR-005**: A merge-train run that is between steps (for example a trial waiting on CI) MUST NOT be treated as an in-flight worker and MUST NOT delay the restart; its persisted run MUST resume after the exec.
- **FR-006**: Polling and settle scans MAY stop immediately on the first SIGHUP; draining MUST NOT require any write to GitHub.
- **FR-007**: A second SIGHUP during the drain MUST cancel the remaining workers, run the existing bounded drain, and re-exec, and MUST NOT force-quit with exit code 1.
- **FR-008**: SIGINT and SIGTERM behaviour MUST be unchanged (ADR-1393), including when received during a SIGHUP drain.
- **FR-009**: On the first SIGHUP the engine MUST log the in-flight set with issue, stage, kind and elapsed time per worker, and MUST repeat that line every minute until the set is empty.
- **FR-010**: The engine MUST log the hint "send SIGHUP again to restart now" exactly once per drain.
- **FR-011**: While draining, the TUI title and `fabrik_health` MUST report that the daemon is draining for a restart and the number of workers remaining.
- **FR-012**: A drained restart MUST NOT pause any item or apply `fabrik:paused`.
- **FR-013**: A worker that finishes during the drain MUST complete normally, including labels, comments, push and the 🚀 reaction.
- **FR-014**: The idle-only self-upgrade re-exec path MUST be unaffected.
- **FR-015**: `docs/USER_GUIDE.md` (signal section) and `docs/state-machine.md` MUST describe SIGHUP as drain-then-restart with the second-SIGHUP behaviour, `docs/llms-full.txt` MUST be regenerated, and an ADR MUST record the decision.
- **FR-016**: Tests MUST show (a) first SIGHUP leaves a mid-run scripted worker's context uncancelled, dispatches nothing new, and runs the re-exec hook only after the worker returns, and (b) a second SIGHUP cancels the worker and re-execs; each test MUST be shown non-vacuous by neutralising the change.

### Key Entities *(if applicable)*

- **Drain state**: The daemon-level condition "draining for a restart", with the count of workers still in flight; surfaced in logs, the TUI title and `fabrik_health`.
- **In-flight worker**: A running stage run, comment pass, reinvoke or merge-train conflict resolution holding a worker slot; excludes a merge-train run that is merely waiting between steps.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A SIGHUP delivered while a worker is mid-run results in zero cancelled workers and zero Claude turns discarded, and the worker's completion work (labels, comments, push, 🚀) is applied before the restart.
- **SC-002**: After a first SIGHUP, zero new workers start until the re-exec completes.
- **SC-003**: A second SIGHUP results in the restart completing within the existing bounded drain time (`drainDeadline()`, default 30 s) of that signal, with a non-failure outcome.
- **SC-004**: During a drain, an operator can determine from the log, TUI title or `fabrik_health` within one minute that the daemon is draining and how many workers remain.
- **SC-005**: No item is paused or labelled `fabrik:paused` as a result of a drained restart.

## Assumptions

- `ctrl+r` in the TUI sends SIGHUP and therefore inherits the new behaviour, with a second `ctrl+r` acting as the second SIGHUP. The existing `docs/USER_GUIDE.md` text already says `ctrl+r` drains in-flight workers; it becomes accurate, and the stale wording about workers being "interrupted by the SIGHUP drain" is corrected.
- The 15-minute inactivity timeout and `max_wall_time` bound every worker, so an uncapped first-phase drain terminates; no new drain cap is introduced.
- Follow-up work a finishing worker would normally trigger is deferred to the new process rather than dispatched during the drain.
- "Kind" in the in-flight log line uses the existing worker kinds (stage run, comment review, review/CI-fix/rebase reinvoke, merge-train conflict resolution).
- The ADR is numbered after this issue (`adrs/2259-…`).

## Out of Scope *(optional)*

- Any change to SIGINT/SIGTERM handling (ADR-1393).
- A configurable drain timeout or a "drain only, don't restart" mode.
- Changes to the idle-only self-upgrade path.
- Making an in-progress merge-train trial wait part of the drain.
- Windows signal support.

## Source References *(optional)*

- `engine/sighup_unix.go` (SIGHUP handler), `drainAndExit` and `drainDeadline()`
- ADR-1393 (clean stop/shutdown), ADR 2051 (persisted merge-train run state)
- `docs/USER_GUIDE.md` "Recovering from Wedged State" (SIGHUP restart), `docs/state-machine.md`
- Incident: #2210 comment pass cancelled by SIGHUP on 2026-10-13
