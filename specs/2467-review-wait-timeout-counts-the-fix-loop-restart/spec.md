# Feature Specification: Review-wait timeout measures reviewer silence on the current head

**Feature Branch**: `fabrik/issue-2467`
**Created**: 2026-10-29
**Status**: Specified
**Input**: User description:

> ## Problem
>
> The review-wait timeout measures how long the fix loop has run, not how long the reviewer has been
> silent. `checkAwaitingReviewTimeout` (`engine/reviews.go`) anchors on
> `labelAppliedAt(…, "fabrik:awaiting-review")`; the label stays on through every fix round, so the
> clock starts at the **first** application and a push that asks for a new review never resets it.
> Reported in #2453 (@totalslacker).
>
> Reported instance, chronicle#529 / PR #534 (2026-10-24): `awaiting-review` at 4:28 PM; two fix
> rounds; last fix pushed `b15bf0d7` at 4:45:47 PM; `review wait timeout elapsed` at 4:46:18 PM — 31
> seconds after the push, 17.5 minutes after the label, before the bot had reviewed the new head.
>
> ## Requirements
>
> **R1 — Measure reviewer silence on the current head.** The wait anchor is the later of the label's
> applied time and the moment the current PR head appeared. A head change after the gate began waiting
> restarts the clock.
>
> **R2 — Anchor on a GitHub-sourced time, never the host clock.** Use a recorded fact for the head's
> arrival (the PR `synchronize` event or the head commit's push time as GitHub reports it), so a
> restart neither resets nor extends the wait.
>
> **R3 — Bounded.** A loop of pushes can no longer be ended by the timeout, so it must still end:
> `MaxReviewCycles` already bounds review reinvokes; confirm it does on this path and pin it.
>
> **R4 — The bot re-prompt ladder follows the same anchor** (`fabrik:bot-reprompted` timing), or
> documents why not.
>
> ## Scope
>
> **In scope:** the review-wait anchor, the bot re-prompt ladder's timing, `docs/state-machine.md`,
> tests and a sim twin.
> **Out of scope:** head-aware verdicts (#2465); the declined-finding exit (#2466);
> `FABRIK_REVIEW_WAIT_TIMEOUT`'s default.
>
> ## Acceptance
>
> 1. A push after `awaiting-review` restarts the wait; the reported 31-second timeout does not occur.
>    Reproduced red against `main` first.
> 2. No push → the timeout fires as today.
> 3. A restart mid-wait keeps the anchor (R2).
> 4. A push loop still ends within `MaxReviewCycles` (R3).
> 5. Tests demonstrated non-vacuous; `go test -race ./...` and `go vet ./...` pass.
>
> ## Notes
>
> Reported in #2453 — a human-owned community thread. Mention it in the PR body by bare number only.
> Do not place any closing keyword (`Closes`/`Fixes`/`Resolves`) adjacent to that number in any form,
> including inside a negated or disclaiming sentence. Closed by hand once a release ships.

## Background

When a stage with `wait_for_reviews: true` completes, the engine puts `fabrik:awaiting-review` on the item and waits for the outstanding reviewers (human or bot) to respond. The wait is bounded by the review-wait timeout (`FABRIK_REVIEW_WAIT_TIMEOUT`). If the timeout elapses, the item is paused for a human.

Today the timeout is anchored on when `fabrik:awaiting-review` was **first applied**. That label stays on through every fix round. Each time the engine addresses review feedback and pushes, the push asks the reviewer for a fresh look at a new head. The clock does not restart. The timeout therefore measures how long the whole fix loop has run, not how long the reviewer has been silent on the commit they are being asked to review.

The reported instance was chronicle#529 / PR #534 on 2026-10-24. The label was applied at 4:28 PM, and two fix rounds followed. The last fix was pushed at 4:45:47 PM, and the timeout fired at 4:46:18 PM. That is 31 seconds after the push and 17.5 minutes after the label, before the bot had a chance to review the new head. The item was paused for a "silent" reviewer that had never been given time to speak. The report came from a community thread, #2453.

The bot re-prompt ladder has the same flaw. It is the escalation that @mentions unresponsive bot reviewers (Phase 1, marked by `fabrik:bot-reprompted`) before pausing (Phase 2). Its Phase 1 deadline is measured from the same label time, and its Phase 2 deadline is measured from the `fabrik:bot-reprompted` label.

Related, but separate work:
- #2465 is the head-aware authoritative review verdict (which review counts at which head). This issue is blocked on it.
- #2466 is the declined-finding exit.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A new push gives the reviewer a fresh wait (Priority: P1)

An operator has an item waiting on a bot reviewer. The engine addresses a review finding and pushes a new head. The reviewer is given the full review-wait window, counted from the new head's arrival on GitHub, to review it. The item is not paused for silence on a commit the reviewer was only just shown.

**Why this priority**: This is the reported defect. It pauses items that were making progress and wastes the fix work already done.

**Independent Test**: Put an item into the review-wait state, advance time near the timeout, push a new head, and advance time to just past the original timeout. Assert the item is still waiting. Then advance a full window past the push and assert the timeout fires.

**Acceptance Scenarios**:

1. **Given** an item has waited on `fabrik:awaiting-review` for most of the timeout window, **When** a new head is pushed to its PR and the original timeout instant then passes, **Then** the gate keeps waiting and no pause is applied.
2. **Given** the same item, **When** the full timeout window elapses after the new head's arrival with no reviewer response, **Then** the timeout fires as it does today.
3. **Given** the reported scenario (label at T, last push at T+17m, check at T+17.5m), **When** the gate is evaluated, **Then** it does not time out.

---

### User Story 2 - A wait with no new push is unchanged (Priority: P1)

An operator has an item waiting on a reviewer, and nobody pushes. The timeout fires at the same moment as before this change.

**Why this priority**: The change must not weaken or lengthen the timeout when nothing asks for a new review.

**Independent Test**: Wait on an item with no head change and assert the timeout fires at the label time plus the window, including when the head predates the label.

**Acceptance Scenarios**:

1. **Given** an item whose PR head arrived before `fabrik:awaiting-review` was applied, **When** the window elapses from the label's applied time, **Then** the timeout fires exactly as today.

---

### User Story 3 - The anchor survives a restart (Priority: P1)

The daemon restarts mid-wait. After the restart, the wait is neither reset (which would let a silent reviewer wait forever) nor extended. It keeps the anchor it had.

**Why this priority**: Operators restart the daemon often (upgrades, SIGHUP). A host-clock anchor would make the timeout depend on uptime.

**Independent Test**: Establish a wait with a pushed head, restart the engine, and assert the timeout fires at the same instant as without the restart.

**Acceptance Scenarios**:

1. **Given** an item waiting after a push, **When** the engine restarts and the window then elapses from the push's GitHub-reported time, **Then** the timeout fires at that instant, not at restart time plus the window.
2. **Given** the engine has just restarted with no recorded head-arrival fact, **When** the gate is evaluated, **Then** it falls back to the label's applied time and never invents a time from the host clock.

---

### User Story 4 - A push loop still ends (Priority: P2)

Restarting the clock on each push means the timeout alone can no longer end a loop in which every round pushes a new head. That loop must still end.

**Why this priority**: Removing the timeout as a backstop for push loops must not leave an unbounded loop.

**Independent Test**: Simulate a reviewer that asks for changes on every head and a fix round that pushes every time. Assert the item is paused for the review-cycle limit after `MaxReviewCycles` rounds, without the timeout ever firing.

**Acceptance Scenarios**:

1. **Given** a review-reinvoke loop where every fix pushes a new head and the reviewer never settles, **When** `MaxReviewCycles` is reached, **Then** the item is paused for the review-cycle limit.
2. **Given** a reinvoke that makes no commit, **When** it completes, **Then** the existing no-op cycle exemption behaves as it does today.

---

### User Story 5 - The bot re-prompt ladder follows the new head (Priority: P2)

A bot reviewer is outstanding and the ladder is engaged. A new push restarts the ladder rather than leaving a stale re-prompt state that would pause the item on the old clock.

**Why this priority**: The ladder is the other timeout driven off the same label. Left alone, it would reproduce the same premature pause by another path.

**Independent Test**: Engage Phase 1 of the ladder, push a new head, and assert the ladder restarts from the new head rather than escalating to Phase 2 on the old timing.

**Acceptance Scenarios**:

1. **Given** the ladder has fired Phase 1 (`fabrik:bot-reprompted` applied), **When** a new head arrives, **Then** the ladder cycle is reset and the bot is given a fresh Phase 1 window counted from the new head.
2. **Given** no new head arrives after Phase 1, **When** the Phase 2 window elapses, **Then** Phase 2 fires as today.

---

### Edge Cases

- The head changes more than once during a wait: the anchor is the arrival of the **current** head.
- The head's GitHub-reported arrival time is unavailable (unreadable event, no event found): fall back to the label's applied time, not the host clock, and never wait longer than today.
- The head arrived before the label was applied (the ordinary first-round case): the label's applied time is the later one, so behaviour is unchanged.
- A force-push or rebase changes the head with no new content: any head change counts. Distinguishing a "real" re-review request from a rebase is not attempted.
- `fabrik:awaiting-review` is removed and re-applied (a new gate cycle): the anchor is the later of the new label time and the current head's arrival.
- A pause lifted by a human: the existing anchoring rule for resumed waits is kept. Only the head-arrival input is added.
- Mixed bot and human outstanding reviewers: the pause path is the same one, so the anchor change applies to it as well.
- A review arriving at the current head still clears the gate through the existing path. Which review counts at which head is #2465's business, not this issue's.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The review-wait timeout's anchor MUST be the later of (a) the applied time of `fabrik:awaiting-review` and (b) the arrival time of the PR's current head, so that a head change after the gate began waiting restarts the clock.
- **FR-002**: The head-arrival time MUST come from a time GitHub reports (the PR `synchronize` event time, or the head commit's push time), never from the host clock.
- **FR-003**: The head-arrival time MUST be recoverable after a daemon restart, so a restart neither resets nor extends the wait. When no GitHub-sourced arrival time can be determined, the anchor MUST fall back to the label's applied time.
- **FR-004**: With no head change after `fabrik:awaiting-review` was applied, the timeout MUST fire at the same instant as before this change.
- **FR-005**: A loop in which every fix round pushes a new head MUST still terminate, bounded by `MaxReviewCycles` through `pauseForReviewCycleLimit`. The bound on this path MUST be confirmed and pinned by a test.
- **FR-006**: The bot re-prompt ladder MUST follow the same anchor. Phase 1 timing MUST be measured from the same later-of anchor. A head change after `fabrik:bot-reprompted` was applied MUST end that gate cycle (remove the label) so the ladder restarts from the new head. Phase 2 timing, with no head change, is unchanged.
- **FR-007**: A rejection or failure to read the head's arrival time MUST NOT cause a timeout earlier than the label-only anchor would, and MUST NOT produce a pause.
- **FR-008**: `docs/state-machine.md` MUST describe the new anchor, the ladder behaviour on a head change, and the `MaxReviewCycles` bound on the push-loop path. The regenerated `docs/llms-full.txt` bundle MUST be committed with it.
- **FR-009**: The change MUST be covered by unit tests and a sim twin that are demonstrated non-vacuous (they fail when the behaviour is neutralised). The reported scenario MUST be reproduced as a failing test against `main` before the fix.

### Key Entities

- **Review-wait anchor**: The instant from which the review-wait timeout (and the ladder's Phase 1) is measured. It was the label's applied time. It becomes the later of the label time and the current head's arrival.
- **Head arrival**: The GitHub-reported time at which the PR's current head commit appeared.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the reported scenario (label, two fix rounds, a push seconds before evaluation), the item is not paused for a review-wait timeout. The reviewer gets the full configured window counted from the last push.
- **SC-002**: For an item with no head change since the label was applied, the timeout fires at the same instant as it does on `main` today.
- **SC-003**: Restarting the daemon during a wait changes the timeout instant by zero.
- **SC-004**: A push-every-round loop is paused for the review-cycle limit after at most `MaxReviewCycles` rounds, and the review-wait timeout never ends it.
- **SC-005**: The new tests fail when the head-arrival input is neutralised and pass otherwise. `go test -race ./...` and `go vet ./...` pass.

## Assumptions

- "Head change" means any change of the PR head commit SHA after the gate began waiting, including a force-push or rebase. This is the simplest GitHub-observable proxy for "a push asks for a new review". It errs toward giving the reviewer more time, and `MaxReviewCycles` bounds the loop.
- The ladder resets on a head change. A head change after `fabrik:bot-reprompted` was applied ends that gate cycle, and the next evaluation re-enters Phase 1 from the new head. This is chosen over leaving the ladder on its old clock, which would reproduce the same defect through the other path. Documenting the opposite was the alternative R4 allowed.
- Head-aware verdicts (#2465) land first, as the dependency says. This issue only changes when the wait *times out*, not which reviews clear the gate.
- No new configuration is added, and `FABRIK_REVIEW_WAIT_TIMEOUT`'s default is unchanged.
- A recorded GitHub fact for head arrival is available or derivable. If it is not, the label-time fallback is acceptable (FR-003, FR-007).

## Out of Scope

- Head-aware verdicts and which review counts at which head (#2465).
- The declined-finding exit (#2466).
- Changing `FABRIK_REVIEW_WAIT_TIMEOUT`'s default or adding new timeout settings.
- Distinguishing a content-bearing push from a rebase or force-push with identical content.
- Changes to the CI-wait timeout (`fabrik:awaiting-ci`), which has its own anchor.

## Source References

- Reported in community thread #2453 (mention in the PR body by bare number only).
- Review gate and ladder: `docs/state-machine.md` (review gate, bot re-prompt ladder, `ReviewWaitTimeout` semantics).
- Reported instance: chronicle#529 / PR #534, 2026-10-24.
