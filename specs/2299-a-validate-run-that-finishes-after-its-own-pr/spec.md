# Feature Specification: A Validate run that finishes after its own PR merged takes the terminal path, not `fabrik:awaiting-ci`

**Feature Branch**: `fabrik/issue-2299`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "

> ## Problem
>
> Report #1810 (observed on v0.0.81, still present in v0.0.85): a Validate worker that returns after the linked PR has already merged arms `fabrik:awaiting-ci` against that merged PR. Nothing in the completion path checks whether the PR is still open.
>
> Sequence from the report (one issue, one PR):
>
> 1. A Validate run completes and arms `fabrik:awaiting-ci`. On a later poll `settleAwaitingCIScan` clears the CI gate and, with `fabrik:yolo`, merges the PR (`PR #3125 merged directly`). The remote branch is deleted on merge.
> 2. A second Validate run for the same item was already in flight. It finishes 12 minutes later (16 turns of work on a branch that has already landed).
> 3. The engine's push fails (the branch no longer exists on the remote), the failure is logged as a warning, and the stage still completes.
> 4. `handleStageComplete` adds `fabrik:awaiting-ci` again and logs `deferring stage:Validate:complete until CI gate clears`.
>
> In v0.0.85 code:
>
> - **Completion never looks at the PR.** `engine/stages.go:180-209` (`handleStageComplete`): when the stage has `wait_for_ci: true`, the label is added if absent and the function returns. `stage:Validate:complete` is withheld. There is no read of the linked PR's state at this point. The same function is reached from three call sites (`engine/item.go:821`, `engine/item.go:1862`, `engine/comments.go:1120`).
> - **The settle scan can merge while a worker for the item is running.** `settleAwaitingCIScan` (`engine/ci_settle.go:88`) checks `ciBackstopWorkerInFlight` only on the backstop branch (`engine/ci_settle.go:244`). The normal path runs the Phase 1 handler chain and then `runCatchUpPhase2` (`engine/ci_settle.go:301`), which for a yolo Validate item calls `attemptMergeOnValidate` (`engine/poll.go:2158`) with no in-flight check. That is how the merge in step 1 happened under the second worker.
> - **A push to a deleted branch is only a warning.** `engine/item.go:1752-1758`: `pushBranchUnlessQueued` errors are logged and ignored unless the error is `ErrPushRefusedZeroAhead`. The push is `git push --force-with-lease -u origin <branch>` (`engine/worktree.go:625`). Depending on the state of the local remote-tracking ref, a push after the remote branch was deleted either fails the lease (the report's case) or succeeds and re-creates the deleted branch. The zero-ahead guard does not help here: it treats a merged PR as "not open" and allows the push (`engine/push_guard.go:89`).
> - **What happens next depends on the column the item is in when the label lands:**
>   - **Validate column.** `settleAwaitingCIScan` reaches `checkCIGate`, which has a `PRMergeTerminal` branch for a merged PR (`engine/ci.go:81-86`: `addCompleteLabelAndRemoveCI`). The reporter saw no `pr-terminal` line 47 minutes after the merge on v0.0.81. Whether v0.0.85 recovers through this branch in this exact sequence has not been confirmed and must be established (see R6).
>   - **Done column** (the item had already advanced). `settleAwaitingCIScan` finds no `wait_for_ci` stage at the column and logs `stranded at column` (`engine/ci_settle.go:113-118`). After `MaxRetries` passes, `escalateAwaitingCIOrphanFailure` (`engine/ci_settle.go:343-363`) pauses the issue with the comment "move the issue back to the `wait_for_ci` stage ... (e.g. `Validate`)". That advice is wrong for a landed item.
>   - **Closed issue.** The scan skips closed items (`engine/ci_settle.go:108`), so the label simply stays.
>
> Consequences listed in the report: the eventual escalation blames CI for a PR that has already merged; withholding `stage:Validate:complete` hides the item from the merged-but-open sweep (`gh issue list --state open --label "stage:Done:complete"` and the related checks); and the second run's spend is wasted.
>
> ## Requirements
>
> R1. When a stage with `wait_for_ci: true` completes (every caller of `handleStageComplete`), the engine reads the linked PR's state live, not from the dispatch-time snapshot or the board cache. If the PR is merged, the engine does not add `fabrik:awaiting-ci`. It takes the same terminal path a merged Validate PR already takes (`advanceValidateTerminalItem`'s merged branch: fill the gate-checked `stage:*:complete` labels, clear gate labels, advance to Done, close the issue for a non-default base or via the #1962 default-base guard, mark landing verification). That path must stay idempotent if the item has already reached Done.
>
> R2. If the PR is closed without merging at that point, the outcome matches the existing `PRMergeTerminal` closed-not-merged handling (`pauseForPRClosedNotMerged`). It does not arm `fabrik:awaiting-ci`.
>
> R3. If the live PR read fails, the behaviour stays as it is today (arm `fabrik:awaiting-ci`). The settle scan's `PRMergeTerminal` handling remains the backstop. The log line must say the read failed.
>
> R4. `settleAwaitingCIScan` does not merge (and does not enable auto-merge for) a Validate item while a worker for that item is in flight. It defers, with a log line naming the in-flight stage, and evaluates again on a later poll. Gate evaluation and label clearing may still run. Only the merge decision waits. If research shows that deferring breaks an existing guarantee (for example the ADR-1216 same-poll landing handoff), the issue must record that trade-off in an ADR rather than silently dropping R4.
>
> R5. A post-stage push failure whose cause is that the remote branch no longer exists is treated as a signal, not a warning. The engine checks the linked PR live. If the PR is merged, it skips the completion chain's gate arming and follows R1. The engine must never re-create the remote branch of a merged PR: a push that would do so is not made. Other push failures keep today's behaviour.
>
> R6. An item that already sits in the Done column (or is closed) with a merged linked PR is never paused as "stranded" by the awaiting-ci orphan escalation. If `fabrik:awaiting-ci` is found on such an item, the engine clears it and runs (or confirms) the terminal path. The "move the issue back to Validate" comment must not be posted for a merged PR. Research must confirm which v0.0.85 path (Validate-column `PRMergeTerminal`, Done-column stranded escalation, closed-skip) each ordering of the race reaches. The fix must cover every one of them.
>
> R7. Log lines name what happened, e.g. `[#N awaiting-ci] linked PR #M already merged at completion — taking terminal path, not arming awaiting-ci`.
>
> ## Scope
>
> In scope: `handleStageComplete`'s `wait_for_ci` branch, the post-stage push failure handling in `finalizeStageOutcome`, the merge decision inside `settleAwaitingCIScan`/`runCatchUpPhase2` while a worker is in flight, and the awaiting-ci orphan escalation for Done/closed items with a merged PR.
>
> Out of scope: cancelling a Validate worker when its PR merges (a possible follow-up). The CI-backstop timeout wording in general. The transient-`mergeable` case the report mentions as filed separately. The merge-train landing paths, which do not go through `handleStageComplete`'s `wait_for_ci` branch.
>
> Mention #1810 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1810. This issue's own number gets `Closes`.
>
> ## Acceptance
>
> - Unit tests (engine):
>   - `handleStageComplete` with `wait_for_ci: true` and a live-merged linked PR: no `fabrik:awaiting-ci` add; terminal path taken; `stage:Validate:complete` present; item advanced to Done.
>   - The same with a closed-unmerged PR: the closed-not-merged pause, no awaiting-ci.
>   - A live-read error: awaiting-ci armed as today, plus the log line.
>   - `settleAwaitingCIScan` with a cleared CI gate, yolo, and a registered Validate worker: no `MergePR`/auto-merge call. The same without a worker: merge happens as today.
>   - Post-stage push failing with a deleted remote branch and a merged PR: no awaiting-ci and no branch re-creation.
>   - A Done-column item with `fabrik:awaiting-ci` and a merged PR: no orphan pause, no "move back to Validate" comment.
> - Sim twin in `tests/sim`: a yolo item at Validate. The first Validate run completes and arms awaiting-ci. A second Validate run is held in flight (simclaude blocks) while the CI gate clears. Assert that no merge happens while it runs. In a second scenario the PR is merged out of band before the worker returns: assert the item reaches Done and the issue closes with no awaiting-ci left, no pause, and no "CI wait timeout" comment.
> - Non-vacuity: neutralise each fix in turn (remove the live merged check in `handleStageComplete`; remove the in-flight merge guard) and show the corresponding test fails.
> - As-built docs: update `docs/state-machine.md` (the `fabrik:awaiting-ci` label row, §6.14 "Awaiting-CI Settle Scan", and the Validate terminal-advance description) and `docs/stage-lifecycle.md` ("Branch Pushing"). Regenerate `docs/llms-full.txt` with `bash scripts/generate-llms-full.sh` in the same commit."

## Background

Community report #1810 (seen on v0.0.81, still present in v0.0.85) shows that a Validate worker finishing after its own PR has already merged leaves the item in a bad state. In the observed sequence a first Validate run completes and arms the CI wait. A later poll clears the CI gate and, with `fabrik:yolo`, merges the PR, and the remote branch is deleted on merge. A second Validate run for the same item was already in flight, and it finishes 12 minutes later. Its push fails because the branch is gone, the failure is only a warning, and the stage still completes. The completion step then arms the CI wait again, this time against a PR that has already merged.

Three gaps combine to produce this:

1. **Stage completion never checks the linked PR's state.** Completion of a stage that waits for CI always arms the CI wait and withholds the stage-complete label, even when the PR has already landed.
2. **The CI-wait settle scan can merge under a running worker.** The in-flight-worker check exists only on the scan's timeout backstop, not on the path that decides to merge a yolo Validate item.
3. **A push to a deleted remote branch is only a warning.** Depending on the local remote-tracking state, such a push either fails (the report's case) or silently re-creates the deleted branch of a merged PR. The zero-ahead push guard treats a merged PR as "not open" and lets the push through.

What happens next depends on where the item sits when the label lands:

- **Validate column.** The settle scan has a terminal-PR branch that should recover a merged PR. The reporter saw no such recovery 47 minutes after the merge on v0.0.81, and whether v0.0.85 recovers in this exact sequence is unconfirmed.
- **Done column.** The scan finds no CI-waiting stage at the column and eventually pauses the issue, telling the operator to move it back to Validate. That advice is wrong for a landed item.
- **Closed issue.** The scan skips closed items, so the label stays forever.

The consequences are that the eventual escalation blames CI for a PR that has already merged, and that withholding the Validate-complete label hides the item from the operator's merged-but-open sweeps. The second run's spend is also wasted.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A late Validate completion on a merged PR lands the item (Priority: P1)

As an operator running Fabrik, when a Validate worker (or any stage that waits for CI) finishes after its linked PR has already merged, I want the item to take the normal landed path (stage labels filled, advanced to Done, issue closed) instead of being parked in a CI wait for a PR that can never pass CI.

**Why this priority**: This is the core defect reported in #1810; every other story supports or hardens it.

**Independent Test**: With a stage that waits for CI and a linked PR the live read reports as merged, complete the stage and check that no CI-wait label is added, the stage-complete label is present, and the item is in Done and closed.

**Acceptance Scenarios**:

1. **Given** a CI-waiting stage whose worker finishes while the live linked-PR read reports merged, **When** the stage completion is handled, **Then** `fabrik:awaiting-ci` is not added, `stage:<Stage>:complete` is present and the item takes the merged-PR terminal path (advance to Done, close, landing verification as that path already does).
2. **Given** the same situation but the item already reached Done, **When** the terminal path runs again, **Then** it is idempotent (no duplicate close, comment or label churn, no error).
3. **Given** a stage completion reached from any of its callers (stage run, CI-fix/reinvoke, comment review), **When** the live read reports merged, **Then** each behaves as in scenario 1.
4. **Given** the live read reports merged, **When** the engine logs the decision, **Then** the log line names the item, the PR and that it is taking the terminal path instead of arming the CI wait.

---

### User Story 2 - Closed-unmerged and unreadable PRs behave predictably (Priority: P1)

As an operator, I want a PR that was closed without merging to get the existing closed-not-merged pause, and an unreadable PR state to fall back to today's behaviour with a clear log line, so no new failure mode appears.

**Why this priority**: The live read introduced by Story 1 has three outcomes; the other two must be specified so the change stays safe.

**Independent Test**: Complete a CI-waiting stage with the live read returning (a) closed-unmerged and (b) an error; check the resulting labels and logs.

**Acceptance Scenarios**:

1. **Given** the live read reports the PR closed without merging, **When** the stage completion is handled, **Then** the outcome matches the existing closed-not-merged terminal handling and `fabrik:awaiting-ci` is not added.
2. **Given** the live read fails, **When** the stage completion is handled, **Then** `fabrik:awaiting-ci` is armed exactly as today and the log line says the read failed.
3. **Given** the live read reports an open PR (or no PR), **When** the stage completion is handled, **Then** behaviour is unchanged from today.

---

### User Story 3 - The CI-wait scan does not merge under a running worker (Priority: P1)

As an operator, I want a yolo Validate item not to be merged while another worker for that item is still running, so that a merge cannot land beneath in-flight work and strand its result.

**Why this priority**: This is how the report's race starts; closing it removes the cause rather than only the symptom.

**Independent Test**: With the CI gate cleared, a yolo Validate item and a registered in-flight worker for it, run the settle scan and assert no merge or auto-merge call is made; remove the worker and assert the merge happens as before.

**Acceptance Scenarios**:

1. **Given** a yolo Validate item with a cleared CI gate and a worker in flight for it, **When** the settle scan runs, **Then** no merge and no auto-merge enablement is made, and a log line names the in-flight stage.
2. **Given** the same item once the worker has exited, **When** the settle scan next runs, **Then** the merge decision proceeds as today.
3. **Given** a worker in flight, **When** the scan runs, **Then** gate evaluation and label clearing may still proceed; only the merge decision waits.

---

### User Story 4 - A push to a deleted remote branch never resurrects a merged PR's branch (Priority: P2)

As an operator, I want a post-stage push that fails because the remote branch no longer exists to be treated as a signal that the PR may have landed, and I never want the remote branch of a merged PR re-created.

**Why this priority**: It closes the step-3 gap in the report and a latent branch-resurrection hazard, but Story 1 already covers the outcome once the PR state is read.

**Independent Test**: Run a post-stage push against a remote with the branch deleted and a merged linked PR; assert the remote branch is not re-created, no CI wait is armed and the item follows the Story 1 path.

**Acceptance Scenarios**:

1. **Given** a post-stage push whose failure cause is "remote branch no longer exists" and a live read showing the PR merged, **When** the stage finishes, **Then** gate arming is skipped and the Story 1 path is followed.
2. **Given** the remote branch is gone and the linked PR is merged, **When** the engine would otherwise push, **Then** the push is not made, so the branch is not re-created.
3. **Given** any other push failure, **When** the stage finishes, **Then** today's behaviour is kept.

---

### User Story 5 - Landed items in Done or closed are never paused as "stranded" (Priority: P1)

As an operator, I want an item already in the Done column (or closed) with a merged PR and a leftover `fabrik:awaiting-ci` to be cleaned up quietly, not paused with advice to move it back to Validate.

**Why this priority**: Without it, any ordering of the race that lets the item advance first still ends in a wrong pause or a permanently stuck label.

**Independent Test**: Seed a Done-column item, and separately a closed item, each with `fabrik:awaiting-ci` and a merged PR; run the settle scan enough passes to exceed the retry limit and assert no pause and no "move back to Validate" comment appear and the label is cleared.

**Acceptance Scenarios**:

1. **Given** a Done-column item carrying `fabrik:awaiting-ci` with a merged linked PR, **When** the settle scan runs, **Then** the label is cleared and the terminal path is run or confirmed, with no pause and no "move back to Validate" comment.
2. **Given** a closed issue carrying `fabrik:awaiting-ci` with a merged linked PR, **When** the scan runs, **Then** the stray label is cleared rather than left forever.
3. **Given** an item whose column is Validate, **When** the scan runs and the PR is merged, **Then** the existing terminal branch recovers it, and the investigation confirms this in the report's exact ordering.
4. **Given** a Done-column item with `fabrik:awaiting-ci` and a PR that is not merged, **When** the scan runs, **Then** today's stranded handling is unchanged.

---

### Edge Cases

- The PR merges between the live read and the label write (a very narrow window): the settle scan's terminal-PR handling remains the backstop.
- The item has no linked PR, or the linked PR cannot be found: unchanged from today (arm the CI wait).
- The completion is a comment-review or CI-fix reinvoke of the stage rather than the main stage run: the same live check applies at every caller.
- A stage that does not wait for CI is unaffected.
- The in-flight guard defers the merge on one poll: it must not wedge the item once the worker exits, and must not strand the same-poll landing hand-off that exists today (see Assumptions).
- The remote branch is missing but the PR is open (not merged): this keeps today's push-failure behaviour.
- Merge-train landing paths are untouched.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a stage with `wait_for_ci: true` completes, for every caller of the stage-completion handler, the engine MUST read the linked PR's state live, not from the dispatch-time snapshot or the board cache.
- **FR-002**: If that live read shows the PR merged, the engine MUST NOT add `fabrik:awaiting-ci` and MUST take the same terminal path a merged Validate PR already takes: fill the gate-checked `stage:*:complete` labels, clear gate labels, advance to Done, close the issue (non-default base, or via the #1962 default-base guard), and mark landing verification.
- **FR-003**: The terminal path in FR-002 MUST be idempotent when the item has already reached Done.
- **FR-004**: If the live read shows the PR closed without merging, the outcome MUST match the existing closed-not-merged terminal handling, and `fabrik:awaiting-ci` MUST NOT be armed.
- **FR-005**: If the live read fails, behaviour MUST stay as today (`fabrik:awaiting-ci` armed), the settle scan's terminal-PR handling remains the backstop, and the log line MUST state that the read failed.
- **FR-006**: The awaiting-CI settle scan MUST NOT merge, and MUST NOT enable auto-merge for, a Validate item while a worker for that item is in flight. It MUST defer with a log line naming the in-flight stage and evaluate again on a later poll. Gate evaluation and label clearing MAY continue; only the merge decision waits.
- **FR-007**: If the research or planning for FR-006 shows that deferring breaks an existing guarantee (for example the ADR-1216 same-poll landing handoff), the trade-off MUST be recorded in an ADR numbered after this issue rather than FR-006 being silently dropped.
- **FR-008**: A post-stage push failure whose cause is that the remote branch no longer exists MUST be treated as a signal rather than a warning: the engine checks the linked PR live, and if it is merged, skips gate arming and follows FR-002. All other push failures MUST keep today's behaviour.
- **FR-009**: The engine MUST NOT re-create the remote branch of a merged PR; a push that would do so MUST NOT be made.
- **FR-010**: An item in the Done column (or closed) with a merged linked PR MUST NOT be paused as "stranded" by the awaiting-CI orphan escalation. If `fabrik:awaiting-ci` is found on such an item, the engine MUST clear it and run or confirm the terminal path. The "move the issue back to Validate" comment MUST NOT be posted for a merged PR.
- **FR-011**: Research MUST establish which current path (Validate-column terminal-PR branch, Done-column stranded escalation, closed-issue skip) each ordering of the race reaches, and the fix MUST cover every one of them.
- **FR-012**: Log lines MUST say what happened, for example `[#N awaiting-ci] linked PR #M already merged at completion — taking terminal path, not arming awaiting-ci`.
- **FR-013**: Engine unit tests MUST cover: a live-merged PR (terminal path, no awaiting-ci, stage-complete present, item at Done); a closed-unmerged PR; a live-read error (awaiting-ci armed plus the log line); the settle scan with a cleared gate and yolo both with and without a registered Validate worker (no merge vs merge as today); a post-stage push failure with a deleted remote branch and a merged PR (no awaiting-ci, no branch re-creation); a Done-column item with awaiting-ci and a merged PR (no orphan pause, no "move back" comment).
- **FR-014**: A sim twin in `tests/sim` MUST cover two scenarios for a yolo item at Validate. In the first, a second Validate run is held in flight while the CI gate clears, and no merge happens while it runs. In the second, the PR is merged out of band before the worker returns, and the item reaches Done, the issue closes, and no awaiting-ci, pause or "CI wait timeout" comment remains.
- **FR-015**: Non-vacuity MUST be demonstrated for each fix in turn: with the live merged check removed from stage completion, and with the in-flight merge guard removed, the corresponding test fails.
- **FR-016**: As-built docs MUST be updated: `docs/state-machine.md` (the `fabrik:awaiting-ci` label row, §6.14 "Awaiting-CI Settle Scan", and the Validate terminal-advance description) and `docs/stage-lifecycle.md` ("Branch Pushing"). `docs/llms-full.txt` MUST be regenerated in the same commit.
- **FR-017**: The PR body MUST mention #1810 by bare number and MUST NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near it; this issue's own number gets `Closes`.

### Key Entities *(if applicable)*

- **Linked PR state**: open, merged, or closed-without-merge, read live at the moment of the decision (or unreadable).
- **`fabrik:awaiting-ci`**: the marker that a stage's completion is deferred until CI clears; meaningful only while the linked PR is open.
- **In-flight worker**: a running Fabrik worker for an item, which the settle scan must respect before merging.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the reported race (second Validate run finishing after its PR merged), the item ends at Done and closed with no `fabrik:awaiting-ci`, no pause and no CI-timeout comment, in both the unit and the sim scenario.
- **SC-002**: Zero merges (or auto-merge enablements) occur for a yolo Validate item while a worker for it is in flight; the merge occurs on a later poll after the worker exits.
- **SC-003**: No remote branch is re-created for an issue whose linked PR is merged.
- **SC-004**: No Done-column or closed item with a merged PR is paused by the awaiting-CI orphan escalation, and no "move the issue back to Validate" comment is posted for a merged PR.
- **SC-005**: Behaviour for open PRs, unreadable PR state, stages without CI waiting, and other push failures is unchanged, demonstrated by existing tests passing and by the explicit read-error case.
- **SC-006**: Disabling either fix makes its dedicated test fail (non-vacuity demonstrated for both).
- **SC-007**: The as-built docs describe the new behaviour and `docs/llms-full.txt` matches its regeneration, so the docs-drift check passes on first push.

## Assumptions

- "Merged" and "closed without merging" are taken from the live PR state the engine already uses for terminal-PR decisions, not a new source of truth.
- The existing merged-PR terminal path and the existing closed-not-merged handling are reused as-is rather than redesigned.
- For the in-flight guard, deferring the merge to a later poll is acceptable because the item is re-evaluated every poll. It is assumed not to break the same-poll landing handoff; if investigation shows otherwise, an ADR records the trade-off (FR-007).
- Stage-complete labelling for a merged-PR terminal path at completion follows whatever the existing terminal path does for the item's column.
- The fix covers all current callers of stage completion without changing their signatures' external behaviour for non-CI-waiting stages.

## Out of Scope *(optional)*

- Cancelling a Validate worker when its PR merges (a possible follow-up).
- The CI-backstop timeout wording in general.
- The transient-`mergeable` case the report mentions, which is filed separately.
- The merge-train landing paths, which do not go through the CI-wait branch of stage completion.

## Source References *(optional)*

- Community report #1810 (observed on v0.0.81, still present in v0.0.85)
- #1962 (default-base auto-close guard), ADR-1216 (same-poll landing handoff)
