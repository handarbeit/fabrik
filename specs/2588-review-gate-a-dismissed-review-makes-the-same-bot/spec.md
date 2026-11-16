# Feature Specification: A dismissed review no longer hides the same bot's later COMMENTED reviews from the review gate

**Feature Branch**: `fabrik/issue-2588`
**Created**: 2026-11-15
**Status**: Specified
**Input**: User description (verbatim original issue body):

> ## Problem
>
> Community report #2582: a `wait_for_reviews` gate with a declared `expected_reviewers` bot times out (`fabrik:pause:review-timeout`, "`<bot>` did not respond") even though that bot has reviewed the current PR head, COMMENTED with no findings. The report has already happened on three or more PRs. Unpausing re-runs the same wait, and the bot never re-reviews a head that has not changed, so the only way out is a human merge.
>
> **Cause (confirmed by reading the code):** `github.(*Client).FetchPRReviews` collapses the review history to one entry per author and lets a COMMENTED never supersede a stored "formal verdict". `isFormalReviewVerdict` counts `DISMISSED` as a formal verdict. So once any review by an author has been dismissed, every later COMMENTED review by that author is dropped and the collapsed entry stays `DISMISSED` for good.
>
> - The reporter's timeline: the bot's CHANGES_REQUESTED on head A was dismissed, then it left clean COMMENTED reviews on heads B and C.
> - `checkReviewGate`'s live confirmation read (`liveReviews` → `FetchPRReviews`) sees only that DISMISSED entry. `declaredReviewersOutstanding` skips DISMISSED, so the bot reads as "did not respond", and `reviewGateOutstanding.hasReviews` is false when it is the only reviewer. That drives the bot re-prompt ladder into the timeout pause.
> - The Store's collapse (`itemstate.latestReviewPerAuthor`) keeps the latest review whatever its state, and `reviewerVerdicts` uses the head-aware standings (ADR 2465). That is why `fabrik_status` shows the same review as `at_head: true, clean_pass: true`. The cached view would clear the gate, and the live confirmation read blocks it.
>
> A dismissal ends a verdict; it is not one. A later COMMENTED from the same author is that author's current state.
>
> `tests/sim/simgh/reviews.go` mirrors the same sticky-DISMISSED rule, so the sim cannot reproduce the bug today. `github/prs_test.go` and `tests/sim/simgh/state_test.go` pin parts of the current behaviour.
>
> ## Requirements
>
> - **R1:** After a dismissal, a later COMMENTED review from the same author must count as that author's response for the review gate: in `declaredReviewersOutstanding` and in `hasReviews`. This applies to every gate that reads reviews: `checkReviewGate` (advance), `reviewGateBlocksLanding` / `reviewGateHold` (landing and `fabrik_advance`) and `pauseForReviewTimeout`'s status text.
> - **R2:** APPROVED and CHANGES_REQUESTED stay sticky against a later COMMENTED in `FetchPRReviews`. GitHub's reviewDecision semantics and ADR 2465's head-aware withdrawal are unchanged. Only DISMISSED stops blocking a later COMMENTED.
> - **R3:** The gate's live confirmation and the cached view must agree on the same PR state, so the cached view cannot say "clear" while the live read says "did not respond".
> - **R4:** Audit every `FetchPRReviews` caller (`engine/reviews.go`, `engine/live_read.go`, `pruefer/review.go`, `pruefer/trialpr.go`) and record in the ADR whether the collapse change affects each one. Pruefer's re-review decisions must not change behaviour unintentionally. If a caller needs the old collapse, keep it for that caller explicitly.
> - **R5:** Fix the sim fidelity: `simgh`'s collapse must match the real client's, and the FIDELITY note must be updated.
> - **R6:** Leave the re-prompt ladder, the review-wait anchor (#2467) and the timeout values unchanged. Once R1 holds, a bot that has reviewed the head is no longer outstanding, so the ladder does not fire.
>
> ## Scope
>
> In scope: the review collapse and the gate's reading of it. Out of scope: GraphQL `latestReviews` semantics (the cached path already keeps the latest review), human-review counting, and pause-comment wording beyond what R1 changes.
>
> ## Acceptance
>
> - A sim twin: a declared bot's CHANGES_REQUESTED on head A is dismissed, then the bot leaves a COMMENTED review on head B, which is the current head. Under advisory and under authoritative authority, the gate clears without a re-prompt or a pause. Neutralising the fix (restoring the sticky-DISMISSED collapse) must make the twin fail.
> - A unit test in `github/prs_test.go`: a history of DISMISSED then COMMENTED from one author collapses to the COMMENTED entry. A history of CHANGES_REQUESTED then COMMENTED still collapses to CHANGES_REQUESTED.
> - The existing tests that pin the old DISMISSED behaviour are updated, each with its reason stated.
> - An ADR numbered 2582 (the community report's number) records the change and the R4 audit. `docs/state-machine.md` §6.6 / §6.1.1 is updated, and `docs/llms-full.txt` is regenerated.
>
> ## Linkage
>
> Mention #2582 in the PR body by bare number only. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it. The PR closes only this work issue.

## Background

Community report #2582 describes a review-wait gate that pauses with a "did not respond" timeout even though the declared reviewer bot has already reviewed the current PR head with a clean COMMENTED review. This has happened on at least three PRs. Resuming does not help: the bot only reviews on push and never re-reviews an unchanged head, so the only way out has been a human merge.

The root cause is a disagreement between two views of the same review history. The live read the gate uses to confirm a clear verdict collapses each author's reviews to one entry, and it treats a dismissed review as a verdict that a later COMMENTED review can never replace. Once a bot's review has been dismissed, all of its later COMMENTED reviews are invisible to the gate, so the bot reads as silent. The cached view (and the status surface built on it) keeps the author's latest review whatever its state, so it reports the same review as a clean pass at the current head. The cached view would clear the gate; the live confirmation blocks it.

A dismissal ends a verdict; it is not itself one. A later COMMENTED review from the same author is that author's current state. The simulator mirrors the same sticky-dismissed rule, so it cannot reproduce the bug today.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A bot that reviewed the current head clears the gate after an earlier dismissal (Priority: P1)

An operator runs a stage with a review wait and a declared expected reviewer bot. The bot's earlier CHANGES_REQUESTED review was dismissed, and the bot has since left a clean COMMENTED review on the current PR head. The gate treats the bot as having responded, and the item advances (or lands) without a re-prompt or a timeout pause.

**Why this priority**: This is the reported bug. Without it, items stall into a pause that only a human merge can clear.

**Independent Test**: In the simulator, seed a declared bot's CHANGES_REQUESTED on head A, dismiss it, add a COMMENTED review by the same bot on head B (the current head), and run the gate under both advisory and authoritative review authority.

**Acceptance Scenarios**:

1. **Given** a declared bot whose CHANGES_REQUESTED on an older head was dismissed and who left a COMMENTED review on the current head, **When** the review gate is evaluated under advisory authority, **Then** the gate clears with no bot re-prompt and no review-timeout pause.
2. **Given** the same history, **When** the gate is evaluated under authoritative authority, **Then** the gate clears with no bot re-prompt and no review-timeout pause.
3. **Given** the same history, **When** the landing gate, the operator advance action, and the timeout pause's status text read the reviews, **Then** each treats the bot as having responded.
4. **Given** the fix is neutralised (the old sticky-dismissed collapse restored), **When** the same scenario runs, **Then** the test fails.

---

### User Story 2 - Standing verdicts stay sticky (Priority: P1)

An author's APPROVED or CHANGES_REQUESTED review is not erased by a later COMMENTED review from the same author. Only a dismissal stops blocking a later COMMENTED review.

**Why this priority**: Weakening the stickiness of real verdicts would let an unresolved change request look cleared, which is worse than the original bug.

**Independent Test**: A unit test on the review collapse for each history shape.

**Acceptance Scenarios**:

1. **Given** a history of DISMISSED then COMMENTED from one author, **When** the history is collapsed, **Then** the result is the COMMENTED entry.
2. **Given** a history of CHANGES_REQUESTED then COMMENTED from one author, **When** the history is collapsed, **Then** the result is still CHANGES_REQUESTED.
3. **Given** a history of APPROVED then COMMENTED from one author, **When** the history is collapsed, **Then** the result is still APPROVED.

---

### User Story 3 - The live confirmation and the cached view agree (Priority: P2)

For any PR review history, the gate's live confirmation read and the cached view must give the same answer about whether a declared reviewer has responded.

**Why this priority**: The bug was a live-versus-cached disagreement; removing the disagreement class prevents recurrence.

**Independent Test**: Run the dismissed-then-commented scenario and check that the status surface and the gate both report the reviewer as responded.

**Acceptance Scenarios**:

1. **Given** the dismissed-then-commented history, **When** the cached view and the live read are compared, **Then** both report the reviewer as having responded at the current head.

---

### User Story 4 - Other consumers of the collapse are audited, and the simulator matches reality (Priority: P2)

Every consumer of the review collapse (the engine's gates, the live-read helper, and Pruefer's re-review logic) is audited. The change is recorded for each, and Pruefer's re-review decisions do not change unintentionally. The simulator's collapse matches the real client's, with its fidelity note updated.

**Why this priority**: The collapse is shared. A silent behaviour change for another consumer, or a simulator that cannot reproduce the bug, would hide regressions.

**Independent Test**: Review the decision record's per-consumer audit; run the existing Pruefer tests unchanged; confirm the simulator twin from User Story 1 fails when the old rule is restored.

**Acceptance Scenarios**:

1. **Given** each consumer of the collapse, **When** the change is made, **Then** the decision record states for each whether its behaviour is affected.
2. **Given** a consumer that needs the old collapse, **When** the change is made, **Then** that consumer keeps the old behaviour explicitly rather than by accident.
3. **Given** the simulator, **When** it collapses a dismissed-then-commented history, **Then** it yields the same result as the real client.

---

### Edge Cases

- A dismissed review followed by a COMMENTED review on an older head (not the current head): the author counts as having reviewed (the dismissal no longer hides it), and the existing head-aware standings rules decide whether it satisfies the current head. The head-aware rules are unchanged.
- A dismissed review with no later review: the author is still treated as not having responded.
- DISMISSED, then COMMENTED, then a new CHANGES_REQUESTED: the CHANGES_REQUESTED is the standing verdict.
- CHANGES_REQUESTED, then DISMISSED, then COMMENTED: the COMMENTED review is the current state, since the dismissal ended the earlier verdict.
- A dismissed review followed by a later APPROVED or CHANGES_REQUESTED: the later verdict applies as today.
- A reviewer who is the only reviewer on the PR and has a dismissed review followed by a COMMENTED review: the PR counts as having reviews.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a dismissal, a later COMMENTED review from the same author MUST count as that author's response for the review gate, both in the check that a declared reviewer is still outstanding and in the check that the PR has any reviews at all.
- **FR-002**: FR-001 MUST apply to every gate that reads reviews: the stage advance gate, the landing gate, the operator advance action, and the status text of the review-timeout pause.
- **FR-003**: An author's APPROVED and CHANGES_REQUESTED reviews MUST remain sticky against a later COMMENTED review from that author in the review collapse. GitHub's review-decision semantics and the head-aware withdrawal of a change request (ADR 2465) MUST be unchanged. Only a dismissal stops blocking a later COMMENTED review.
- **FR-004**: The gate's live confirmation read and the cached view MUST agree about whether a reviewer has responded for the same PR state, so the cached view cannot say "clear" while the live read says "did not respond".
- **FR-005**: Every consumer of the review collapse (the engine's review gates, the live-read helper, and Pruefer's re-review and trial-PR logic) MUST be audited, and the decision record MUST state for each whether the collapse change affects it. Pruefer's re-review decisions MUST NOT change behaviour unintentionally; a consumer that needs the old collapse MUST keep it explicitly.
- **FR-006**: The simulator's review collapse MUST match the real client's, and its fidelity note MUST be updated to say so.
- **FR-007**: The bot re-prompt ladder, the review-wait anchor (#2467) and the timeout values MUST be unchanged. A bot that has reviewed the head is no longer outstanding after FR-001, so the ladder does not fire for it.
- **FR-008**: A simulator twin scenario MUST cover: a declared bot's CHANGES_REQUESTED on head A is dismissed, then the bot leaves a COMMENTED review on head B (the current head); under both advisory and authoritative review authority the gate clears with no re-prompt and no pause. The twin MUST fail when the fix is neutralised (the sticky-dismissed collapse restored).
- **FR-009**: A unit test MUST pin the collapse: DISMISSED then COMMENTED from one author yields the COMMENTED entry; CHANGES_REQUESTED then COMMENTED still yields CHANGES_REQUESTED.
- **FR-010**: Existing tests that pin the old dismissed-is-sticky behaviour MUST be updated, each with its reason stated.
- **FR-011**: A decision record numbered 2582 (the community report's number) MUST record the change and the FR-005 audit. The review-gate and review-verdict sections of the engine state-machine documentation MUST be updated, and the bundled documentation file regenerated.
- **FR-012**: The PR for this work MUST mention #2582 by bare number only, MUST NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it, and closes only this work issue. The report is closed by hand after a release containing the fix ships.

### Key Entities *(if applicable)*

- **Review history**: the ordered list of reviews on a PR, each with an author, a state (APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED), a time and the commit it was made on.
- **Collapsed review**: the single entry per author derived from the history; the thing the gate reads.
- **Declared reviewer**: a bot named in the stage's expected reviewers, which the gate waits for.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A PR whose declared bot has a dismissed review followed by a COMMENTED review on the current head no longer reaches a review-timeout pause under either review authority; the simulator twin passes with the fix and fails without it.
- **SC-002**: For every history shape in the collapse unit test, the collapsed result is as specified (DISMISSED then COMMENTED yields COMMENTED; CHANGES_REQUESTED or APPROVED then COMMENTED stays the earlier verdict).
- **SC-003**: Existing Pruefer review and re-review tests pass without modification, or each modification is explained in the decision record.
- **SC-004**: The decision record lists every consumer of the collapse with an explicit affected / not-affected statement, and the documentation bundle is in sync (the docs-drift check passes).

## Assumptions

- A dismissal ends the verdict it dismissed; a later COMMENTED review from the same author is that author's current state. This is the intended semantic.
- The cached view's behaviour (keep the latest review whatever its state) is already correct and is the reference the live collapse is brought in line with. No change is made to GraphQL latest-reviews semantics.
- The head-aware standings logic (ADR 2465) already decides whether a COMMENTED review at a given head satisfies the gate, so FR-001 only needs to stop the dismissal from hiding the review.
- The work is a single fix with its tests, decision record and documentation updates, so no decomposition into separate issues is needed.

## Out of Scope *(optional)*

- GraphQL latest-reviews semantics (the cached path already keeps the latest review).
- Human-review counting.
- Pause-comment wording beyond what FR-001 changes.
- Changing the re-prompt ladder, the review-wait anchor, or any timeout value.
- Closing community report #2582; that is done by hand after a release.

## Source References *(optional)*

- Community report #2582 (the report this work addresses).
- ADR 2465 (head-aware authoritative review verdict) and #2467 (review-wait anchor), both of which must remain unchanged in behaviour.
