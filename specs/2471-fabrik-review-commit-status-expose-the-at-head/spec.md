# Feature Specification: `fabrik/review` commit status exposing the at-head review verdict

**Feature Branch**: `fabrik/issue-2471`
**Created**: 2026-11-15
**Status**: Specified
**Input**: User description: the original request, verbatim:

> ## Problem
>
> A repository can hold its expensive CI job until the review bot has reviewed the head commit, but only
> by working around Fabrik. Reported in #2239 (@totalslacker): a CI-side `pruefer-gate` job that the
> expensive `test` job depends on, polling the PR's reviews on its own runner. Because Fabrik does not
> know the gate exists, `test` just looks pending for a long time:
>
> - the CI wait timeout and the `fabrik:awaiting-ci` backstop count the review wait as CI time;
> - Fabrik cannot tell "waiting for review" from "CI stuck".
>
> ## Requirements
>
> **R1 — A Fabrik-owned commit status.** On a stage with `wait_for_reviews: true`, Fabrik sets a commit
> status (working name `fabrik/review`) on the PR head: `pending` while an expected reviewer's review of
> the head is outstanding, `success` when every expected reviewer's review **of the head** asks for no
> changes, `failure` when one requests changes. A workflow's expensive job can then depend on it.
>
> **R2 — One verdict.** The status is derived from the per-reviewer at-head verdict helper introduced
> by #2465, never from a second collapse of the review history. Blocked by #2465.
>
> **R3 — Attribute the wait.** While Fabrik's own status is `pending`, the CI wait timeout and the
> `fabrik:awaiting-ci` backstop treat the time as a review wait, not a CI stall, and say so in any
> pause comment.
>
> **R4 — Opt-in.** Off by default; enabled per stage or per repo. No behaviour change otherwise.
>
> **R5 — Permissions.** Writing a commit status needs `statuses: write` under GitHub App auth (and
> `repo` scope with a PAT). Adding a required App permission is a startup-refusing change for existing
> installations (compare #2105), so decide whether the feature is optional-permission, refuse-if-enabled
> or required, and document it.
>
> ## Scope
>
> **In scope:** R1–R5, `docs/state-machine.md`, `docs/USER_GUIDE.md` (with a worked workflow example), an
> ADR, tests and a sim twin.
> **Out of scope:** the report's option 1 (review the head before marking ready — tracked as the open
> design question on the report); treating a declined finding as resolved (declined, see #2466 R4);
> Pruefer's own behaviour.
>
> ## Acceptance
>
> 1. Status goes `pending` → `success` when the expected reviewer's review of the head is clean, and to
>    `failure` on a change request at the head.
> 2. A new push resets it to `pending` for the new head.
> 3. While it is `pending`, a CI wait is not reported as a CI stall.
> 4. Off by default; no status written when not enabled.
> 5. Tests demonstrated non-vacuous; `go test -race ./...` and `go vet ./...` pass.
>
> ## Notes
>
> Reported in #2239 — a human-owned community thread. Mention it in the PR body by bare number only. Do
> not place any closing keyword (`Closes`/`Fixes`/`Resolves`) adjacent to that number in any form,
> including inside a negated or disclaiming sentence. Closed by hand once a release ships.

## Background

A repository owner wants an expensive CI job to wait until the review bot has reviewed the exact commit being tested. Today the only way to do that is to work around Fabrik: a CI-side gate job polls the PR's reviews on its own runner, and the expensive job depends on it (reported in #2239). Fabrik does not know that gate exists, so from its point of view the expensive job is simply a CI check that has been pending for a long time. Two things go wrong:

- the CI wait timeout and the `fabrik:awaiting-ci` backstop count the review wait as CI time, so an item can be escalated as a CI stall when it is really waiting for a reviewer;
- Fabrik, an operator or an overseer cannot tell "waiting for review" from "CI stuck", and any pause comment names the wrong cause.

The fix is for Fabrik to publish the review verdict itself, as a commit status on the PR head that a workflow can depend on. Fabrik already computes a per-reviewer, at-head verdict (introduced by #2465, now merged). The status is a second presentation of that same verdict, and it also gives Fabrik the knowledge to attribute the wait correctly.

Option 1 from the report (reviewing the head before the PR is marked ready) and the treatment of declined findings are separate questions and are not part of this issue.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A workflow waits on the review verdict (Priority: P1)

A repository owner enables the feature on a stage that waits for reviews and declares its expected reviewer. They make their expensive CI job depend on the `fabrik/review` status. The job does not start while the review of the head is outstanding, starts once the review of the head is clean, and does not run while the reviewer is requesting changes.

**Why this priority**: This is the whole point of the feature and the thing the reporter had to work around.

**Independent Test**: With the feature on, open a PR at a stage with an expected reviewer. Observe the status on the head commit move `pending` → `success` when the reviewer's review of that head asks for no changes, and `pending` → `failure` when the review of that head requests changes.

**Acceptance Scenarios**:

1. **Given** the feature is enabled and the expected reviewer has not yet reviewed the head, **When** Fabrik evaluates the PR, **Then** the head commit has a `fabrik/review` status of `pending`.
2. **Given** the status is `pending`, **When** the expected reviewer submits a review of the head that asks for no changes, **Then** the status becomes `success`.
3. **Given** the status is `pending`, **When** the expected reviewer submits a review of the head that requests changes, **Then** the status becomes `failure`.
4. **Given** the status is `success` or `failure` for a head, **When** a new commit is pushed, **Then** the new head has a `pending` status until the reviewer has reviewed it.
5. **Given** the expected reviewer's only change request is on an earlier commit, **When** the head has moved on, **Then** the status is `pending` (awaiting re-review), not `failure`.

---

### User Story 2 - A review wait is not reported as a CI stall (Priority: P1)

An operator whose CI is held on the review verdict sees the item reported, and timed, as waiting for a review. The CI wait timeout and the `fabrik:awaiting-ci` backstop do not escalate it as a stuck CI run, and any pause comment names the review as the thing being waited on.

**Why this priority**: This is the other half of the reported problem. Without it the status would exist, but items would still be escalated for the wrong reason.

**Independent Test**: With the feature on and the status `pending`, let the item sit longer than the CI wait timeout and confirm it is not paused as a CI stall; if the existing review-wait handling does pause it, confirm the comment names the review wait.

**Acceptance Scenarios**:

1. **Given** the status is `pending` and the CI wait would otherwise have timed out, **When** the CI wait timeout and the `fabrik:awaiting-ci` backstop are evaluated, **Then** the elapsed time is treated as a review wait and the item is not reported or escalated as a CI stall.
2. **Given** an item is paused while the status is `pending`, **When** the pause comment is written, **Then** it says the item was waiting for the review of the head rather than for CI.
3. **Given** the status is `success` or `failure`, **When** CI is slow, **Then** the existing CI timeout behaviour applies unchanged.

---

### User Story 3 - Existing installations are unaffected (Priority: P1)

An operator who does not use the feature sees no change: no status is written, no extra permission is checked, and an installation that lacks the permission starts exactly as before. An operator who turns the feature on without the permission is told clearly at startup.

**Why this priority**: Adding a required permission would stop every existing installation from starting (as happened with #2105) for a feature most will not use.

**Independent Test**: Run a daemon under App auth with the feature off and the permission absent, then with the feature on and the permission absent, then with the feature on and the permission present, and compare startup outcomes.

**Acceptance Scenarios**:

1. **Given** the feature is not enabled anywhere and the App installation lacks `statuses: write`, **When** the daemon starts, **Then** it starts and nothing about the permission is checked or reported.
2. **Given** the feature is enabled for any stage or repo and the installation lacks `statuses: write`, **When** the daemon starts (or `fabrik init --github-app` runs), **Then** it refuses, names the missing permission and uses the shared permission-shortfall remedy wording.
3. **Given** the feature is enabled and the installation has `statuses: write`, **When** the daemon starts, **Then** it starts normally.
4. **Given** PAT mode and a runtime write refused by GitHub, **When** the status write fails, **Then** the failure is logged and reported once on the warnings panel, no item is paused and no gate is blocked.

---

### Edge Cases

- The expected reviewer reviews an older commit and then the head moves: the status is `pending`, never `failure`, for the new head.
- The expected reviewer first requests changes at the head and later submits a clean review at the same head: the status moves from `failure` to `success`, following the same at-head verdict the review gate uses.
- A review is dismissed: the verdict follows the review history as the shared helper reads it; the status never disagrees with the review gate.
- Several expected reviewers: `pending` while any is outstanding, `failure` if any requests changes at the head, `success` only when every one is clean at the head.
- The PR is closed, merged or has no head yet: no status is written for it.
- A status write fails (permission revoked, API error): logged and reported once; the review gate, CI gate and items are unaffected, and the write is retried on a later evaluation.
- The review wait itself runs out under the existing review-wait handling: that existing escalation is unchanged, and its comment names the review wait.
- A declined finding (a request for changes the stage answered without a commit) is not treated as resolved by this feature; it stays `failure` until the reviewer's verdict at the head changes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: On a stage with `wait_for_reviews: true` for which the feature is enabled, Fabrik MUST publish a commit status named `fabrik/review` on the head commit of the item's open PR.
- **FR-002**: The status MUST be `pending` while any expected reviewer's review of the head is outstanding, `success` when every expected reviewer's review of the head asks for no changes, and `failure` when any expected reviewer's review of the head requests changes.
- **FR-003**: The status MUST be derived from the same per-reviewer, at-head verdict the review gate uses (the helper introduced by #2465). There MUST be no second collapse of the review history, so the status and the gate cannot disagree about the same head.
- **FR-004**: A new head commit MUST produce a `pending` status for that head until the expected reviewer has reviewed it; a verdict on an earlier commit MUST NOT yield `success` or `failure` for the new head.
- **FR-005**: While the status for the current head is `pending`, the CI wait timeout and the `fabrik:awaiting-ci` backstop MUST treat the elapsed time as a review wait and MUST NOT report or escalate it as a CI stall.
- **FR-006**: Any pause comment written while the status is `pending` MUST state that the item was waiting for the review of the head, not for CI.
- **FR-007**: The feature MUST be off by default and enabled explicitly, per stage or per repo. With it off, no status is written and no existing behaviour changes.
- **FR-008**: Under GitHub App auth, `statuses: write` MUST NOT be added to the permissions required at every startup.
- **FR-009**: When the feature is enabled for any stage or repo under App auth and the installation lacks `statuses: write`, startup (and `fabrik init --github-app`) MUST refuse, name the missing permission and reuse the shared permission-shortfall remedy wording.
- **FR-010**: When the feature is not enabled, the `statuses: write` permission MUST be neither checked nor required, and an installation without it MUST start exactly as it does today.
- **FR-011**: Under PAT auth the documentation MUST state that the token needs `repo` scope (or fine-grained commit-statuses write access).
- **FR-012**: A commit-status write that GitHub refuses or that fails at runtime MUST be logged and reported once on the warnings panel, MUST NOT pause any item and MUST NOT block any gate.
- **FR-013**: The feature MUST be documented in `docs/state-machine.md` and `docs/USER_GUIDE.md`, the user guide including a worked workflow example in which an expensive job depends on the status, and `docs/llms-full.txt` MUST be regenerated with those changes.
- **FR-014**: An ADR MUST record the permission decision (refuse-if-enabled, not required, not silently optional) and the attribution decision.
- **FR-015**: Tests MUST cover the permission cases (enabled and missing is refused, disabled and missing starts, enabled and present starts), each status transition, the push reset, the timeout attribution and the pause comment wording. A sim twin MUST exist, and the tests MUST be demonstrated non-vacuous by neutralising the behaviour and seeing them fail. `go test -race ./...` and `go vet ./...` MUST pass.
- **FR-016**: The pull request for this work MUST mention the community report #2239 by bare number only and MUST NOT place any closing keyword next to it in any form, including a negated or disclaiming sentence. The report is closed by hand after a release ships.

### Key Entities

- **`fabrik/review` status**: a Fabrik-owned commit status on a PR head with the states `pending`, `success` and `failure`, whose value is the at-head review verdict of the stage's expected reviewers.
- **At-head review verdict**: the per-reviewer judgement, already used by the review gate, of whether each expected reviewer has reviewed the current head and whether that review asks for changes.
- **Review wait**: time an item spends waiting for an expected reviewer's review of the head, distinct from CI time.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a run with the feature on, the status for a head goes `pending` → `success` when the expected reviewer's review of that head is clean and `pending` → `failure` on a change request at that head, in every tested scenario.
- **SC-002**: After a push, the new head shows `pending` in 100% of tested scenarios, including when the previous head had `success` or `failure`.
- **SC-003**: An item held only on the review verdict is never paused or reported as a CI stall by the CI wait timeout or the `fabrik:awaiting-ci` backstop, and any pause comment names the review wait.
- **SC-004**: With the feature off, zero status writes occur and an installation lacking `statuses: write` starts as before.
- **SC-005**: With the feature on and `statuses: write` missing under App auth, startup is refused with a message naming the permission; with it present, startup succeeds.
- **SC-006**: A failed status write causes no pause and no blocked gate, and produces exactly one warnings-panel entry per condition.
- **SC-007**: Each new test fails when the behaviour it covers is neutralised, and `go test -race ./...` and `go vet ./...` pass.

## Assumptions

- The status context name is `fabrik/review` (the issue's working name); it is fixed once chosen and documented.
- The opt-in is a per-stage setting that requires `wait_for_reviews: true` and declared expected reviewers; a repo-wide switch is a convenience over the same setting. The exact key names are a Plan-stage decision.
- The feature needs at least one expected reviewer to wait on. Enabling it on a stage that declares none is reported as a configuration problem and writes no status, so a dependent workflow is not held on a verdict that can never arrive.
- The status reports the verdict only. If the existing review-wait handling escalates and pauses an item, the status stays `pending` and the pause comment says why; this feature adds no new review-wait timeout.
- Attribution changes how the elapsed time is classified and worded, not the lengths of the existing timeouts.
- "Asks for no changes" means the same non-blocking verdicts the existing gate treats as non-blocking at the head.
- Under GitHub App auth the daemon already holds an installation token, so no new credential mechanism is needed beyond the `statuses: write` grant.

## Out of Scope

- Reviewing the head before the PR is marked ready (the report's option 1, tracked as the open design question on the report).
- Treating a declined finding as resolved (see #2466 R4).
- Any change to Pruefer's own behaviour.
- Adding `statuses: write` to the permissions required at every startup.
- Changing the length of the CI wait timeout or the review-wait timeout.
- A new status for CI or other gates; only the review verdict is published.

## Source References

- #2239 (community report, human-owned thread; mentioned for context)
- #2465 (head-aware review verdict helper this status is derived from)
- #2105 (precedent: a required App permission refused existing installations at startup)
- #2466 (declined-finding handling, out of scope here)
- Steering comment on this issue deciding R5 (refuse if enabled, no change when disabled, PAT note, failed writes reported once and never blocking)
