# Feature Specification: e2e — Conjunctive CI∧Review Gate (Joint-Clear + No Premature Completion)

**Feature Branch**: `fabrik/issue-895`
**Created**: 2026-06-18
**Status**: Draft
**Input**: User description: "test(e2e): conjunctive CI∧review gate — joint-clear + no premature completion"

## Background

The Fabrik engine supports a conjunctive gate: a stage configured with both `wait_for_ci: true` and `wait_for_reviews: true` must satisfy **both** conditions before `stage:<X>:complete` is applied and the issue advances. The 2026-06-17 state-machine review (`notes/state-machine-review-2026-06-17.md`) flagged this as the single biggest untested hole in the e2e suite: there is no described joint-clearing path, no test, and a reviewer who comments during the CI-await window is currently unverified to not be silently dropped.

ADR-056 / #887 (settle-owner) defines the single-advance-owner model where both gates must be satisfied before the advance fires. #890 (spec) makes the joint-clear semantics explicit. This issue verifies the consolidated behavior end-to-end against a real Fabrik instance.

The test must not be written against the legacy two-poll handoff — it should be authored after #887 lands so it asserts the settle-owner-consolidated behavior directly.

**Critical setup risk**: GitHub forbids approving your own PR. The bot identity is `@arbeithand`. A second distinct reviewer is required to submit an approving review and satisfy the `wait_for_reviews` half of the gate. Resolving this identity (second PAT, GitHub App, or API workaround) is Research's primary task. If a real second reviewer is infeasible, the test falls back to a reduced scope (ordering/withholding half + review-timeout path only), deferring the full approval assertion.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Gate Withholds Completion Until Both CI and Review Pass (Priority: P1)

An issue advances to a stage configured with both `wait_for_ci: true` and `wait_for_reviews: true`. After `FABRIK_STAGE_COMPLETE` fires, the engine:
1. Applies `fabrik:awaiting-ci` (withholds `stage:<X>:complete`).
2. Once CI passes, replaces it with `fabrik:awaiting-review` (still withholds complete).
3. Once an approving review arrives, clears `fabrik:awaiting-review` and applies `stage:<X>:complete`.

At no point does `stage:<X>:complete` appear while either gate is unsatisfied.

**Why this priority**: This is the entire conjunctive gate contract. A regression where either gate is silently skipped would cause stages to advance on partial signals — a correctness bug.

**Independent Test**: A `TestConjunctiveCIReviewGate` test files an issue through the both-gates stage, then asserts the label sequence and final complete timing.

**Acceptance Scenarios**:

1. **Given** an issue reaches the both-gates stage with a linked PR, **When** `FABRIK_STAGE_COMPLETE` fires (Claude emits the marker), **Then** `fabrik:awaiting-ci` is applied and `stage:<X>:complete` is absent.

2. **Given** `fabrik:awaiting-ci` is present, **When** all required CI checks pass, **Then** `fabrik:awaiting-ci` is cleared and `fabrik:awaiting-review` is applied — `stage:<X>:complete` is still absent.

3. **Given** `fabrik:awaiting-review` is present, **When** an approving review is submitted by a distinct reviewer, **Then** `fabrik:awaiting-review` is cleared, `stage:<X>:complete` is applied, and the issue advances to the next stage.

---

### User Story 2 — Reviewer Comment During CI-Await Window Is Not Dropped (Priority: P1)

A reviewer posts a comment on the PR while `fabrik:awaiting-ci` is still present (CI has not yet passed). The comment must not be silently discarded — it should receive a 👀 reaction (acknowledged) and eventually a 🚀 reaction or a response, once the engine processes comments in that state.

**Why this priority**: The state-machine review flagged that reviewer comments during CI-await may be silently ignored until CI clears. This must be verified.

**Independent Test**: Post a PR comment immediately after `fabrik:awaiting-ci` appears. Then wait for the 👀 reaction or a reply before CI clears (or confirm it appears after CI clears but before gate advances).

**Acceptance Scenarios**:

1. **Given** `fabrik:awaiting-ci` is present and a comment is posted on the PR, **When** the comment is eventually processed (either before or after CI clears), **Then** the comment receives at minimum a 👀 acknowledgment reaction.

---

### User Story 3 — Reduced Scope Fallback When Second Reviewer Is Infeasible (Priority: P2)

If no distinct reviewer identity is available, the test asserts only the ordering/withholding half:
- `fabrik:awaiting-ci` appears and `stage:<X>:complete` is withheld.
- After CI passes, `fabrik:awaiting-review` appears and `stage:<X>:complete` remains withheld.
- The review-timeout path fires (after `FABRIK_REVIEW_WAIT_TIMEOUT`) and `fabrik:awaiting-review` clears naturally, then `stage:<X>:complete` is applied.

The full approval path (Story 1, step 3) is documented as deferred with an explicit note in the test.

**Why this priority**: A test asserting the first two gates is valuable even without proving the third. The timeout path is real behavior that also needs coverage.

**Independent Test**: Set a short `FABRIK_REVIEW_WAIT_TIMEOUT` (e.g. 5 minutes) in the test bed `.env` and wait for the timeout to clear the review gate.

---

### Edge Cases

- If CI passes before the reviewer comment is posted (race condition in the test), the comment must still be acknowledged after the gate transitions to `awaiting-review`. The test should post the comment within a tight window after `awaiting-ci` appears and log a warning if the window is missed.
- If both `fabrik:awaiting-ci` and `fabrik:awaiting-review` are somehow present simultaneously (implementation bug), the test must fail with a clear diagnostic.
- The `wait_for_reviews` half uses GitHub's PR review API. The required reviewer must be requested on the PR, not just any commenter. The harness helper `SubmitPRReview` must submit via the Reviews API (not a comment).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A new e2e test `TestConjunctiveCIReviewGate` in `tests/e2e/conjunctive_gate_test.go` files an issue through a stage with both `wait_for_ci: true` and `wait_for_reviews: true`, asserts label transitions in order, and asserts no premature completion.
- **FR-002**: The both-gates stage must be configured in the test bed (`.fabrik/stages/` of `~/dev/fabrik-test`). Validate is the natural host; alternatively a dedicated test stage named `TestGate` may be added. Research must specify which approach is cleaner given the existing stage config.
- **FR-003**: A new harness helper `SubmitPRReview(t, env, repo, prNumber, event)` submits a PR review via the GitHub REST API (`POST /repos/{owner}/{repo}/pulls/{pull_number}/reviews`). `event` is `"APPROVE"` or `"REQUEST_CHANGES"`. The helper uses the reviewer PAT (a second identity, if available) or skips with `t.Skip` if no second identity is configured.
- **FR-004**: A new harness helper `PRReviewDecision(t, env, repo, prNumber)` returns the current PR review decision (`"APPROVED"`, `"CHANGES_REQUESTED"`, `"REVIEW_REQUIRED"`, or `""`). Used to assert the gate has been satisfied.
- **FR-005**: A new harness helper `WaitForPRReviewDecision(t, env, repo, prNumber, decision, timeout)` polls until the named PR reaches the given review decision, or fails on timeout.
- **FR-006**: A new harness helper `RequestPRReviewer(t, env, repo, prNumber, reviewer)` requests a specific reviewer on the PR via `POST /repos/{owner}/{repo}/pulls/{pull_number}/requested_reviewers`. Needed so `wait_for_reviews` has an actual outstanding reviewer request.
- **FR-007**: The test posts a PR comment while `fabrik:awaiting-ci` is present and asserts the comment eventually receives a 👀 reaction (or a response), confirming the comment is not silently dropped.
- **FR-008**: The test asserts `stage:<X>:complete` is absent throughout the CI-await window (a 2-minute withheld check, as in `TestCIFixReinvoke`) and again throughout the review-await window.
- **FR-009**: Research must resolve the second reviewer identity. If a second PAT for a distinct GitHub account (e.g. a test user) is available, `SubmitPRReview` uses it. If not, `SubmitPRReview` calls `t.Skip` with an instructional message and User Story 3 (timeout fallback) is exercised instead.
- **FR-010**: `tests/e2e/README.md` scenario and regression-coverage tables are updated with a row for `TestConjunctiveCIReviewGate`, referencing ADR-056 and this issue (#895).
- **FR-011**: The `ci-fix-sentinel` (or a new sentinel job, per Research's recommendation) must trigger a required CI check on the test PR so the `wait_for_ci` gate fires with a real failing-then-passing check, not a synthetic simulation.

### Key Entities

- **Both-gates stage**: A stage in the test bed configured with `wait_for_ci: true` and `wait_for_reviews: true`. Either the existing Validate stage (modified) or a new `TestGate` stage.
- **SubmitPRReview (harness)**: A new helper that submits a PR review via the GitHub Reviews REST API using a reviewer PAT (distinct from `@arbeithand`).
- **Reviewer identity**: A second GitHub account (or GitHub App) capable of submitting an approving review on a PR authored by `@arbeithand`. The main setup risk; resolved by Research.
- **Joint-clear**: The engine behavior where both `fabrik:awaiting-ci` and `fabrik:awaiting-review` must be cleared before `stage:<X>:complete` is applied — the settlement ADR-056 defines.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `TestConjunctiveCIReviewGate` passes end-to-end: issue reaches `stage:<X>:complete` only after both `fabrik:awaiting-ci` and `fabrik:awaiting-review` have been applied and cleared in order.
- **SC-002**: `stage:<X>:complete` is never observed while either gate label is present (the withheld-window check never fires).
- **SC-003**: A reviewer comment posted during the CI-await window receives a 👀 reaction before or after CI clears (not silently dropped).
- **SC-004**: `SubmitPRReview`, `PRReviewDecision`, `WaitForPRReviewDecision`, and `RequestPRReviewer` helpers are added to `harness.go` and reusable by any future scenario.
- **SC-005**: If a second reviewer is unavailable, `TestConjunctiveCIReviewGate` passes in reduced scope (fallback path) — the withholding half and review-timeout path pass, with the approval path explicitly skipped via `t.Skip`.
- **SC-006**: `go test ./tests/e2e/ -run TestConjunctiveCIReviewGate -tags e2e` passes cleanly against the live test bed.

## Assumptions

- The test bed Fabrik instance is running with the same config as all other e2e tests.
- `ci-fix-sentinel` (or a new dedicated sentinel) is enrolled as a required status check on `handarbeit/fabrik-test-alpha/main` — the same prerequisite as `TestCIFixReinvoke`. The test skips gracefully if not.
- The issue body for this test can be a minimal trivial change (same template as `TestCIFixReinvoke`) so the full pipeline completes quickly up to the gate stage.
- If a `TestGate` dedicated stage is introduced, it must be added to the test-bed project board columns; Research must verify this does not break the existing `TestSmokeSingleRepoFullPipeline` path.
- The review-timeout fallback path assumes `FABRIK_REVIEW_WAIT_TIMEOUT` is configurable in the test bed `.env`. Research must confirm this.

## Out of Scope

- Unit-testing the conjunctive gate logic in isolation (engine unit tests are separate; this is e2e only).
- Verifying `wait_for_ci` or `wait_for_reviews` individually (each is already exercised by `TestCIFixReinvoke` and the review-gate machinery respectively).
- Bot-reprompted escalation ladder (`fabrik:bot-reprompted`) — that is a distinct scenario.
- Verifying the conjunctive gate with `fabrik:cruise` (cruise × gate interaction is out of scope for this issue).
- ADR changes beyond a note that the conjunctive gate now has e2e coverage.

## Source References

- `tests/e2e/harness.go` — existing helpers; `SubmitPRReview`, `PRReviewDecision`, `WaitForPRReviewDecision`, `RequestPRReviewer` are new additions
- `tests/e2e/ci_fix_reinvoke_test.go` — pattern for CI-gate wait + withheld-window check
- `adrs/056-consolidate-convergence-gate-recovery.md` — settle-owner model; conjunctive gate semantics
- `notes/state-machine-review-2026-06-17.md` — "Angle 1 — high" section that flagged this gap
- `engine/ci.go` — CI gate implementation
- `engine/poll.go` — review gate implementation and `checkAwaitingReview`
- `docs/state-machine.md` — as-built specification for label transitions and gate semantics
- Issue #887 (settle-owner), #890 (spec for joint-clear) — prerequisites
