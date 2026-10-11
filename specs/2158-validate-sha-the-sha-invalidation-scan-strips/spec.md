# Feature Specification: SHA-invalidation scan must not strip Validate completion from landed items or engine catch-up heads

**Feature Branch**: `fabrik/issue-2158`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

`settleSHAInvalidationScan` (`engine/poll_settle.go`) strips `stage:Validate:complete` from items that have already landed.

It skips an item only when:
- no Validate-completion SHA was recorded;
- the linked PR's head equals that SHA;
- a worker is in flight.

It never checks whether the work has already landed. When the merge train's singleton catch-up (#2044) merges the base into the member's branch, the head moves past `ValidateCompletedSHA`. The train then lands the member's own PR. About 10 s later the scan logs `SHA changed after Validate completion — clearing stale completion labels … Validate will dispatch on next poll`, and the label is removed from a closed item in Done.

Observed on the dev daemon on 2026-10-11 (`dev(3d0a73f)`), 4 of 4 catch-up landings:

| Item | PR | Catch-up head |
|---|---|---|
| #2110 | #2117 | `650410be` |
| #2113 | #2131 | `d4d2706c` |
| #2112 | #2118 | `8dffa41f` |
| #2130 | #2145 | — |

Trial-PR landings (#2096, #2097, #2115) were not affected, because their member head does not move.

So far this is cosmetic: landing verification passed and no Validate re-ran. But the scan says it will re-dispatch Validate, and an open item that is still settling could actually re-run it.

## Requirements

- **R1 — Landed or terminal items are exempt.** Skip the item when any of these holds:
  - the issue is closed;
  - the item is in the cleanup (Done) stage;
  - its linked PR is merged;
  - it carries `fabrik:awaiting-landing-verification` or `fabrik:credited-pr:<N>`.

  Check these against the facts the scan already has, plus at most one live read when they disagree.
- **R2 — A head moved by the engine's own catch-up is not "external".** When the new head is a Fabrik catch-up merge, do not invalidate Validate:
  - its marker is the engine's own `<!-- fabrik:train-catch-up head=… base=… pure=true -->` for that head;
  - the trust rules are the same as `dropCatchUpFeedback`'s: never trust the commit trailer.

  Instead, record the new head as the completion SHA (or the equivalent) so later real force-pushes are still detected.
- **R3 — Real force-pushes are still caught.** A non-engine push to an open item after Validate completion still invalidates it, exactly as today.
- **R4 — Docs.** Update `docs/state-machine.md` where the SHA-invalidation scan is described.

## Acceptance

- Sim test: a singleton catch-up landing leaves `stage:Validate:complete` in place on the Done item. The test fails against today's scan.
- Sim test: an open item settled at Validate whose head moves only by a pure catch-up is not re-validated.
- Existing tests: an external force-push after Validate completion still clears the label and re-dispatches.
- The full test suite passes."

## Background

Fabrik records the PR head SHA at the moment a Validate stage completes. A per-poll settle scan compares the linked PR's current head to that SHA and, if they differ, treats the push as external: it removes the Validate completion labels so Validate re-runs and the CI gate verifies the new head before any merge. That protects against someone force-pushing unreviewed code onto a validated item.

The scan only skips an item when no completion SHA was recorded, the head still matches, or a worker is in flight. It never asks whether the item has already landed, and it cannot tell an engine-authored head movement from an external one.

The merge train's singleton catch-up (#2044) merges the base branch into a queued member's own branch, which legitimately moves the head past the recorded completion SHA. The train then lands the member's own PR and the item goes to Done. A few seconds later the scan sees the moved head and strips `stage:Validate:complete` from the closed, Done item, logging that Validate will re-dispatch. This happened on 4 of 4 catch-up landings on the dev daemon on 2026-10-11 (#2110, #2113, #2112, #2130); trial-PR landings were unaffected because the member head does not move.

Today the effect is cosmetic: landing verification passed and Validate did not re-run. But the same mechanism applies to an open item that is still settling after a catch-up, where the scan could cause a real, wasteful Validate re-run. The scan must therefore (a) leave landed or terminal items alone, and (b) recognise the engine's own catch-up merge as a non-external head change, without weakening detection of real external pushes.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Landed items keep their Validate completion (Priority: P1)

An operator watching the board sees a merge-train member land via singleton catch-up. After it reaches Done and its issue is closed, its `stage:Validate:complete` label is still present, and the log carries no "clearing stale completion labels" line for it.

**Why this priority**: This is the observed, reproducible defect (4 of 4 catch-up landings) and the only one that currently happens in production.

**Independent Test**: In the sim, run a singleton catch-up landing to completion and poll past the point where the scan would fire; assert the Done item still carries `stage:Validate:complete` and that no Validate dispatch occurs.

**Acceptance Scenarios**:

1. **Given** a queued member that completed Validate at SHA A and whose branch is moved to SHA B by a singleton catch-up, **When** the train lands the member's PR and the item reaches Done, **Then** the following polls leave `stage:Validate:complete` on the item and log no invalidation.
2. **Given** an item whose linked PR is merged but whose board column is not yet Done, **When** its head differs from the recorded completion SHA, **Then** the scan skips it.
3. **Given** an item carrying `fabrik:awaiting-landing-verification` or `fabrik:credited-pr:<N>`, **When** its head differs from the recorded completion SHA, **Then** the scan skips it.
4. **Given** a closed issue, **When** its head differs from the recorded completion SHA, **Then** the scan skips it.

---

### User Story 2 - An open item whose head moved only by engine catch-up is not re-validated (Priority: P1)

An item that has completed Validate is still open (for example, queued or settling) when the merge train's catch-up merges the base into its branch. Validate does not re-run because of that engine-authored head change.

**Why this priority**: It closes the path by which the cosmetic bug could become a real, wasteful Validate re-run.

**Independent Test**: In the sim, settle an item at Validate, apply a pure catch-up that moves the head (with the engine's own catch-up marker for the new head), poll, and assert Validate completion is retained and Validate is not dispatched.

**Acceptance Scenarios**:

1. **Given** an open item settled at Validate at SHA A and a self-authored catch-up marker naming the live head B with `pure=true`, **When** the scan runs, **Then** it does not invalidate Validate and records B as the new completion SHA.
2. **Given** the same item, **When** a later external push moves the head to C after B was recorded, **Then** the scan invalidates Validate as it does today.

---

### User Story 3 - Real external pushes still invalidate Validate (Priority: P1)

A human or other actor force-pushes or pushes new commits to an open, validated item. Validate completion is cleared and Validate re-runs, exactly as before.

**Why this priority**: This is the scan's purpose; the fix must not weaken it.

**Independent Test**: The existing external force-push test(s) continue to pass unmodified.

**Acceptance Scenarios**:

1. **Given** an open item that completed Validate at SHA A, **When** its head moves to SHA B with no engine catch-up marker, **Then** the completion labels are cleared and Validate dispatches on the next poll.
2. **Given** an open item whose head moves to B and a catch-up marker exists but is not the engine's own, names a different head, or has `pure` not true, **When** the scan runs, **Then** it invalidates Validate.
3. **Given** an open item whose head carries a catch-up trailer in the commit message but no valid engine marker, **When** the scan runs, **Then** it invalidates Validate (the trailer is never trusted).

---

### Edge Cases

- The facts the scan already holds disagree about whether the item has landed (for example, the cached item looks open but the linked PR is merged, or the reverse): one live read may break the tie; no more than that.
- The live read fails: the scan must not strip completion labels on the strength of an unreadable landing state. It keeps today's behaviour for an open, non-landed item only when the item is positively known not to have landed; an unreadable landing state is treated as "do not invalidate this poll" and retried next poll.
- The catch-up marker names the live head but the commit that moved the head was pushed by someone else after the marker was posted: the marker's head will not equal the live head, so the push is treated as external.
- A catch-up marker is authored by a non-engine account (anyone can post a comment): it is ignored.
- A catch-up with `pure=false` (conflicts resolved by Claude, so the head contains non-mechanical changes): it is not exempt, and Validate is invalidated as today.
- The recorded completion SHA is updated to the catch-up head, then the engine restarts: the updated record must be as durable as the original completion SHA record, so a restart does not cause the new head to be re-treated as external.
- An item that has not recorded a completion SHA at all: still skipped, as today.
- A worker is in flight on the item: still skipped, as today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The SHA-invalidation scan MUST skip an item whose issue is closed.
- **FR-002**: The scan MUST skip an item that is in the cleanup (Done) stage.
- **FR-003**: The scan MUST skip an item whose linked PR is merged.
- **FR-004**: The scan MUST skip an item carrying `fabrik:awaiting-landing-verification` or any `fabrik:credited-pr:<N>` label.
- **FR-005**: The FR-001–FR-004 checks MUST use the facts the scan already has, and MAY add at most one live read per item per scan pass, and only when those facts disagree about whether the work has landed.
- **FR-006**: If a needed live read fails, the scan MUST NOT invalidate Validate for that item in that pass.
- **FR-007**: The scan MUST NOT invalidate Validate when the linked PR's new head is a Fabrik singleton catch-up merge, defined as: a catch-up marker (`<!-- fabrik:train-catch-up head=… base=… pure=true -->`) authored by the engine's own identity, whose `head` equals the PR's live head, and whose `pure` value is `true`.
- **FR-008**: The catch-up determination in FR-007 MUST follow the same trust rules as the existing catch-up review-feedback suppression: the marker must be the engine's own and match the live head, and the commit message trailer MUST NOT be trusted.
- **FR-009**: When FR-007 applies, the scan MUST record the new head as the item's Validate completion SHA (or an equivalent record) so that a later real push, moving the head again, is still detected as external. That record MUST survive an engine restart to the same degree the original completion SHA does.
- **FR-010**: A head change that does not satisfy FR-007 on an open, non-landed item MUST invalidate Validate exactly as it does today (completion labels removed, Validate re-dispatched on the next poll).
- **FR-011**: Existing skips (no completion SHA recorded, head equals recorded SHA, worker in flight) MUST be preserved unchanged.
- **FR-012**: When the scan skips a landed or terminal item, or accepts a catch-up head, it MUST NOT log the "clearing stale completion labels … Validate will dispatch on next poll" message for that item.
- **FR-013**: `docs/state-machine.md` MUST describe the scan's new exemptions (landed/terminal items, engine catch-up heads) where the SHA-invalidation scan is described, and `docs/llms-full.txt` MUST be regenerated in the same change.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a singleton catch-up landing, the Done item retains `stage:Validate:complete` across at least the polls in which the scan previously fired, in 100% of simulated catch-up landings; the sim test fails against the pre-fix scan.
- **SC-002**: An open item settled at Validate whose head moves only by a pure engine catch-up produces zero Validate dispatches and zero completion-label removals.
- **SC-003**: Every existing external force-push scenario after Validate completion still clears the completion label and re-dispatches Validate (existing tests pass unmodified).
- **SC-004**: A catch-up marker that is not the engine's own, does not name the live head, or has `pure` other than `true` never prevents invalidation.
- **SC-005**: The full test suite (`go test -race ./...`) passes.
- **SC-006**: The scan performs at most one additional live read per item per pass, and none when its cached facts agree.

## Assumptions

- "Engine's own identity" for the marker means the same self-authorship test already used by the catch-up review-feedback suppression; no new identity concept is introduced.
- Recording the catch-up head as the completion SHA uses the same storage and durability as the existing completion SHA, so restarts need no special handling beyond that.
- A catch-up with `pure=false` is deliberately not exempt, because a conflict-resolved head contains non-mechanical content that Validate has not verified.
- A failed live read defers the decision to the next poll rather than invalidating, because wrongly stripping completion from a landed item is the bug being fixed, and the re-check is cheap.
- Items in the Done stage or with closed issues have no reason to ever re-validate on head movement, so these skips apply unconditionally.

## Out of Scope

- Changing how the merge train's catch-up works, or when it posts its marker.
- Changing the landing-verification scan or its labels.
- Re-adding `stage:Validate:complete` to items that already lost it in earlier runs (retroactive repair of past landings).
- Exempting other engine-authored head movements (for example, auto-repair or rebase reinvokes) from the scan.
- Any change to how Validate itself runs.

## Source References

- Issue #2158 (this issue), observed on the dev daemon on 2026-10-11 (`dev(3d0a73f)`).
- Merge-train singleton catch-up: #2044 / ADR-2044, `docs/state-machine.md` §6.30.
- Catch-up review-feedback suppression trust rules: `dropCatchUpFeedback` (`engine/catchup_feedback.go`).
- SHA-invalidation scan description: `docs/state-machine.md`.
