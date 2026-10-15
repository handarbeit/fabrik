# Feature Specification: Merge gate: bound an unknown merge state on its own short clock, and make the CI-timeout pause name the gate that actually held the item

**Feature Branch**: `fabrik/issue-2302`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: the original issue body, verbatim, quoted below.

> ## Problem
>
> #2096 (v0.0.84) fixed the re-check half of #1809. A merge-gate claim now records a `merge-unsettled` cooldown at `mergeGateRecheckInterval()` = PollSeconds/2 (`engine/catch_up_handlers.go:545-566`, `engine/retry_backoff.go:55-62`), so a transient `mergeable=null` is re-read within a poll instead of waiting out the periodic re-evaluation. #2096's spec explicitly left two things out of scope: "giving `mergeable=null` its own escalation bound, or naming the blocker in the CI-timeout pause (#1809)". The ADR records the same (`adrs/2096-cooldown-consumed-on-admission.md`: "a `mergeable=null` escalation bound is #1809 and out of scope"). Both are still missing in v0.0.85.
>
> **(a) No bound of its own for an unknown merge state.**
> - `settlePRMergeState` returns `PRMergeUnsettled` for `mergeable == nil` ("mergeable=null (GitHub computing)") and for `mergeable_state == "unknown"` (`engine/pr_settle.go:168-175`).
> - `checkMergeabilityGate` claims every `PRMergeUnsettled` (`engine/merge_gate.go:41-47`), and `handleMergeAndCIGates` returns before `checkCIGate` ("merge gate blocked, checkCIGate not reached").
> - The only bound on that hold is the `CIBackstopTimeout` backstop in `settleAwaitingCIScan` (`engine/ci_settle.go:239-264`, default 4h). It applies only to items carrying `fabrik:awaiting-ci`.
> - An item held by the merge gate *without* `fabrik:awaiting-ci`, such as a Validate-complete item at the landing decision (the #1943 shape noted on this report), has no bound at all.
> - `PRMergeUnsettled` also covers states that are legitimately long (CI checks pending, a suite hold, a pending required context). So "GitHub has not computed mergeability" shares the CI wait's 4h budget, when it is a state GitHub resolves on request within seconds to minutes.
>
> **(b) The escalation blames CI.** When the backstop fires "while never reaching the CI gate" (`engine/ci_settle.go:261-262`), it calls `pauseForCITimeout` (`engine/ci.go:733-753`). That posts "The CI gate for stage **X** timed out waiting for checks to pass… Please check the PR's CI status, address any failures…". The only extra detail it can add is an outstanding check suite (`suiteTimeoutNote`). In the reported incident (v0.0.81) all 20 checks had been green for 5h25m and the PR was `MERGEABLE/CLEAN` a minute later. The comment sent the reader looking for CI failures that did not exist.
>
> The engine already records what held the item. `noteGateHold(item, gateMerge, settle.Reason, nil)` (`engine/catch_up_handlers.go:552`, `engine/channel_hooks.go:258-282`) stores an `itemstate.GateHold{Gate, Reason, Since, ConfirmedAt}` on every merge-gate claim. Nothing in the escalation reads it.
>
> ## Requirements
>
> **R1. Distinguish "merge state unknown".** The merge-gate claim must be able to tell an *unknown merge state* (`mergeable == nil`, or `mergeable_state == "unknown"`) from every other `PRMergeUnsettled` reason. A `FetchPRMergeableFields` read error is not "unknown merge state". It stays on today's handling.
>
> **R2. Its own short bound.** An item whose PR head has been continuously in an unknown merge state is escalated once that state has lasted a new, shorter bound:
> - The bound is a named setting with flag/env/YAML resolution like the other timeouts. The default is on the order of tens of minutes, well below `CIBackstopTimeout`. Plan picks the value and the name. A new `FABRIK_*` variable is classified in `internal/workerenv`.
> - The clock starts at the first observation of the unknown state for that PR head SHA.
> - The clock resets when GitHub returns a computed state, or when the head changes.
> - It is in memory. After a daemon restart it starts again, as a lower bound; it never fires blind on the first evaluation after a start, following ADR-2059's precedent.
>
> **R3. The bound applies wherever the merge gate claims.** R2 applies whether or not the item carries `fabrik:awaiting-ci`, and so covers the Validate-complete, merge-gate-held item. It does not change `CIBackstopTimeout` or the CI liveness dwell for any other `PRMergeUnsettled` reason.
>
> **R4. Escalation for R2 names what was observed.** On R2's bound the item is paused through the existing pause pair (`pauseIssue`, `fabrik:paused` + `fabrik:awaiting-input`) with its own comment. The comment states:
> - that GitHub has not reported a computed mergeability for PR #N at head `<sha>` for `<duration>`, and the value it kept returning (`mergeable=null` / `mergeable_state=unknown`);
> - that the CI gate was never reached because of it;
> - the check-run state at escalation (for example "20 of 20 checks passed").
>
> The comment must not tell the reader to address CI failures when none failed. The pause is deduplicated per episode, as `hasCIGatePauseComment` does for the CI pauses (#1408). A human comment or label removal resumes the item as for any pause.
>
> **R5. The `CIBackstopTimeout` escalation names the actual blocker.** When `settleAwaitingCIScan`'s backstop fires for an item that never reached the CI gate, its comment names the gate that held the item and that gate's reason and duration, taken from the recorded `GateHold`. Example: "the merge gate held this item for 4h: CI checks pending". It also gives the check-run state at escalation. When the recorded hold is not the CI gate and no check failed, the comment does not say the wait was for checks to pass and does not ask for failures to be addressed. When the CI gate *was* reached, today's text stays. Episode dedup (`hasCIGatePauseComment`) keeps recognising the comment, and #1408's reapply-without-repost behaviour is unchanged.
>
> **R6. Observability.** The R2 escalation sets the attention `cause` and the channel event detail to the unknown-merge-state cause, so overseer tools do not report it as a CI timeout. Plan chooses the vocabulary entry in line with ADR 2121. The log line names the bound and the elapsed time.
>
> ## Scope
>
> In scope: R1–R6, tests and docs.
>
> Out of scope:
> - the #2096 re-check cadence (already shipped);
> - bounding `PRMergeUnsettled` "CI checks pending" holds on items without `fabrik:awaiting-ci` (#1943's broader case);
> - applying `fabrik:awaiting-ci` on merge-gate claims (excluded by #2096 as well);
> - the CI liveness dwell (#1410);
> - `PRMergeQueued` holds;
> - merge-train landing waits (#2297).
>
> Mention #1809 and #1943 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1809 or #1943. This issue's own number gets `Closes`.
>
> ## Acceptance
>
> - **Unit (`engine`).**
>   - The unknown-merge-state clock starts on `mergeable=null` and on `mergeable_state=unknown`.
>   - The clock resets on a computed state and on a head change.
>   - The clock is not started by a `FetchPRMergeableFields` error or by "CI checks pending".
>   - The pause fires once at the bound, not on the first evaluation after a start, and is not reposted.
>   - The `CIBackstopTimeout` comment names the merge gate and its reason when the CI gate was never reached, and keeps today's text when it was.
> - **Sim twins (`tests/sim`).** Extend `merge_gate_recheck_test.go` or `ci_backstop_resume_test.go`:
>   - A Validate item with all checks green, whose PR returns `mergeable=null` on every read, is paused after the new bound (not after `CIBackstopTimeout`). The comment names `mergeable=null` and the green checks.
>   - The same with `mergeable=null` clearing to `clean` before the bound: the item proceeds and nothing is paused.
>   - A Validate-complete yolo item without `fabrik:awaiting-ci` under a persistent null is also bounded.
>   - An item held by "CI checks pending" past `CIBackstopTimeout` gets a comment naming the merge gate's real reason.
> - **Non-vacuity.** Each new test fails with the fix neutralised: the unknown-merge-state bound disabled, and the backstop comment reverted to the fixed CI text. Record this in the PR.
> - **Research note.** Confirm that the merge gate's re-read of a null `mergeable` reaches GitHub, and is not answered from the #1952 conditional-request cache with a stale null. If it can be, the bound must account for it.
> - **Docs.** Update `docs/state-machine.md` §6.4 (settling primitive: the unknown-merge-state sub-reason), §6.5 / §6.14.6 (the CIBackstopTimeout backstop's comment content) and the admission / gate-hold description. Regenerate `docs/llms-full.txt` in the same commit. Add the new setting to `docs/USER_GUIDE.md`, then regenerate the bundle.

## Background

Issue #2096 (v0.0.84) fixed how quickly the merge gate re-checks a PR whose mergeability GitHub has not yet computed: the item is now re-read within about half a poll interval instead of waiting out the periodic re-evaluation. It deliberately left two gaps, both still present in v0.0.85:

1. **An unknown merge state has no bound of its own.** GitHub reports a PR's mergeability as null, or its merge state as `unknown`, while it computes it. This normally resolves on request within seconds to minutes. The merge gate holds the item for as long as the state is unknown, and the only bound on that hold is the four-hour CI-wait backstop. That backstop covers only items carrying `fabrik:awaiting-ci`. An item held by the merge gate without that label, such as a Validate-complete item at the landing decision, has no bound at all. The same "unsettled" verdict also covers waits that are legitimately long (checks pending, a check-suite hold, a pending required context), so a state GitHub resolves quickly shares a budget sized for CI.
2. **The escalation blames CI.** When the CI-wait backstop fires for an item that never reached the CI gate, the pause comment says the CI gate timed out waiting for checks and asks the reader to address CI failures. In the reported incident (v0.0.81) all 20 checks had been green for 5h25m and the PR became mergeable and clean a minute later, so the comment sent the reader looking for failures that did not exist. The engine already records which gate held the item, why, and since when. Nothing in the escalation reads it.

This issue closes both gaps. It is a follow-up to #1809 and #1943, which are referenced for context only.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An unknown merge state is escalated on its own short clock (Priority: P1)

An operator has an item at the merge gate whose PR GitHub keeps reporting with no computed mergeability. Whether or not the item carries `fabrik:awaiting-ci`, the operator wants it to stop silently waiting after a short, configurable time. They want a pause that says what happened, not a four-hour (or unbounded) wait.

**Why this priority**: This is the core defect. A Validate-complete item under a persistent unknown merge state currently has no bound at all, so it can stall indefinitely with no signal.

**Independent Test**: In the sim, run a Validate item with all checks green whose PR returns a null mergeability on every read. Advance the clock past the new bound but well short of the CI-wait backstop, and confirm the item is paused with a comment naming the unknown value and the green checks.

**Acceptance Scenarios**:

1. **Given** an item at the merge gate whose PR returns a null mergeability on every read, **When** the unknown state has lasted the new bound for that PR head, **Then** the item is paused with `fabrik:paused` + `fabrik:awaiting-input` and a comment stating that GitHub has not reported a computed mergeability for the PR at that head for the elapsed duration, the value it kept returning, that the CI gate was never reached because of it, and the check-run state at escalation (for example "20 of 20 checks passed").
2. **Given** the same item, **When** the null mergeability clears to a computed state before the bound, **Then** the item proceeds through the gate and nothing is paused.
3. **Given** a Validate-complete yolo item without `fabrik:awaiting-ci` under a persistent null mergeability, **When** the bound elapses, **Then** it is paused as in scenario 1.
4. **Given** a PR whose merge state is `unknown` (rather than a null mergeability), **When** that lasts the bound, **Then** it is escalated the same way and the comment names `mergeable_state=unknown`.
5. **Given** the item is already paused for this episode, **When** later evaluations run, **Then** the pause comment is not reposted.
6. **Given** a human comments or removes the pause label, **When** the item resumes, **Then** it resumes as for any other pause.

---

### User Story 2 - The CI-wait timeout pause names the gate that actually held the item (Priority: P2)

An operator reads the pause comment after the four-hour CI-wait backstop fires. When the item never reached the CI gate, they want the comment to say which gate held it, why, and for how long, rather than telling them to look for CI failures that do not exist.

**Why this priority**: The misleading comment wastes operator time, but it only appears after the long backstop. The state it describes is already visible to the engine.

**Independent Test**: In the sim, hold an item at the merge gate for "CI checks pending" past the CI-wait backstop. Confirm the comment names the merge gate and that reason, gives the check-run state, and neither claims the wait was for checks to pass nor asks for failures to be addressed.

**Acceptance Scenarios**:

1. **Given** the backstop fires for an item whose recorded hold is the merge gate with reason "CI checks pending", **When** the pause comment is posted, **Then** it names the merge gate, that reason, the hold duration (for example "the merge gate held this item for 4h: CI checks pending") and the check-run state at escalation.
2. **Given** the recorded hold is not the CI gate and no check failed, **When** the comment is posted, **Then** it does not say the wait was for checks to pass and does not ask the reader to address failures.
3. **Given** the backstop fires for an item that did reach the CI gate, **When** the comment is posted, **Then** today's text is used unchanged.
4. **Given** a pause comment of either form has been posted for the episode, **When** the engine re-evaluates, **Then** it recognises the comment as the episode's pause (#1408 dedup) and the reapply-without-repost behaviour is unchanged.

---

### User Story 3 - Overseer tools report the right cause (Priority: P3)

An overseer reading attention state or channel events wants to see that an item was paused because GitHub never computed mergeability, not because CI timed out, and wants the daemon log to show the bound and elapsed time.

**Why this priority**: Observability only. The behaviour in Story 1 is complete without it, but a wrong cause would mislead anyone relying on those tools.

**Independent Test**: Trigger the Story 1 escalation and inspect the attention `cause`, the channel event detail and the log line.

**Acceptance Scenarios**:

1. **Given** the unknown-merge-state escalation fires, **When** the attention classifier and channel events are read, **Then** the cause is a distinct unknown-merge-state cause and not a CI-timeout cause.
2. **Given** the escalation fires, **When** the daemon log is read, **Then** a line names the configured bound and the elapsed time.

---

### Edge Cases

- A `FetchPRMergeableFields` read error is not an unknown merge state. It does not start or advance the clock, and it keeps today's handling.
- A "CI checks pending", suite-hold or pending-required-context wait does not start the clock, even though it shares the unsettled verdict.
- A new push or any other head change resets the clock. The next unknown observation starts a fresh one for the new head.
- A computed state (clean, dirty, blocked and so on) resets the clock. A later return to unknown starts a new clock.
- After a daemon restart the clock starts again from the first observation. It never fires on the first evaluation after a start, so the effective wait can be longer than the bound but never shorter.
- If the re-read of a null mergeability can be answered from the #1952 conditional-request cache with a stale null, the bound must account for it rather than count cached answers as fresh observations.
- If both the new bound and the CI-wait backstop could apply to an item, the shorter unknown-merge-state bound fires first and the item is paused once for the episode.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The merge-gate claim distinguishes an unknown merge state (`mergeable` null, or `mergeable_state` equal to `unknown`) from every other unsettled reason.
- **FR-002**: A failed read of the PR's mergeable fields is never treated as an unknown merge state and keeps today's handling.
- **FR-003**: An item whose PR head has been continuously in an unknown merge state is escalated once that state has lasted a new bound, exposed as a named setting with flag, environment and YAML resolution like the other timeouts. Its default is on the order of tens of minutes and well below the CI-wait backstop.
- **FR-004**: A new `FABRIK_*` variable introduced for the setting is classified in the worker-environment classification.
- **FR-005**: The unknown-merge-state clock starts at the first observation of the unknown state for a given PR head SHA.
- **FR-006**: The clock resets when GitHub returns a computed state, and when the PR head changes.
- **FR-007**: The clock is held in memory only. After a restart it starts again from the first observation, and the escalation never fires on the first evaluation after a start.
- **FR-008**: The bound applies wherever the merge gate claims an item, whether or not the item carries `fabrik:awaiting-ci`, including a Validate-complete item at the landing decision.
- **FR-009**: The new bound changes nothing about the CI-wait backstop or the CI liveness dwell for any other unsettled reason.
- **FR-010**: When the bound is reached, the item is paused through the existing pause pair (`fabrik:paused` + `fabrik:awaiting-input`) with a comment stating:
  - that GitHub has not reported a computed mergeability for the PR at its head SHA for the elapsed duration;
  - the value it kept returning (`mergeable=null` or `mergeable_state=unknown`);
  - that the CI gate was never reached because of it;
  - the check-run state at escalation.
- **FR-011**: That comment does not tell the reader to address CI failures when none failed.
- **FR-012**: The unknown-merge-state pause is deduplicated per episode in the same way as the CI pauses (#1408), and a human comment or label removal resumes the item as for any other pause.
- **FR-013**: When the CI-wait backstop fires for an item that never reached the CI gate, the pause comment names the gate that held the item, that gate's reason and the hold duration, taken from the recorded gate hold, and gives the check-run state at escalation.
- **FR-014**: When the recorded hold is not the CI gate and no check failed, the backstop comment does not say the wait was for checks to pass and does not ask for failures to be addressed.
- **FR-015**: When the CI gate was reached, the backstop comment keeps today's text.
- **FR-016**: Episode deduplication continues to recognise both forms of the backstop comment, and the reapply-without-repost behaviour of #1408 is unchanged.
- **FR-017**: The unknown-merge-state escalation sets the attention cause and the channel event detail to a distinct unknown-merge-state cause, chosen in line with the existing cause vocabulary, and not to a CI-timeout cause.
- **FR-018**: The escalation log line names the configured bound and the elapsed time.
- **FR-019**: The research stage confirms whether the merge gate's re-read of a null `mergeable` reaches GitHub or can be answered from the #1952 conditional-request cache with a stale null, and the bound accounts for either outcome.

### Key Entities

- **Unknown merge state clock**: In-memory record, per item and PR head SHA, of when an unknown merge state was first observed. It is reset by a computed state or a head change.
- **Gate hold**: The engine's existing record of which gate held an item, why, since when, and when it was last confirmed. The backstop comment reads it.
- **Unknown-merge-state bound**: The new named setting, a duration shorter than the CI-wait backstop.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An item at the merge gate under a persistent unknown merge state is paused no later than the new bound (plus one evaluation interval). That holds with and without `fabrik:awaiting-ci`, and the wait is never governed by the four-hour CI-wait backstop.
- **SC-002**: An unknown merge state that clears before the bound results in no pause and no comment.
- **SC-003**: Neither a read error nor a "CI checks pending" wait ever starts the unknown-merge-state clock.
- **SC-004**: In every backstop pause for an item that never reached the CI gate, the comment names the holding gate and its reason. When no check failed, it contains no instruction to address CI failures and no claim that the wait was for checks to pass.
- **SC-005**: A reader of the pause comment can see the observed mergeability value, the PR head, the duration and the check-run state without opening GitHub.
- **SC-006**: Each new test fails when the fix is neutralised: once with the unknown-merge-state bound disabled, once with the backstop comment reverted to the fixed CI text. The pull request records this.
- **SC-007**: Unit tests cover:
  - the clock starting on both unknown forms;
  - the clock resetting on a computed state and on a head change;
  - no start on a read error or a "CI checks pending" wait;
  - a single firing at the bound;
  - no firing on the first evaluation after a start;
  - no repost.
- **SC-008**: Sim tests cover:
  - a paused persistent-null Validate item with green checks;
  - a null that clears before the bound;
  - a yolo item without `fabrik:awaiting-ci` under a persistent null;
  - an item held by "CI checks pending" past the CI-wait backstop.
- **SC-009**: The canonical docs describe the unknown-merge-state sub-reason, the backstop comment content and the gate-hold description. The new setting is documented in the user guide. The docs bundle is regenerated in the same commit.

## Assumptions

- The default for the new bound, and the setting's name, are chosen at planning. This spec requires only that the default be tens of minutes and well below the CI-wait backstop.
- The attention-cause vocabulary entry is also chosen at planning, in line with the existing cause vocabulary.
- An in-memory clock is acceptable. A restart can lengthen the effective wait but never shorten it, which matches the precedent for the CI-wait backstop.
- "Check-run state at escalation" means the counts of passed, failed and pending checks for the PR head, read at the moment of escalation.
- "Unknown merge state" covers exactly a null mergeability and a merge state equal to `unknown`. Other unsettled reasons are unchanged.
- The pull request mentions #1809 and #1943 by bare number only, with no closing keyword, and carries `Closes #2302` for this issue.

## Out of Scope *(optional)*

- The #2096 re-check cadence, which is already shipped.
- Bounding "CI checks pending" holds on items without `fabrik:awaiting-ci` (the broader case in #1943).
- Applying `fabrik:awaiting-ci` on merge-gate claims, which #2096 also excluded.
- The CI liveness dwell (#1410).
- Queued-in-merge-queue holds.
- Merge-train landing waits (#2297).
- Any change to the CI-wait backstop's duration.

## Source References *(optional)*

- #2096 and `adrs/2096-cooldown-consumed-on-admission.md`: the shipped re-check fix that left these two gaps out of scope.
- #1809: the original report. #1943: the Validate-complete merge-gate-held shape. Both are mentioned for context only.
- ADR-2059: precedent for never escalating blind on the first evaluation after a start.
- ADR 2121: the attention cause vocabulary.
- #1408: per-episode deduplication of the CI pause comments.
- #1952: the conditional-request cache whose effect on re-reading a null mergeability must be confirmed.
- `docs/state-machine.md` §6.4, §6.5 and §6.14.6.
