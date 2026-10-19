# Feature Specification: Bound re-dispatches of one stage per episode, and don't resume a large session on an unchanged re-dispatch

**Feature Branch**: `fabrik/issue-2395`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description: "Bound re-dispatches of one stage per episode, and don't resume a large session on an unchanged re-dispatch.

> **Problem.** Nothing bounds how many times one stage of one item is dispatched when every run "succeeds" but nothing changes. `max_retries` counts only failures, and `max_turns` is per invocation, so a stage that completes, fails to record, and re-dispatches never reaches either limit. In #2388 one Research stage ran about 150 times, and its session's reported cumulative cost reached $202. The defaulted `resume_session: resume` resumed the same ever-growing session each time, so the per-run context cost rose with the run count. Mentioned for context: #1191, a cumulative budget across resumed retries.
>
> **Requirements.**
> - R1: A per-item, per-stage dispatch counter for one stage episode, reset when the stage completes durably, the item changes column, or a human comment resumes it. It counts every stage-run dispatch, whatever the outcome.
> - R2: At `max_stage_dispatches` (flag `--max-stage-dispatches`, env `FABRIK_MAX_STAGE_DISPATCHES`, YAML; default to be chosen in Plan, for example 10; 0 disables) the item is paused with `fabrik:paused` + `fabrik:awaiting-input` and one comment. The comment names the stage, the count and the last outcomes.
> - R3: The counter survives a restart. A restart, including a self-upgrade re-exec, must not reset it.
> - R4: A re-dispatch whose inputs are unchanged since the previous run does not resume the saved session when that session's measured context is above `resume_threshold_tokens`. Unchanged inputs means the same head SHA, issue body and comment set. The resume rule is the #2300 one; this extends its continuation exemption, which otherwise always resumes.
> - R5: Sim twin: a stage that completes but is never recorded stops at the cap and pauses. Neutralising the cap lets it run on.
>
> **Scope.** Stage dispatch bookkeeping and the #2300 resume decision. Cost accounting is out of scope: `CostUSD` is notional on subscription billing, and the dispatch count is the bound.
>
> **Acceptance.** A stage cannot be dispatched more than `max_stage_dispatches` times in one episode without a human. The counter persists across a restart (sim `RestartEnv`).
>
> Mention #2388 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) next to it."

## Background

Fabrik limits a stage's work in two ways today: `max_retries` counts *failed* runs, and `max_turns` bounds the turns inside *one* invocation. Neither bounds how many times a stage is dispatched when every run ends in apparent success but leaves nothing durable behind. In that shape a stage completes, the completion fails to be recorded, and the next poll dispatches the stage again. The loop is invisible to both limits.

Issue #2388 hit exactly this. One Research stage on one item ran about 150 times, and the cumulative cost reported for its session reached $202. Each re-dispatch resumed the same saved session, and by default (`resume_session: resume`) a closed or continuing session is resumed whatever its size. The context cost of every run therefore grew with the run count. Issue #2300 introduced a size-aware resume rule, but its continuation exemption always resumes, so it does not help when the same inputs are being re-dispatched.

Two defences are needed, both at the level of stage dispatch bookkeeping. One is a hard bound on dispatches per stage episode that ends in a pause for a human. The other stops a large session being resumed for a re-dispatch that has nothing new to act on. Cost accounting is deliberately not the bound: cost is notional on subscription billing, so the dispatch count is the limit. A cumulative budget across resumed retries is separate work, mentioned for context: #1191.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A runaway stage is stopped at a dispatch cap (Priority: P1)

An operator runs Fabrik unattended. A stage of an item completes each time it runs but the completion is never recorded, so the engine keeps dispatching it. After a bounded number of dispatches the item is paused and a human is told why, instead of the stage running for hours.

**Why this priority**: This is the failure that cost $202 in #2388. It is the only change that guarantees an upper bound whatever the cause of the loop.

**Independent Test**: In the sim bed, script a stage that always completes but never records, run polls until the cap, and observe the pause and the comment. Run it again with the cap neutralised and observe that it keeps dispatching.

**Acceptance Scenarios**:

1. **Given** `max_stage_dispatches` is N and a stage whose runs never record durable completion, **When** the stage has been dispatched N times in one episode, **Then** the item carries `fabrik:paused` and `fabrik:awaiting-input`, exactly one comment is posted, and no further dispatch occurs.
2. **Given** the item was paused by the cap, **When** a human comments, **Then** the item resumes and the dispatch count for that stage starts again from zero.
3. **Given** `max_stage_dispatches` is 0, **When** a stage is dispatched any number of times, **Then** the engine never pauses the item on account of the dispatch count.
4. **Given** the cap is neutralised in the sim, **When** the same never-recording stage runs, **Then** it continues to be dispatched past N.

---

### User Story 2 - The pause comment explains the loop (Priority: P1)

A human opening a paused item can see which stage looped, how many times it was dispatched, and what the recent runs ended in, so they can decide how to unblock it.

**Why this priority**: A pause with no diagnosis just moves the problem to a person. The comment is the evidence that the loop was seen.

**Independent Test**: Drive the cap in the sim and assert the comment text names the stage, the count and the last outcomes.

**Acceptance Scenarios**:

1. **Given** the cap fires on Research after N dispatches, **When** the comment is read, **Then** it names "Research", the number N, and the outcomes of the most recent dispatches.
2. **Given** the cap fires repeatedly across several episodes on one item, **When** each pause occurs, **Then** each posts one comment, and an already-paused item is not commented on again.

---

### User Story 3 - The dispatch count survives restarts (Priority: P1)

The engine can be restarted at any time, including by self-upgrade or SIGHUP re-exec. A stage that was looping before the restart must not get a fresh allowance after it.

**Why this priority**: Without persistence the bound is defeated by any restart, and a looping stage is likely to coincide with operator restarts.

**Independent Test**: In the sim, dispatch a looping stage part-way to the cap, restart the environment (`RestartEnv`), and verify the cap fires after the remaining dispatches, not after N more.

**Acceptance Scenarios**:

1. **Given** a stage has been dispatched k < N times in an episode, **When** the engine restarts, **Then** the next dispatches continue from k and the cap fires at N in total.
2. **Given** the engine restarts via the self-upgrade re-exec path, **When** the stage is dispatched again, **Then** the count is unchanged by the restart.

---

### User Story 4 - A re-dispatch with nothing new does not resume a large session (Priority: P2)

When a stage is re-dispatched and nothing it depends on has changed (same head SHA, issue body and comment set), resuming a big saved session only re-pays a large context for no new information. In that case the saved session is not resumed once its measured context exceeds `resume_threshold_tokens`. As built, this applies to re-entry after a *closed* run only (the last stage run emitted `FABRIK_STAGE_COMPLETE`, the #2388 shape); an unclosed session is a continuation (for example a time slice) and keeps resuming under #2300.

**Why this priority**: It limits the per-run cost growth within the cap. It is secondary to the cap, which is the hard bound, and it reuses the #2300 size measure.

**Independent Test**: In the sim, record a session whose context exceeds the threshold, re-dispatch the stage with unchanged inputs and check that the decision is "fresh". Change one input and check the decision is "resume" as before.

**Acceptance Scenarios**:

1. **Given** a saved session with measured context above `resume_threshold_tokens` and a re-dispatch with the same head SHA, issue body and comment set as the previous run, **When** the resume decision is made, **Then** the session is not resumed.
2. **Given** the same session but any one of the head SHA, issue body or comment set has changed, **When** the resume decision is made, **Then** the #2300 rule applies as before.
3. **Given** a saved session whose measured context is at or below the threshold, or unknown, **When** a re-dispatch has unchanged inputs, **Then** the session is resumed as it is today.
4. **Given** an unclosed (continuation) session whose measured context is above the threshold, **When** a re-dispatch has unchanged inputs, **Then** the session is still resumed under the #2300 continuation rule, so a time-sliced read-only stage that commits nothing can converge.

---

### Edge Cases

- A stage that is legitimately re-dispatched many times in one episode (for example time-sliced by `max_turns` or `max_wall_time`) counts every dispatch. The cap must be set high enough that ordinary slicing does not hit it, and operators can raise or disable it.
- Dispatches that never ran a worker (for example skipped for a usage-limit hold or an unresolved skill) are not stage-run dispatches and do not consume the allowance. Only dispatches that start a stage run count.
- A durable completion resets the counter. A column change resets it. A human comment that resumes the item resets it. A restart does not.
- The inputs of the previous run are unknown, for example after an upgrade from a build that did not record them. The inputs are then treated as changed and the session is resumed per the #2300 rule.
- The context size of the saved session is unknown. The existing #2300 rule applies (unknown size resumes), so the unchanged-input exemption never fires without a measured size.
- The item is already paused when the cap would fire. No second pause or comment is produced.
- A configured value that is negative or non-integer is refused with a clear error rather than silently defaulted.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The engine keeps a dispatch counter per item and per stage for the current stage episode. Every stage-run dispatch increments it, whatever the run's outcome (complete, incomplete, failed, time-sliced, or complete-but-unrecorded).
- **FR-002**: The counter is reset when the stage's completion is durably recorded, when the item changes column, or when a paused item is resumed (a human comment, or the pause labels being removed, both of which clear the engine pause record; a paused dispatch record that meets an unpaused item starts a new episode).
- **FR-003**: `max_stage_dispatches` sets the per-episode cap. It is configurable by flag `--max-stage-dispatches`, environment variable `FABRIK_MAX_STAGE_DISPATCHES`, and a YAML config key. A value of 0 disables the cap. Negative or non-integer values are refused.
- **FR-004**: The default value of `max_stage_dispatches` is a documented positive number, chosen to sit above the dispatch count of ordinary healthy episodes (including time-sliced ones). It was fixed during Plan at 25: a healthy episode can legitimately reach about 14 dispatches (`max_slice_retries` slices plus `max_retries` failures plus the final run), so the working figure of 10 would pause it. Operators can raise the value or set 0 (ADR 2395).
- **FR-005**: When an item's counter reaches `max_stage_dispatches`, the engine pauses the item with `fabrik:paused` and `fabrik:awaiting-input` and posts one comment. It dispatches no further run of that stage for the item until a human resumes it.
- **FR-006**: The pause comment names the stage, the dispatch count, and the outcomes of the most recent dispatches, and tells the human how to resume.
- **FR-007**: The pause comment is posted at most once per pause. An item that is already paused is not paused or commented on again by the cap.
- **FR-008**: The dispatch counter persists across engine restarts, including the self-upgrade re-exec and SIGHUP restart. A restart does not reset it, and a restart does not cause a dispatch to be double-counted or lost.
- **FR-009**: The session resume decision treats a stage re-dispatch as "unchanged" when the head SHA, the issue body and the comment set are the same as at the previous run of that stage on that item.
- **FR-010**: For an unchanged re-dispatch whose saved session has measured context above `resume_threshold_tokens`, the engine does not resume the saved session and starts fresh. This applies only when the saved session's last stage run was closed (it emitted `FABRIK_STAGE_COMPLETE`); a session whose last stage run was not closed is a continuation under #2300 and still resumes (ADR 2395).
- **FR-011**: When the inputs have changed, when the previous run's inputs are unknown, when the saved session is an unclosed continuation, or when the saved session's measured context is at or below the threshold or unknown, the resume decision is exactly the #2300 decision.
- **FR-012**: The resume-decision log line for a fresh start caused by FR-010 states that the cause is an unchanged re-dispatch with an oversized session.
- **FR-013**: The new cap and the unchanged-input exemption each have a neutralisation seam usable from the sim bed, so the sim twin can show the behaviour with and without them.
- **FR-014**: A sim twin covers a stage that completes but is never recorded: with the cap active it stops at the cap and pauses, and with the cap neutralised it runs past the cap. A second sim scenario covers persistence across `RestartEnv`.
- **FR-015**: The new setting is documented in the user guide and the as-built state-machine and stage-lifecycle documents, together with the regenerated documentation bundle.

### Key Entities *(if applicable)*

- **Stage episode**: The span in which one stage of one item is being worked toward durable completion. It begins when the item enters the stage or is resumed by a human, and ends at durable completion or a column change.
- **Dispatch counter**: The number of stage-run dispatches in the current episode for one item and stage, with the recent outcomes kept for the pause comment.
- **Run inputs**: The head SHA, issue body and comment set that a stage run saw, recorded so the next dispatch can tell whether anything changed.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A stage of an item is dispatched at most `max_stage_dispatches` times in one episode without a human acting, whatever the outcome of each run.
- **SC-002**: A looping scenario such as #2388's stops after the configured number of dispatches (not about 150) and leaves the item paused with an explanatory comment.
- **SC-003**: After a restart part-way through an episode, the total dispatches before the pause still equal the configured cap.
- **SC-004**: For a re-dispatch with unchanged inputs and an oversized saved session, the context carried into the run does not grow with the run count, because the session is not resumed.
- **SC-005**: With `max_stage_dispatches` set to 0 and no unchanged-input trigger, engine behaviour is identical to before this change.

## Assumptions

- The default cap is 25, chosen in Plan against the dispatch counts of healthy episodes including time-sliced ones (the specification's working figure was 10).
- Only dispatches that start a stage run count. Dispatches skipped before a worker is spawned (usage-limit hold, unresolved skill, and similar) do not.
- "Comment set" means the set of comments on the item as seen by the previous run, so a new human or bot comment makes the inputs "changed".
- The measured session context and `resume_threshold_tokens` are those introduced by #2300; this issue adds no new size measurement or threshold setting.
- A human comment resumes a paused item per the existing rule (a human comment at or after the pause), and that resume resets the counter. Removing the pause labels by hand also resets it, because the engine pause record is cleared on any unpause.
- Pausing uses the engine's existing pause pair and comment-deduplication behaviour; no new label is introduced.

## Out of Scope *(optional)*

- Cost accounting or a cumulative cost or budget bound across resumed retries (mentioned for context: #1191). Cost is notional on subscription billing, and the dispatch count is the bound.
- Diagnosing or fixing the root cause of a stage completing without being recorded (the #2388 defect itself). This issue only bounds the damage.
- Changing the `max_retries` or `max_turns` semantics.
- Changing the #2300 resume rule for runs whose inputs have changed.
- Bounding comment-review or reinvoke dispatches other than stage runs, beyond what existing breakers already do.

## Source References *(optional)*

- #2388 — the incident: one Research stage ran about 150 times and reported $202 cumulative session cost.
- #2300 — the session resume decision (`resume_session`, `resume_threshold_tokens`, continuation exemption) that R4 extends.
- #1191 — cumulative budget across resumed retries (mentioned for context).
