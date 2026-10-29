# Feature Specification: Surface a declined at-head bot review finding as an immediate decision

**Feature Branch**: `fabrik/issue-2466`
**Created**: 2026-10-29
**Status**: Specified
**Input**: User description: "## Problem

When Review answers a reviewer's `CHANGES_REQUESTED` with a deliberate no-change, it pushes nothing, so a bot reviewer that reviews on push never reviews again. The `CHANGES_REQUESTED` stays **on the current head**, the gate waits for a review that cannot come, and the item pauses on `review wait timeout`. Every unpause repeats the timeout. Reported in #2453 (@totalslacker).

Reported instance, chronicle#529 / PR #534 (2026-10-24): `CHANGES_REQUESTED` on head `b15bf0d7` with a low-severity note the bot itself said it \"could not confirm\"; after unpause, Review declined it with reasons and logged `no new commit pushed (HEAD still b15bf0d7)`; the gate then scheduled the next timeout.

**#2465 does not fix this.** Head-awareness only discounts verdicts on older commits; here the verdict is on the head.

## Requirements

**R1 — Never wait out a timeout for a review that structurally cannot arrive.** When a review reinvoke addresses an at-head `CHANGES_REQUESTED` from a declared bot reviewer and lands no commit, the engine must act at once rather than after the wait timeout. The detection point exists: the no-op branch of `dispatchReviewReinvoke`'s `after` hook (`ReviewCycleDecremented`).

**R2 — Floor: surface it as a decision immediately.** Pause with a comment naming the reviewer, the finding(s) declined, and the options (re-review by hand, dismiss the review, or override). It must read as a decision, not as a timeout.

**R3 — Optional, only if verified: re-request the review.** If the reviewing bot demonstrably re-runs on a review request (GitHub `requestReviews` for an App reviewer — unverified, check before relying on it), re-request it instead of pausing, bounded by the existing `MaxReviewCycles` so a bot that keeps raising the same finding still ends in a pause.

**R4 — Never treat the finding as answered on Fabrik's own say-so.** Clearing an authoritative gate because the worker disagreed with the reviewer would make `authoritative` meaningless. A human, or the reviewer itself, decides.

## Scope

**In scope:** the review-reinvoke no-op path, the pause/decision comment, re-request if R3 holds, `docs/state-machine.md`, tests and a sim twin.
**Out of scope:** head-aware verdicts (#2465); the wait clock (sibling issue); advisory mode.

## Acceptance

1. At-head bot `CHANGES_REQUESTED`, reinvoke lands no commit → the item is surfaced as a decision on that poll, without waiting for the timeout. Reproduced red against `main` first.
2. The comment names the reviewer and the declined finding(s).
3. The gate is never cleared by the worker's reply alone (R4).
4. If R3 is implemented, a bot that re-raises the same finding still ends in a pause within `MaxReviewCycles`.
5. Tests demonstrated non-vacuous; `go test -race ./...` and `go vet ./...` pass.

## Notes

Reported in #2453 — a human-owned community thread. Mention it in the PR body by bare number only. Do not place any closing keyword (`Closes`/`Fixes`/`Resolves`) adjacent to that number in any form, including inside a negated or disclaiming sentence. Closed by hand once a release ships."

## Background

When the Review stage reinvokes a worker to address an authoritative reviewer's `CHANGES_REQUESTED`, the worker may deliberately decline the finding and push nothing. A bot reviewer that reviews only on push never reviews again, so its `CHANGES_REQUESTED` stays on the current head. The review gate then waits for a review that structurally cannot arrive, the item pauses on `review wait timeout`, and every unpause repeats the same wait.

Reported in #2453. The observed instance was chronicle#529 / PR #534 on 2026-10-24. The bot's `CHANGES_REQUESTED` on head `b15bf0d7` carried a low-severity note it said it "could not confirm". After unpause, Review declined it with reasons and logged `no new commit pushed (HEAD still b15bf0d7)`. The gate then scheduled the next timeout.

Neighbouring work does not cover this case:

- #2465 (head-aware verdicts) only discounts verdicts on older commits. Here the verdict is on the current head.
- The review-wait clock is a separate sibling issue.

The engine already has a detection point for the no-op outcome: a review reinvoke that lands no commit reverses its cycle-counter increment (`ReviewCycleDecremented`). That is the natural place to recognise the situation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Declined at-head bot finding becomes an immediate decision (Priority: P1)

As an operator, when Review declines a declared bot reviewer's at-head `CHANGES_REQUESTED` without pushing a commit, I want the item to be surfaced to me as a decision right away. I do not want to wait out the review timeout for a review that cannot come.

**Why this priority**: This is the reported defect. Every occurrence costs a full timeout, and every unpause repeats it. It is also the floor that every other requirement builds on.

**Independent Test**: In the sim, seed a PR whose head carries a declared bot reviewer's `CHANGES_REQUESTED` and let the review reinvoke run with a worker that lands no commit. Assert that on that poll the item is paused with a decision comment, and that no wait timeout has elapsed. Run the same scenario against `main` first to show it waits.

**Acceptance Scenarios**:

1. **Given** a declared bot reviewer's `CHANGES_REQUESTED` on the current head and a review reinvoke that lands no commit, **When** the reinvoke completes, **Then** the item is paused and a decision comment is posted on that poll, not after the review wait timeout.
2. **Given** the same state, **When** the decision comment is read, **Then** it names the reviewer, lists the finding(s) the worker declined, and offers the options (re-review by hand, dismiss the review, or override). It does not read as a timeout.
3. **Given** the item was paused by this decision, **When** a human resumes it without changing the review state, **Then** the gate does not treat the finding as answered, and a repeat decline surfaces the decision again.

---

### User Story 2 - The worker's reply never clears an authoritative gate (Priority: P1)

As an operator relying on `authoritative` review mode, I want Fabrik to refuse to mark a reviewer's finding as answered because its own worker disagreed. Only a human, or the reviewer itself, may decide.

**Why this priority**: Without this, the fix for the wait would become a way to bypass an authoritative gate and would make `authoritative` meaningless.

**Independent Test**: In the sim, run the Story 1 scenario and assert that, after the worker's decline, the gate's verdict for the head is still blocking. The only way it clears is a reviewer's new review or a human action on the review.

**Acceptance Scenarios**:

1. **Given** a declined at-head `CHANGES_REQUESTED`, **When** the engine handles the no-op reinvoke, **Then** no gate-clearing label or state change is written on the strength of the worker's reply.
2. **Given** a human dismisses the review or the reviewer submits a fresh non-blocking review, **When** the next poll runs, **Then** the gate clears through its normal rules.

---

### User Story 3 - Re-request the review when the bot demonstrably re-runs (Priority: P3, conditional)

As an operator, if the reviewing bot is known to re-run when a review is requested, I want Fabrik to ask for the re-review itself instead of pausing. A bot that keeps raising the same finding must still end in a pause.

**Why this priority**: This is an optimisation over the floor. Whether a review request makes an App reviewer re-run is unverified, so it is only worth shipping if the behaviour is confirmed.

**Independent Test**: With a sim reviewer that re-reviews on request, assert the engine requests the review instead of pausing. With one that re-raises the same finding each time, assert the item ends paused within `MaxReviewCycles`.

**Acceptance Scenarios**:

1. **Given** the review-request behaviour has been verified for the reviewer, **When** a declined at-head finding is handled, **Then** the engine requests a re-review instead of pausing.
2. **Given** the bot re-raises the same finding on each re-review, **When** `MaxReviewCycles` is reached, **Then** the item pauses with the Story 1 decision comment.
3. **Given** the behaviour has not been verified, **When** the work ships, **Then** only Stories 1 and 2 are delivered and this story is recorded as a follow-up.

---

### Edge Cases

- The reinvoke lands a commit: the head changes, so the existing flow applies unchanged and no decision pause is raised.
- The `CHANGES_REQUESTED` is from a reviewer that is not a declared bot reviewer (for example a human): behaviour is unchanged, because a human can re-review.
- The `CHANGES_REQUESTED` is on an older commit rather than the head: that is #2465's territory and is not changed here.
- Several declared bots have a blocking review at the head: the comment names each reviewer and its declined findings.
- The reinvoke fails or is cancelled rather than completing as a no-op: it is not treated as a declined finding, and the existing failure handling applies.
- The item is already paused or the decision comment was already posted for this head and reviewer: the comment is not duplicated.
- The reviewer's finding is a low-confidence note (as in the reported case): the same decision flow applies, because the engine cannot judge severity on the reviewer's behalf.
- Advisory-mode gates do not wait on this verdict and are unaffected.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a review reinvoke addresses an at-head `CHANGES_REQUESTED` from a declared bot reviewer and lands no commit, the engine acts on that same poll and does not wait for the review wait timeout.
- **FR-002**: The action in FR-001 is a pause that presents the item as a decision, using the existing pause semantics. It carries a comment that names each reviewer, lists the finding(s) the worker declined, and states the options: re-review by hand, dismiss the review, or override.
- **FR-003**: The decision comment's wording presents a pending decision and does not present a timeout.
- **FR-004**: The engine never treats a finding as answered, and never clears or bypasses an authoritative review gate, because its own worker declined or disagreed with the finding. Clearing requires a human action or a new review from the reviewer.
- **FR-005**: The decision pause is raised only when all of these hold: the blocking verdict is from a declared bot reviewer, it is on the current head, and the reinvoke landed no commit. Verdicts from non-declared reviewers, verdicts on older commits, and reinvokes that land a commit behave exactly as before.
- **FR-006**: The decision comment is not duplicated for the same head and reviewer when the situation is re-evaluated.
- **FR-007**: A resume of the decision pause that leaves the review state unchanged does not clear the gate. A further declined reinvoke on the same head surfaces the decision again, without an intermediate review wait timeout.
- **FR-008**: *(Conditional, ships only if verified)* If the reviewing bot is verified to re-run when a review is requested, the engine requests a re-review instead of pausing. The number of re-requests is bounded by the existing `MaxReviewCycles`, and a bot that keeps raising the same finding ends in the FR-002 pause within that bound.
- **FR-009**: The as-built behaviour is documented in `docs/state-machine.md`, with `docs/llms-full.txt` regenerated in the same change.
- **FR-010**: The behaviour is covered by unit tests and a sim twin scenario. Each is demonstrated non-vacuous (it fails when the behaviour is disabled), and the scenario is shown to fail against `main` first.

### Key Entities *(if applicable)*

- **Declared bot reviewer**: a self-submitting review bot named in the stage's expected reviewers, whose review verdicts are tracked per head.
- **At-head blocking verdict**: a `CHANGES_REQUESTED` review whose commit is the PR's current head, so no head-awareness discount applies.
- **Declined finding**: a finding in the blocking review that the review reinvoke chose not to act on and therefore landed no commit for.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the reproduced scenario, the item is surfaced as a decision on the same poll in which the no-commit reinvoke completes. The elapsed wait before it is surfaced is zero review-wait-timeouts, where `main` waits one or more.
- **SC-002**: The decision comment names 100% of the declared reviewers and declined findings that blocked the head in the scenario.
- **SC-003**: Across all tested scenarios, there are zero cases where the gate clears solely because the worker declined a finding.
- **SC-004**: If FR-008 ships, a bot that re-raises the same finding on every re-review leaves the item paused within `MaxReviewCycles` re-requests.
- **SC-005**: `go test -race ./...` and `go vet ./...` pass, and each new test fails when its behaviour is disabled.

## Assumptions

- "Declared bot reviewer" means a reviewer named in the stage's `expected_reviewers` (or the equivalent per-issue label). That is the only class for which "will never review again without a push" is known to hold. A non-declared reviewer is assumed able to re-review.
- The pause uses the engine's existing pause pair and the existing pause-cause label vocabulary. No new label is introduced unless the Plan stage finds one necessary.
- The existing no-op detection in the review reinvoke (`ReviewCycleDecremented`) is the intended detection point named in the issue. Its exact behaviour is for Research to confirm.
- FR-008 is conditional. It ships only if Research or Plan verifies that requesting a review makes the relevant App reviewer re-run. If it cannot be verified, it is deferred to a follow-up issue and Stories 1 and 2 ship alone.
- The decision comment's exact wording is a Plan/Implement detail. It must satisfy FR-002 and FR-003.
- The PR for this work mentions the originating community thread #2453 by bare number only, with no closing keyword in any form, per the repository's community-report rules. That thread is closed by hand after a release ships.

## Out of Scope *(optional)*

- Head-aware discounting of verdicts on older commits (#2465).
- The review-wait clock and its anchor (sibling issue).
- Advisory-mode review gating.
- Judging or auto-resolving the correctness of a reviewer's finding.
- Making any change that lets Fabrik itself dismiss a review or clear an authoritative gate.

## Source References *(optional)*

- Reported in #2453 (community thread, human-owned).
- Reported instance: chronicle#529 / PR #534, head `b15bf0d7`, 2026-10-24.
- Related: #2465 (head-aware authoritative verdict), ADR 2465, ADR 1250, ADR 1283, `docs/state-machine.md` §6.1.1 and §6.6.
