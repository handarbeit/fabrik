# Feature Specification: Authoritative review gate judges each reviewer at the head commit

**Feature Branch**: `fabrik/issue-2465`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Under `review_authority: authoritative` with no branch-protection review requirement, a reviewer's
> `CHANGES_REQUESTED` blocks the gate for the life of the PR, including after that reviewer has
> re-reviewed the fixed head cleanly. Reported in #2453 (@totalslacker): 9 timeouts across 6 issues
> since 2026-10-23 on v0.0.89, nearly every PR that got a finding needing a human to move it on.
>
> ### Mechanism
>
> - `FetchPRReviews` (`github/prs.go`) collapses the review history to one entry per author and
>   **refuses to let a `COMMENTED` submission overwrite a formal verdict** — documented as deliberate,
>   mirroring GitHub's `reviewDecision`. It never consults `CommitID`.
> - `reviewGateAuthorityVerdict` (`engine/reviews.go:718`), in its no-branch-protection fallback,
>   blocks on **any** `CHANGES_REQUESTED` in that list.
> - So a bot reviewer that never approves (Pruefer has no approve mode) and once requested changes on
>   an old commit blocks forever: reported log on chronicle PR #531, repeating for 7 minutes after a
>   clean `COMMENTED` review of the head —
>   `authoritative gate still blocking: no branch-protection review requirement; totalslacker-pruefer[bot] requested changes`.
>
> ### Two code paths disagree on the same PR
>
> `reviewerVerdicts` (`engine/review_verdict.go`), behind `fabrik_status`'s `pr.reviewers`, evaluates
> at `AgreedHead()` with `AtHead = rev.CommitID == head`. On the reported PR it said
> `"state": "COMMENTED", "at_head": true, "clean_pass": true, "outstanding": 0` while the gate said
> "requested changes". The Store's own collapse (`latestReviewPerAuthor`, `internal/itemstate/observed.go`)
> keeps the greatest `(SubmittedAt, DatabaseID)` regardless of state — a third reading. Three
> collapses of one review history must become one.
>
> ## Requirements
>
> **R1 — Head-aware for every reviewer.** A formal verdict (`CHANGES_REQUESTED`, `APPROVED`) on a
> commit other than the current head no longer counts as that reviewer's verdict on the head. A
> reviewer whose latest verdict is on an older commit is **awaiting re-review**: the gate keeps
> waiting, it does not clear, and the reason no longer says "requested changes". This never clears a
> human's objection — it only stops counting a verdict on code that no longer exists (GitHub's own
> "dismiss stale reviews" setting is the precedent).
>
> **R2 — `COMMENTED` at head supersedes only for declared bot reviewers.** For a reviewer named in the
> stage's effective `expected_reviewers` (`effectiveExpectedReviewers`), a `COMMENTED` or `APPROVED`
> review **at head** is a clean pass and supersedes that author's earlier `CHANGES_REQUESTED`. For any
> other reviewer, GitHub's semantics stand unchanged: `COMMENTED` never withdraws a change request.
> `expected_reviewers` is already the declaration of self-submitting bots; it is the boundary.
>
> **R3 — One definition.** The gate (`checkReviewGate`, `reviewGateBlocksLanding`) and
> `reviewerVerdicts` must reach their per-reviewer verdict through one helper, so the gate and
> `fabrik_status` cannot disagree again. Follow `effectiveReviewAuthority`'s rationale for the two
> gates. Reconcile or document `latestReviewPerAuthor` against it.
>
> **R4 — Branch protection still wins.** When `reviewDecision` is non-empty, GitHub's value stays
> authoritative exactly as today. R1/R2 change only the no-branch-protection fallback.
>
> **R5 — Fail closed on unknowns.** An unknown head, or a review with no `CommitID`, keeps today's
> blocking behaviour.
>
> ## Scope
>
> **In scope:** the review collapse and verdict helper, `reviewGateAuthorityVerdict`'s fallback, both
> gate call sites, `reviewerVerdicts`, `docs/state-machine.md` (review gate), an ADR, tests and a sim
> twin.
>
> **Out of scope:** the declined-finding deadlock (sibling issue — R1 does not fix it, because there the
> `CHANGES_REQUESTED` is on the current head); the review-wait clock (sibling issue); advisory mode;
> Pruefer's own behaviour.
>
> ## Acceptance
>
> 1. **Reproduced red against `main` first:** expected bot reviewer `CHANGES_REQUESTED` on commit A,
>    push B, `COMMENTED` on B → the gate blocks today; after the fix it clears (R2).
> 2. An **undeclared** reviewer in the same sequence still blocks (R2 boundary).
> 3. `CHANGES_REQUESTED` on A, push B, no review of B → awaiting re-review: not cleared, and the reason
>    does not say "requested changes" (R1).
> 4. A non-empty `reviewDecision` decides, whatever the fallback would say (R4).
> 5. Gate and `reviewerVerdicts` fed the same input agree, pinned by one test (R3).
> 6. Missing head or `CommitID` blocks (R5).
> 7. Every new test demonstrated non-vacuous; `go test -race ./...` and `go vet ./...` pass.
>
> ## Notes
>
> Reported in #2453 — a human-owned community thread. Mention it in the PR body by bare number only.
> Do not place any closing keyword (`Closes`/`Fixes`/`Resolves`) adjacent to that number in any form,
> including inside a negated or disclaiming sentence. The report is closed by hand once a release
> ships. Prior art: ADR-1250, ADR-1283, #1539, #2122.

## Background

With `review_authority: authoritative` and no branch-protection review requirement on the repository, the review gate decides from its own reading of each reviewer's history. That reading is wrong in two ways that combine badly for bot reviewers:

- It does not look at which commit a verdict was given on. A `CHANGES_REQUESTED` on an old commit keeps blocking after new commits replace the code it was about.
- A later `COMMENTED` review can never withdraw a formal verdict. This is deliberate and mirrors GitHub. A self-submitting bot reviewer that has no approve mode, such as Pruefer, can therefore never clear its own earlier change request, even by reviewing the fixed head cleanly.

The result, reported by a community user in #2453, is a gate that blocks until its timeout and then needs a human to move the item on. There were 9 timeouts across 6 issues since 2026-10-23 on v0.0.89, nearly every PR that received a finding. The operator log keeps repeating "requested changes" for minutes after a clean review of the head.

The same review history is also read two other ways. The reviewer-verdict view behind `fabrik_status` judges each reviewer at the head commit and reported "clean pass" on the very PR the gate was blocking. The item Store keeps the latest submission per author regardless of state. Three readings of one history is the underlying defect. The user-visible symptom is that the gate and the status tool contradict each other.

The fix must not weaken legitimate objections. A human's change request stays binding on the current code, and a comment never silently withdraws it. Only two things change: a verdict on a commit that is no longer the head stops counting as a verdict on the head, and a declared bot's clean review of the head can supersede that same bot's earlier change request.

Prior art: ADR-1250 (review authority), ADR-1283 (`expected_reviewers`), #1539, #2122.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A declared bot's clean re-review of the fixed head clears the gate (Priority: P1)

An operator runs an authoritative review gate with a declared self-submitting bot reviewer. The bot requests changes on commit A. The implementation pushes a fix as commit B. The bot reviews B and submits a `COMMENTED` review with no findings. The gate should clear without human intervention.

**Why this priority**: This is the reported failure. It stalls nearly every PR that gets a finding and costs a human intervention each time.

**Independent Test**: Seed a PR with a declared bot's `CHANGES_REQUESTED` on A, head moved to B, and the same bot's `COMMENTED` on B, with no branch-protection requirement. Verify the gate clears.

**Acceptance Scenarios**:

1. **Given** a declared bot reviewer with `CHANGES_REQUESTED` on commit A, the PR head at B, and a `COMMENTED` review by that bot on B, **When** the authoritative gate is evaluated, **Then** it does not block on that reviewer.
2. **Given** the same sequence but the bot's review of B is `APPROVED`, **When** the gate is evaluated, **Then** it does not block on that reviewer.
3. **Given** the same sequence but the reviewer is **not** named in the effective expected reviewers, **When** the gate is evaluated, **Then** it still blocks.

---

### User Story 2 - A verdict on superseded code is not a verdict on the head (Priority: P1)

A reviewer, human or bot, gave a formal verdict on an earlier commit and has not looked at the current head. The gate should say it is waiting for that reviewer to re-review. It should not claim the reviewer currently requests changes, and it should not clear.

**Why this priority**: The first half of the defect. It makes the gate's wording and decision honest about code that no longer exists, and it is the precondition for Story 1.

**Independent Test**: Seed `CHANGES_REQUESTED` by a reviewer on A, head moved to B, no later review by that reviewer. Verify the gate does not clear and the reason says the reviewer is awaiting re-review, not "requested changes".

**Acceptance Scenarios**:

1. **Given** `CHANGES_REQUESTED` on A, head at B, and no review of B by that reviewer, **When** the gate is evaluated, **Then** it keeps waiting and the stated reason names an awaited re-review.
2. **Given** `APPROVED` on A, head at B, and no review of B, **When** the gate is evaluated, **Then** the approval does not count toward the head and the gate keeps waiting.
3. **Given** a human's `CHANGES_REQUESTED` on A, head at B, and a human `COMMENTED` on B, **When** the gate is evaluated, **Then** it does not clear on that reviewer.

---

### User Story 3 - The gate and the status view always agree (Priority: P2)

An operator or overseer reads `fabrik_status` for a PR and sees each reviewer's state. Whatever it shows for a reviewer must be exactly what the gate used to decide.

**Why this priority**: Prevents the contradiction from recurring. It matters less than the behaviour fix but is the stated root cause.

**Independent Test**: Feed one review history, with the same head and the same declared-reviewer set, to both consumers and compare their per-reviewer verdicts across a table of histories.

**Acceptance Scenarios**:

1. **Given** any review history, head and declared-reviewer set, **When** the gate and the reviewer-verdict view each compute a per-reviewer verdict, **Then** the two verdicts are identical.

---

### User Story 4 - Branch protection and unknowns behave as before (Priority: P2)

Where the repository's branch protection defines a review requirement, GitHub's own decision remains authoritative. Where the engine cannot establish the head or a review's commit, it keeps blocking.

**Why this priority**: Guards against the change widening the gate where it must not.

**Independent Test**: Seed cases with a non-empty GitHub review decision, and cases with a missing head or a review with no commit. Verify the outcomes match today's.

**Acceptance Scenarios**:

1. **Given** a non-empty GitHub review decision, **When** the gate is evaluated, **Then** that decision governs regardless of what the per-reviewer fallback would conclude.
2. **Given** an unknown head, or a blocking review that carries no commit, **When** the gate is evaluated, **Then** the gate blocks as it does today.

---

### Edge Cases

- A declared bot's `CHANGES_REQUESTED` and `COMMENTED` are both at head, with the `COMMENTED` submitted later: the comment supersedes. If the `CHANGES_REQUESTED` is later, it stands. Ordering is by submission time, with the review identifier breaking ties, as elsewhere.
- A declared bot passes cleanly at head and the head then moves again: the earlier clean pass no longer counts, and the reviewer is awaiting re-review.
- A declared bot's `CHANGES_REQUESTED` is on the current head and nothing later exists: it stands. This is the declined-finding deadlock, which is a separate issue.
- Several reviewers, one clean at head and another with a stale or standing verdict: the stale or standing one still holds the gate.
- A stage or issue with `expected-reviewers:none` declares no bot reviewers, so GitHub semantics apply to everyone.
- A review dismissed on GitHub keeps today's handling. Dismissal is not changed here.
- A `COMMENTED` review at head by a declared bot that carries unresolved inline findings: this changes only the per-reviewer verdict. Existing feedback handling for unresolved threads and review bodies is unchanged.
- A reviewer's only reviews are `COMMENTED` ones: no formal verdict exists, so nothing in this change makes it block or clear differently from today.
- Head unknown, or a review without a commit: fail closed, same as today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: In the authoritative gate's no-branch-protection fallback, a formal verdict (`CHANGES_REQUESTED` or `APPROVED`) submitted on a commit other than the PR's current head must not count as that reviewer's verdict on the head.
- **FR-002**: A reviewer whose latest formal verdict is on a commit other than the head, and who has no qualifying review at the head, is **awaiting re-review**: the gate keeps waiting and does not clear. The blocking reason shown to the operator must say the reviewer is awaiting re-review and must not say the reviewer "requested changes".
- **FR-003**: For a reviewer named in the stage's effective expected reviewers, a `COMMENTED` or `APPROVED` review at the head is a clean pass and supersedes that same reviewer's earlier `CHANGES_REQUESTED`.
- **FR-004**: For a reviewer not named in the effective expected reviewers, a `COMMENTED` review never withdraws a `CHANGES_REQUESTED`; GitHub's semantics are unchanged. Such a reviewer's `CHANGES_REQUESTED` on the head blocks, and one on an older commit leaves the reviewer awaiting re-review.
- **FR-005**: The authoritative gate at stage advance, the authoritative gate at the landing decision, and the reviewer-verdict view behind `fabrik_status` must obtain each reviewer's verdict from a single shared definition, so the same review history, head and expected-reviewer set always yield the same verdict from all three.
- **FR-006**: The per-author review collapse performed when storing reviews must be reconciled with the shared definition, or its different purpose must be documented where it is defined, so the repository holds no unexplained third reading of one review history.
- **FR-007**: When GitHub's review decision for the PR is non-empty, it remains authoritative exactly as today. FR-001 through FR-004 affect only the fallback used when it is empty.
- **FR-008**: When the head commit is unknown, or a review that would otherwise be judged has no commit identifier, the gate keeps today's blocking behaviour.
- **FR-009**: The effective expected-reviewer set used by FR-003 and FR-004 is the one the gate already resolves for the issue (stage configuration plus the per-issue `expected-reviewers:<mode>` label override), identical at both gate sites.
- **FR-010**: The as-built review-gate documentation describes the head-aware, declared-bot-aware verdict rules, and an ADR records the decision.

### Key Entities

- **Reviewer verdict at head**: the one per-reviewer outcome (clean, standing change request, or awaiting re-review) derived from that reviewer's review history, the current head, and whether the reviewer is a declared bot.
- **Declared bot reviewer**: a reviewer named in the issue's effective expected reviewers.
- **Head**: the current head commit of the issue's linked PR as the engine knows it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A declared bot that requested changes on an earlier commit and then reviews the fixed head with `COMMENTED` or `APPROVED` no longer holds the authoritative gate. No operator action is required and no timeout is reached on account of that earlier request.
- **SC-002**: In every case where a human or undeclared reviewer's objection stood before this change, the gate still blocks. Only objections on superseded commits change from "requested changes" to "awaiting re-review", and these still block.
- **SC-003**: For the full table of review histories tested, the gate and the reviewer-verdict view agree on every reviewer's verdict, with zero disagreements.
- **SC-004**: A blocking message for a reviewer awaiting re-review never contains "requested changes" for that reviewer.
- **SC-005**: Every new test fails when its corresponding behaviour is neutralised, and `go test -race ./...` and `go vet ./...` pass.
- **SC-006**: The reported sequence (declared bot `CHANGES_REQUESTED` on A, push B, `COMMENTED` on B) is shown to block on current `main` before the fix and to clear after it, in both unit tests and the simulation twin.

## Assumptions

- "Head" is the current head commit the engine already uses when judging a PR, with the same fail-closed treatment when it is unknown.
- When a reviewer is awaiting re-review, the existing review-wait timeout and escalation still apply unchanged. How the wait clock is anchored is a separate issue.
- An undeclared reviewer whose `CHANGES_REQUESTED` is on an older commit and who submits `COMMENTED` at the head remains blocking. The reason may be worded as awaiting re-review, because that reviewer has given no formal verdict on the head.
- Only the verdict logic changes. Handling of unresolved review threads and review bodies as feedback to address is unaffected.
- A `COMMENTED` review at head from a declared bot is a clean pass for verdict purposes whether or not it contains inline findings; the findings are handled by the existing feedback path.
- Dismissed reviews keep their present handling.

## Out of Scope *(optional)*

- The declined-finding deadlock, where the `CHANGES_REQUESTED` is on the current head. This head-aware change does not fix it. It is tracked in a sibling issue.
- How the review-wait clock is started and reset. Sibling issue.
- Advisory review authority.
- Changes to Pruefer's own behaviour, such as adding an approve mode.
- Any change to what GitHub's own review decision reports.

## Source References *(optional)*

- Community report #2453 (a human-owned thread; it stays open until a release containing the fix ships and is closed by hand with a comment naming that release).
- Prior art: ADR-1250 (review authority), ADR-1283 (`expected_reviewers`), #1539, #2122.
