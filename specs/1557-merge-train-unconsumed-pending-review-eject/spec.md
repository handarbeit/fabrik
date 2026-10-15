# Feature Specification: Bound the lifetime of merge-train pending-review-eject signals

**Feature Branch**: `fabrik/issue-1557`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `settleQueuedReviewFindings` (ADR-1208) runs on the poll goroutine and, when a merge-train worker is
> in flight for the repo, hands its eject decision to the worker via a mutex-guarded signal
> (`markPendingReviewEject`), which the worker consumes at its own checkpoints
> (`applyPendingReviewEjects`, `engine/merge_train.go`). This is the documented "poll writes a signal,
> worker checks it at a checkpoint" pattern.
>
> The signal is only ever **consumed** at those checkpoints. Every path where the worker returns early
> between a checkpoint and the next one leaves an unconsumed entry behind in
> `e.queuedReviewEjects[repoKey]`.
>
> Concretely: `applyPendingReviewEjects` (Hook 2, `merge_train.go:~646`) drains pending ejects for the
> current trial's survivors. Between that point and the `TrainCIRed`/singleton disposition
> (`~:680`), the poll goroutine can call `markPendingReviewEject` for a member. `ejectRedSingleton`
> then reroutes that member off the train entirely and the worker `return`s — so
> `takePendingReviewEject` is never called for it, and the signal persists.
>
> The same window exists identically on the worker's other early-`return` paths (`TrainCIGreen`, the
> runaway guard, assembly failure).
>
> **Consequence:** a stale eject signal survives into a later, unrelated merge-train batch. If that
> batch happens to contain the same issue number, it can be ejected against a finding count that no
> longer reflects reality — an eject with no current cause, appearing as an unexplained member
> disappearance from a batch.
>
> ### Why this is a follow-up, not a bug fix on a shipped PR
>
> This window is **pre-existing** and was neither introduced nor widened by #1545 / PR #1550 — it was
> found while reviewing that PR, which correctly does not touch this machinery. Probability is low: it
> needs a poll-goroutine `markPendingReviewEject` inside a narrow window, followed by a later batch
> containing the same issue number, within the signal's lifetime.
>
> ## Summary
>
> Make the pending-review-eject signal's lifetime bounded and explicit, so it cannot survive the trial
> it was raised for. The mechanism is left to Research/Plan; candidates:
>
> 1. **Drain on exit** — clear the repo's pending ejects on every worker return path, e.g. via a
>    `defer` established once at worker entry, so no path can leak by omission. Likely the smallest
>    correct change and robust against future early-returns being added.
> 2. **Stamp and expire** — tag each signal with the trial identity that was current when it was
>    raised, and discard on consumption if the trial no longer matches.
> 3. **Re-derive rather than signal** — have the worker re-check review findings at its checkpoints
>    instead of consuming a queued signal, removing the shared mutable state.
>
> Option 1 is preferred on the "new early-returns get it for free" argument, but the correctness
> question — whether an unconsumed signal should be *dropped* or *re-evaluated* on the next batch — is
> a real design decision, not a mechanical one, and should be settled explicitly.
>
> ## Requirements
>
> - **R1** — A signal raised during a trial must not be actionable in any later trial.
> - **R2** — A signal raised for a member that is still legitimately Queued when the next batch forms
>   must not be silently lost if the finding is still unresolved — dropping the signal must not become
>   a way to miss real review feedback. ADR-1208's guarantee (a Queued member's review feedback is seen
>   within one batch cycle) must survive whatever is chosen.
> - **R3** — Whatever is chosen must hold for *every* worker return path, including ones added later,
>   not only the ones enumerated today.
>
> ## Scope
>
> **In scope:** the `markPendingReviewEject` / `takePendingReviewEject` / `applyPendingReviewEjects`
> lifetime, and an ADR-1208 amendment recording the resolution.
>
> **Out of scope:** `settleQueuedReviewFindings`'s detection logic; the eject mechanism itself
> (`ejectMember` / `rerouteQueuedMemberOffHolding`); the red-singleton disposition from #1545.
>
> ## Constraints
>
> - **R2 is the trap.** The naive fix — drop unconsumed signals on exit — risks converting a rare
>   wrong-eject into a systematic missed-eject, which is the worse failure: ADR-1208 exists precisely
>   because a missed review finding on a Queued member is invisible. If option 1 is chosen, confirm the
>   next poll's `settleQueuedReviewFindings` re-raises the signal for a still-unresolved finding.
> - **Do not weaken ADR-1208's within-one-batch-cycle guarantee.**
>
> ## Acceptance Criteria
>
> - **AC1** — A test raising a signal during a trial that ends via an early return, then forming a new
>   batch containing the same issue number, shows the member is not ejected on the stale signal.
> - **AC2** — A test showing an unresolved finding on a still-Queued member is still ejected within one
>   batch cycle after AC1's mechanism is in place — the R2 guard.
> - **AC3** — AC1 shown **red against current `main`** before green.
>
> ## Verification Note
>
> AC2 is the load-bearing criterion and the one most likely to pass vacuously: a test asserting "the
> member was ejected" can pass against code that ejects unconditionally. Pair it with AC1 so the two
> together prove the behaviour differs by signal staleness rather than by ejecting in both cases.
>
> ## Prior Art / Context
>
> - ADR-1208 / #1208 — `settleQueuedReviewFindings` and the pending-eject signal pattern
> - ADR-1270 — the dedicated settle-scan pattern this sits within
> - #1545 / PR #1550 — where this was found; that PR correctly does not touch this machinery
> - Found during independent review: https://github.com/handarbeit/fabrik/pull/1550#issuecomment-5259938954

## Background

The merge-train engine detects unresolved review findings on a Queued member while a train worker is running. The poll side cannot eject the member itself without racing the worker, so it leaves a mutex-guarded "pending eject" signal for the worker. The worker reads and clears that signal only at its own checkpoints (ADR-1208).

If the worker returns between two checkpoints, nothing clears the signal. Return paths include a red or green trial verdict, the runaway guard, an assembly failure, and a red singleton that is rerouted off the train. The stale entry stays in per-repo state and can act in a later, unrelated batch.

If that batch contains the same issue number, the member can be ejected for a finding that no longer exists. To an operator this looks like a member disappearing from a batch for no reason.

This was found while reviewing #1545 / PR #1550 and predates that work. It is rare, but it is a correctness defect in a safety mechanism. The fix has a trap. Simply dropping unconsumed signals could turn a rare wrong-eject into a systematic missed-eject, and ADR-1208 exists because a missed review finding on a Queued member is invisible to operators.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A stale eject signal cannot act in a later batch (Priority: P1)

As an operator running the merge train, I want a review-eject signal raised during one trial to have no effect once that trial's worker has finished. Then no member is ejected from a later batch for a finding that no longer applies.

**Why this priority**: This is the reported defect. A member leaving a batch with no current cause is a silent, hard-to-diagnose failure.

**Independent Test**: In the sim bed, raise a pending-review-eject signal for a member during a trial whose worker ends through an early-return path. Then form a new batch containing the same issue number with no unresolved finding. The member must stay in the batch and be processed normally.

**Acceptance Scenarios**:

1. **Given** a signal raised for a member during a trial that then ends via an early return, **When** a later batch forms containing the same issue number and the member has no unresolved review finding, **Then** the member is not ejected and the batch proceeds.
2. **Given** the same setup, **When** the earlier trial ended through any of its return paths (red singleton reroute, green, runaway guard, assembly failure), **Then** the outcome is identical: the signal does not survive.

---

### User Story 2 - A real, unresolved finding on a Queued member is still caught (Priority: P1)

As an operator, I want a Queued member that has an unresolved review finding to still be ejected within one batch cycle, even if the signal for it was discarded along with a finished trial. Review feedback must never be silently lost.

**Why this priority**: This is the ADR-1208 guarantee. It is the failure the naive fix would introduce, so it is as important as the fix itself.

**Independent Test**: In the sim bed, make a signal for a member stale by ending a trial through an early return, with the member's review finding still unresolved and the member still Queued. Over the next poll and batch cycle, the member must be ejected to address the finding. Pair this with User Story 1: the same mechanism must keep the member in the batch when no finding exists. That shows the difference comes from finding staleness and not from ejecting unconditionally.

**Acceptance Scenarios**:

1. **Given** a member still Queued with an unresolved review finding and a pending signal dropped by an early-returning trial, **When** the next poll and batch cycle run, **Then** the member is ejected to address the finding within one batch cycle.
2. **Given** the paired scenario from User Story 1 (no unresolved finding), **When** the same cycle runs, **Then** the member is not ejected.

---

### User Story 3 - The guarantee holds for return paths added later (Priority: P2)

As a maintainer, I want the signal lifetime to be bounded by a rule that does not depend on each worker return path remembering to clean up. A future early return then cannot reintroduce the leak.

**Why this priority**: The defect arose because cleanup was left to each path. A fix that covers only today's paths would let it recur.

**Independent Test**: Review the chosen mechanism against the worker's structure. A new return path added without any signal-handling code must still leave no signal that outlives its trial, and a test that exercises a return path other than the red-singleton one must show the same result.

**Acceptance Scenarios**:

1. **Given** a worker return path that contains no signal-handling code, **When** the worker returns through it, **Then** no signal raised for that trial remains actionable afterwards.

---

### Edge Cases

- A signal raised after the worker's last checkpoint but before it returns.
- A signal raised for a member that the trial rerouted off the train in the same pass.
- The same issue number appearing in consecutive batches of the same partition.
- Two partitions for the same repo, one active and one starting. A signal for one partition's trial must not be dropped or consumed by the other's.
- A signal raised by the poll side concurrently with the worker finishing, so that it arrives just after the worker's cleanup.
- A daemon restart. In-memory signals are already lost on restart, and the guarantee must still hold because detection re-raises from live review state.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A pending-review-eject signal raised during a trial MUST NOT be actionable in any later trial.
- **FR-002**: A still-unresolved review finding on a member that is still Queued MUST be detected and acted on within one batch cycle even when its earlier signal was dropped or invalidated. The ADR-1208 guarantee MUST be preserved.
- **FR-003**: The lifetime bound MUST hold for every worker return path, including paths added after this change, without each path needing its own handling.
- **FR-004**: Discarding a stale signal MUST NOT cause an ejection, a count against the ejection limit, a pause, or a change to review-cycle counters.
- **FR-005**: The change MUST NOT alter `settleQueuedReviewFindings`'s detection logic, the eject mechanism itself, or the red-singleton disposition.
- **FR-006**: The behaviour MUST be covered by tests showing the stale-signal case failing against current `main` and passing with the change.
- **FR-007**: The ADR-1208 amendment and the as-built state-machine documentation MUST record the resolution, including the explicit choice between dropping and re-evaluating an unconsumed signal.

### Key Entities

- **Pending review-eject signal**: An in-memory, per-repo record, written by the poll goroutine and read by the merge-train worker at checkpoints. It asks the worker to eject a Queued member with unresolved review findings.
- **Trial**: One merge-train attempt, from batch formation to worker return. The signal's lifetime is bounded by it.
- **Queued member**: An issue waiting in the merge train's holding column. ADR-1208 guarantees its review feedback is seen within one batch cycle.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim bed, a signal raised during a trial that ends via an early return produces no ejection in a later batch containing the same issue number with no unresolved finding. The test fails against current `main` and passes with the change.
- **SC-002**: In the sim bed, a still-Queued member with an unresolved finding is ejected within one batch cycle after the same early-return trial. The test is paired with SC-001, so the two together show behaviour differing by signal staleness and not by ejecting in both cases.
- **SC-003**: The stale-signal test passes for each existing worker return path (red-singleton reroute, green, runaway guard, assembly failure). A return path with no signal-handling code also leaves no surviving signal.
- **SC-004**: The full existing test suite, including the existing review-eject tests, passes unchanged.

## Assumptions

- Dropping a stale signal is acceptable only because live detection re-raises the signal on the next poll while the finding is unresolved. The spec requires that property to be demonstrated (FR-002, User Story 2) and not just assumed. Which mechanism achieves it (drain on exit, stamp-and-expire, or re-derive at checkpoints) is left to Research and Plan, provided all requirements hold.
- Signals are in-memory only and need no durable migration.
- The probability of the defect is low, so the change should be small and not introduce new shared state.
- The ADR for this work is numbered after this issue (`adrs/1557-…`) and amends ADR-1208.

## Out of Scope *(optional)*

- `settleQueuedReviewFindings`'s detection logic.
- The eject mechanism itself (`ejectMember` / `rerouteQueuedMemberOffHolding`).
- The red-singleton disposition from #1545.
- Any redesign of how the poll goroutine and the worker coordinate beyond bounding the signal's lifetime.

## Source References *(optional)*

- ADR-1208 / #1208: `settleQueuedReviewFindings` and the pending-eject signal pattern
- ADR-1270: the dedicated settle-scan pattern
- #1545 / PR #1550: where the defect was found
- https://github.com/handarbeit/fabrik/pull/1550#issuecomment-5259938954
