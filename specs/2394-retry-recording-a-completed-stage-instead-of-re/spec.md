# Feature Specification: Retry recording a completed stage instead of re-running it

**Feature Branch**: `fabrik/issue-2394`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "## Problem

A stage run that finished, emitting `FABRIK_STAGE_COMPLETE`, is run again whenever its completion could not be written to GitHub. `handleStageComplete` logs `[done] stage \"X\" complete`, but `could not add completion label` leaves only that log line. The Store and GitHub both still show the stage as not complete, so the next dispatch re-invokes Claude on the same inputs, and the stage comment and the push are lost the same way.

In #2388 this repeated about 150 times over 21 hours while the forge was unreachable. Each repeat resumed the same session, which had grown to about 13 MB, so every \"1-turn\" run re-read the whole transcript.

## Requirements

- **R1:** A finished stage run's outcome is recorded locally and durably **before** any GitHub write: the stage, the head SHA, the stage output, and the marker results such as an issue update or a spawn. It goes under `.fabrik/state/`, written atomically, and survives a restart.
- **R2:** While a recorded outcome is unpublished, the engine does not dispatch that stage again. Each poll it retries the publication instead: the stage comment, the push, the completion label (or `fabrik:awaiting-ci`) and the issue-body update. Each step is idempotent, and the record is removed once every step has landed.
- **R3:** If the worktree head no longer matches the recorded SHA, or the item has moved columns or been paused, the record is discarded and logged, and normal dispatch resumes.
- **R4:** Publication retries are bounded the way the other settle scans are (`MaxRetries`, then escalate with `fabrik:paused` and a comment). Transport errors during an account-wide forge-unreachable hold do not count against that bound.
- **R5:** Sim twin: a completed run whose completion-label write fails is published on a later poll with no second `ClaudeInvoker` call. Neutralising the change makes it re-invoke.

## Scope

Stage dispatch outcomes. Comment-review outcomes are in scope only if the same loss happens there; the audit is part of the work. Capping re-dispatches and the forge-unreachable dispatch hold are separate issues.

## Acceptance

- A stage that completes while GitHub writes fail runs exactly once and is recorded when GitHub comes back (sim).
- A restart between completion and publication still publishes without re-running.

Mention #2388 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) next to it."

## Background

When a stage's Claude run finishes and emits its completion marker, the engine must then publish that outcome to GitHub: the stage comment, the branch push, the completion label (or `fabrik:awaiting-ci`) and, where the stage produced one, the issue-body update. Today the only record that the run finished is the engine's own log line. If any of those writes fails, for example because the forge is unreachable, nothing durable says the work was done. GitHub and the in-memory item state both still show the stage as not complete, so the next poll treats the stage as never having run and dispatches Claude again on the same inputs. The comment and the push are lost in the same way. The "could not add completion label" warning appears at several completion sites in the engine, not just one.

The observed cost was severe. In #2388 this repeated about 150 times over 21 hours while the forge was unreachable. Each repeat resumed the same saved session, which had grown to about 13 MB, so even a one-turn run re-read the whole transcript. That is wasted model spend and time, and the lost comment and push mean the real output of the stage never reached the board. The fix is to remember locally that a stage finished, and to retry publishing it instead of re-running it.

This issue follows #2393 (the lock-write refusal and forge-unreachable hold), which stops new work from starting while the forge is unreachable. This issue addresses the other half: work that already finished must not be redone or lost. Capping the number of re-dispatches is a separate issue.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A finished stage is published, not re-run (Priority: P1)

An operator has a stage that completes while GitHub writes are failing (the completion-label write, comment or push errors out). When GitHub becomes writable again, the engine publishes the stage's already-produced outcome, without calling Claude a second time.

**Why this priority**: This is the defect that produced the 150 repeated runs in #2388. Everything else supports it.

**Independent Test**: In the sim, script one stage run that emits the completion marker, make the completion-label write fail for some polls, then let it succeed. Count `ClaudeInvoker` calls for that stage.

**Acceptance Scenarios**:

1. **Given** a stage run finished and its completion-label write failed, **When** later polls occur, **Then** the stage is not dispatched again and the engine retries publication each poll.
2. **Given** the writes start succeeding, **When** the next publication retry runs, **Then** the stage comment, push, completion label (or `fabrik:awaiting-ci`) and any issue-body update are all published and the item advances exactly as if the first attempt had succeeded.
3. **Given** the same scenario with this change neutralised, **When** the label write fails, **Then** the stage is re-invoked, proving the test detects the defect.

---

### User Story 2 - Publication survives a restart (Priority: P1)

An operator restarts the daemon between a stage finishing and its outcome being published. After the restart the outcome is still published, and the stage is not re-run.

**Why this priority**: A purely in-memory record would reproduce the problem on every restart, including the SIGHUP and self-upgrade restarts the engine performs routinely.

**Independent Test**: In the sim, finish a stage with publication failing, restart the engine (`RestartEnv`), then let writes succeed and count `ClaudeInvoker` calls.

**Acceptance Scenarios**:

1. **Given** a recorded, unpublished outcome and a daemon restart, **When** the first polls after restart run, **Then** the outcome is published and Claude is not invoked for that stage.

---

### User Story 3 - A stale record never blocks real work (Priority: P2)

If the world has changed since the outcome was recorded, publishing it would be wrong, so the record is dropped and the item proceeds normally.

**Why this priority**: The record must be safe. A record that outlives its relevance would wedge an item.

**Independent Test**: Record an unpublished outcome, then change the worktree head, move the item to another column, or pause it, and observe the next dispatch.

**Acceptance Scenarios**:

1. **Given** a recorded outcome, **When** the worktree head no longer matches the recorded SHA, **Then** the record is discarded with a log line and the stage can be dispatched normally.
2. **Given** a recorded outcome, **When** the item has moved to a different column, **Then** the record is discarded with a log line and normal dispatch resumes.
3. **Given** a recorded outcome, **When** the item is paused, **Then** the record is discarded with a log line and normal dispatch resumes.

---

### User Story 4 - Retries are bounded and escalate (Priority: P2)

A publication that keeps failing for a reason other than an outage does not retry forever. It escalates like the other settle scans do.

**Why this priority**: An unbounded retry would hide a persistent fault, such as a missing permission, that a human needs to fix.

**Independent Test**: Fail the publication writes persistently with a non-transport error and count polls until the item is paused; repeat with transport errors under a forge-unreachable hold.

**Acceptance Scenarios**:

1. **Given** publication fails repeatedly with errors that are not transport errors, **When** `MaxRetries` attempts have been used, **Then** the item is paused with `fabrik:paused` and a comment, and the record no longer triggers retries.
2. **Given** an account-wide forge-unreachable hold is in effect, **When** publication fails with transport errors, **Then** those failures do not count toward the `MaxRetries` bound.

---

### Edge Cases

- Publication partly succeeds (the comment posts but the label write fails): the next retry must not duplicate the comment or the push; each step is idempotent and already-landed steps are skipped.
- The recorded outcome includes marker results (an issue update, a spawn): these are published from the record, not re-derived by re-running Claude.
- The record file is corrupt or unreadable after a crash: it is treated as absent and logged, so the item falls back to today's behaviour rather than wedging.
- The stage's `FABRIK_STAGE_COMPLETE` is accompanied by `FABRIK_NO_WORK_NEEDED`: the no-work path has its own durable marker today (`fabrik:awaiting-done`); the record must not conflict with it.
- Two completion paths exist for stages that wait for CI (completion label deferred, `fabrik:awaiting-ci` applied instead): publication applies whichever the stage's configuration calls for.
- The item is closed or reaches Done by other means while a record exists: the record is discarded like any other stale record.
- A run that did not complete (no marker, turn cap, error) leaves no record; its existing retry behaviour is unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a stage run finishes with a completion marker, the engine MUST record the outcome locally and durably before making any GitHub write for it. The record holds the stage, the worktree head SHA, the stage output, and the results of any markers in the output (such as an issue update or a spawn).
- **FR-002**: The record MUST be stored under `.fabrik/state/`, written atomically, and MUST survive a daemon restart.
- **FR-003**: While a recorded outcome for an item and stage is unpublished, the engine MUST NOT dispatch that stage for that item again.
- **FR-004**: While a record is unpublished, the engine MUST retry its publication on each poll. Publication covers the stage comment, the branch push, the completion label (or `fabrik:awaiting-ci` for a stage that waits for CI) and the issue-body update.
- **FR-005**: Each publication step MUST be idempotent, so that a retry after a partial publication does not duplicate comments or pushes, and the record MUST be removed once every step has landed.
- **FR-006**: If the worktree head no longer matches the recorded SHA, the engine MUST discard the record, log that it did so and why, and resume normal dispatch.
- **FR-007**: If the item has moved to a different column or has been paused, the engine MUST discard the record, log that it did so and why, and resume normal dispatch.
- **FR-008**: Publication retries MUST be bounded by `MaxRetries` in the same way as the other settle scans. When the bound is reached, the engine MUST pause the item with `fabrik:paused` and post an explanatory comment.
- **FR-009**: Transport errors that occur while an account-wide forge-unreachable hold is in effect MUST NOT count against the `MaxRetries` bound.
- **FR-010**: A corrupt or unreadable record MUST be treated as absent, logged, and MUST NOT block dispatch.
- **FR-011**: The work MUST include an audit of the comment-review outcome paths for the same loss (a finished pass whose results cannot be written to GitHub and is then repeated). If the same loss exists there, those paths MUST get the same protection; if it does not, the audit's finding MUST be recorded with the change.
- **FR-012**: A sim scenario MUST show that a completed run whose completion-label write fails is published on a later poll with no second `ClaudeInvoker` call, and that neutralising the change makes the stage re-invoke.
- **FR-013**: A sim scenario MUST show that a restart between completion and publication still publishes the outcome without re-running the stage.
- **FR-014**: The change MUST NOT alter behaviour for runs that did not complete, or for runs whose first publication attempt succeeds.

### Key Entities *(if applicable)*

- **Recorded stage outcome**: The durable local record of a finished stage run that has not yet been fully published. It identifies the item, stage and head SHA, and carries the stage output and marker results needed to publish without re-running Claude. It exists from the moment the run finishes until every publication step has landed or the record is discarded.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A stage that completes while GitHub writes fail makes exactly one Claude invocation, regardless of how many polls elapse before GitHub accepts writes.
- **SC-002**: After GitHub writes recover, the stage's comment, push and completion state are all present on GitHub within one successful publication retry, with no duplicates.
- **SC-003**: A daemon restart between completion and publication results in zero additional Claude invocations for that stage and the outcome is published.
- **SC-004**: A discarded record (head moved, column changed, item paused) results in a logged reason and the stage being dispatchable on the next poll.
- **SC-005**: A persistently failing publication escalates to a paused item after `MaxRetries` non-outage failures; outage-time transport failures add nothing to that count.
- **SC-006**: The sim test for SC-001 fails (the stage is re-invoked) when the change is neutralised.

## Assumptions

- The record is per item and stage; at most one unpublished record exists per item at a time, because the stage cannot advance until it is published.
- The local state directory `.fabrik/state/` is the right home for the record, consistent with the engine's other durable local state (for example merge-train runs and decision state).
- Discarding a record on a pause, column move or head mismatch is acceptable even though the finished work is then redone, because publishing a stale outcome would be wrong; these are rare relative to outage-driven failures.
- The existing retry budget (`MaxRetries`) and its escalation wording are reused rather than a new bound being introduced.
- Whether the forge-unreachable hold is "in effect" is decided by the engine's existing account-wide hold from #2393; this issue consumes that signal and does not define it.

## Out of Scope *(optional)*

- Capping the number of re-dispatches of a stage in general (a separate issue).
- The forge-unreachable dispatch hold itself (delivered by #2393).
- Shrinking or rotating oversized resumed sessions (the 13 MB transcript is a symptom here, not the subject).
- Comment-review outcomes, except where the audit under FR-011 finds the same loss.
- Changing what a stage publishes or in what order, beyond making each step retryable.

## Source References *(optional)*

- Issue #2388: the incident in which one completed stage was re-run about 150 times over 21 hours.
- Issue #2393: the lock-write refusal and forge-unreachable hold that this work complements.
