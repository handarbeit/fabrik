# Feature Specification: Feedback gate and review dispatcher agree on which review bodies are outstanding

**Feature Branch**: `fabrik/issue-2160`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

The feedback gate and the review-feedback dispatcher can disagree about whether a review body has been addressed. The item then deadlocks: the gate holds it forever and nothing is dispatched.

Observed on #2125 (PR #2148) on the dev daemon on 2026-10-11 (`dev(3d0a73f)`):
- Pruefer posted two COMMENTED review bodies: 5483754852 (14:57Z) and 5483772616 (15:03Z, at head `ddeecb70`).
- The 15:11Z review-reinvoke was given only 5483754852. Its PR comment carries `<!-- fabrik:review-ids-addressed: 5483754852 -->`.
- From 15:17Z the gate logged `[#2125 feedback-gate] holding advance — 1 unprocessed review body (5483772616)` on every poll. No reinvoke was dispatched for over an hour, although other items were dispatched in that time.
- #2123, which is blocked by #2125, was stuck with it.
- A SIGHUP restart at 16:22:54Z (which clears all in-memory state) dispatched `review-reinvoke … (comments: review-body:5483772616)` at 16:23:26Z. So the cause is in-memory state that survives polls but not a restart.

The gate reads reviews live. The dispatcher works from the store. Two candidate mechanisms fit the evidence, and Research must determine which (or both):

1. **Stale in-memory "processed" record.** `buildReviewBodyCommentsFromReviews` (`engine/reviews.go`) filters candidates through `snap.CommentProcessed`. It consults the durable `review-ids-addressed` marker only for candidates not already marked in memory. If something marked `review-body:5483772616` processed without addressing it, the dispatcher skips it while `feedbackGateBlocks` (`engine/feedback_gate.go`) finds no durable marker and holds. A suspect is a stage-completion / `markCommentsSeenByStage` pass that ran while the review arrived mid-run.
2. **Stale cached reviews.** #2125's last deep fetch was at 15:21:37Z. If the dispatcher's view of the PR's reviews never refreshed to include 5483772616, it has nothing to dispatch, while the gate's live read sees it.

## Requirements

- **R1 — One answer.** The gate and the dispatcher decide \"is this review body addressed?\" with the same predicate over the same facts, so an item can never be held by the gate while the dispatcher considers nothing outstanding.
- **R2 — Fix the cause Research identifies.** A review body is recorded as processed only by a pass that was actually given it. A review the gate can see live is always visible to the dispatcher within one poll.
- **R3 — Deadlock backstop.** If the gate holds an item on feedback that the dispatcher does not consider outstanding, the engine detects the disagreement within one poll, logs it once, and dispatches the feedback rather than holding indefinitely.
- **R4 — Docs.** Update `docs/state-machine.md` §6.6.6 / the feedback-gate description and ADR-1953-a's as-built notes if they change.

## Scope

- Out of scope: the unpushed engine rebase on #2125's branch (ahead 17 / behind 8); that is #2150.

## Acceptance

- Sim test reproducing the #2125 shape: a second bot review body arrives while a Review pass is running, and the reinvoke addresses only the first. The second is then dispatched, without a restart. The test fails today.
- Test for R3: an injected disagreement (in-memory processed, no durable marker) results in a dispatch, not a permanent hold.
- The full test suite passes."

## Background

Fabrik holds an item's advance (and its landing) while any review feedback on its PR is still unaddressed, and separately dispatches a review-reinvoke to address that feedback. These two halves answer "is this review body addressed?" from different sources: the hold reads the PR's reviews live and checks the durable "addressed" marker on the PR; the dispatch works from the engine's in-memory item state. When the two disagree, the hold stays on and nothing is dispatched, so the item is stuck until something clears the in-memory state.

This happened on #2125 (PR #2148) on the dev daemon on 2026-10-11. Two bot reviews (ids 5483754852 and 5483772616) were posted on the PR. The one review-reinvoke that ran was given only the first, and the PR carries the durable marker for the first only. From then on the hold logged "1 unprocessed review body (5483772616)" every poll for over an hour while no reinvoke was dispatched, even though other items were. #2123, which is blocked by #2125, was stuck with it. A restart of the daemon, which clears all in-memory state, immediately dispatched a reinvoke for the second review. The cause is therefore in-memory state that survives polls but not a restart.

Two mechanisms fit the evidence and the Research stage must determine which one (or both) is responsible:

1. A stale in-memory "processed" record: the review body was marked processed in memory by a pass that was never given it (for example a stage-completion pass that ran while the review arrived mid-run), so the dispatcher skips it while the hold finds no durable marker.
2. Stale cached reviews: the dispatcher's view of the PR's reviews was never refreshed after the second review arrived (the item's last deep fetch predates it), so it has nothing to dispatch while the hold's live read sees the review.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A review body that arrives mid-run is still dispatched (Priority: P1)

An operator has an item in Review with a PR. A bot reviewer posts a second review body while a Review pass is already running, and that pass addresses only the first. The second review body must be dispatched for addressing on a later poll, without any restart.

**Why this priority**: This is the observed production failure. It stalls the item and everything blocked by it until an operator restarts the daemon.

**Independent Test**: In the sim bed, seed a PR with one bot review body, start a Review pass, add a second bot review body while it runs, let the pass finish having addressed only the first, then advance polls. A reinvoke carrying the second review body is dispatched and the item is not held indefinitely. This test fails on the current engine.

**Acceptance Scenarios**:

1. **Given** a PR with two bot review bodies of which a Review pass was given and addressed only the first, **When** subsequent polls run, **Then** a review-reinvoke is dispatched for the second review body without a restart.
2. **Given** the second review body has been addressed by that reinvoke, **When** the next poll runs, **Then** the hold clears and the item advances normally.

---

### User Story 2 - The gate and the dispatcher cannot disagree (Priority: P1)

The hold and the dispatch decide whether a review body is addressed with the same rule over the same facts, so an item is never held on feedback the dispatcher considers nothing to do about.

**Why this priority**: Fixing only the one observed trigger leaves the class of deadlock open; the requirement is that the disagreement itself cannot exist.

**Independent Test**: Unit-level tests of the shared decision: for any combination of in-memory processed record, durable marker and live review set, the hold and the dispatcher return the same set of outstanding review bodies.

**Acceptance Scenarios**:

1. **Given** a review body that is recorded as processed in memory but has no durable "addressed" marker, **When** the hold and the dispatcher each evaluate the item, **Then** both treat it identically (both outstanding, or both addressed).
2. **Given** a review that exists on the PR live but is absent from the engine's cached view, **When** the next poll runs, **Then** the dispatcher sees it.

---

### User Story 3 - A residual disagreement is detected and resolved, not held forever (Priority: P2)

If, despite the above, the hold is on feedback the dispatcher does not consider outstanding, the engine notices within one poll, records it once in the log, and dispatches the feedback rather than holding indefinitely.

**Why this priority**: A safety net against any future cause of the same deadlock; lower priority than removing the known causes.

**Independent Test**: Inject a disagreement (review body marked processed in memory, no durable marker, live review present) and advance one poll; a dispatch occurs and one log line records the disagreement; further polls do not repeat the log line while the situation is unchanged.

**Acceptance Scenarios**:

1. **Given** the injected disagreement, **When** one poll runs, **Then** a review-reinvoke is dispatched for the affected review body and the disagreement is logged once.
2. **Given** the disagreement was detected and dispatched, **When** later polls run before the reinvoke finishes, **Then** no duplicate dispatch or repeated log line is produced.

---

### Edge Cases

- A review arrives between the gate's live read and the dispatch in the same poll: it must be picked up by the next poll at the latest, never lost.
- A review body that was genuinely addressed (durable marker present) must stay addressed: the fix must not cause re-dispatch of already-addressed reviews.
- A review body marked as seen by a pass that was given it and addressed it must not be re-dispatched.
- Several review bodies arriving during one run, with the pass given a subset: every one not given to the pass remains outstanding.
- A dismissed or pending review is not feedback for either the hold or the dispatcher, as today.
- The backstop must not loop: if a dispatch for the disagreeing feedback fails or addresses nothing, existing review-cycle limits still bound the retries.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The feedback hold and the review-feedback dispatcher MUST decide whether a given review body is addressed using the same rule over the same facts, so that an item is never held for a review body that the dispatcher does not consider outstanding.
- **FR-002**: A review body MUST be recorded as processed only by a pass that was actually given that review body to address.
- **FR-003**: A review that the hold can see on the PR MUST be visible to the dispatcher no later than the next poll, regardless of when the item's cached data was last refreshed.
- **FR-004**: When the hold is active on review feedback that the dispatcher does not consider outstanding, the engine MUST detect that disagreement within one poll, log it once per occurrence, and dispatch the feedback instead of continuing to hold.
- **FR-005**: The backstop in FR-004 MUST NOT dispatch twice for the same unchanged disagreement, MUST NOT emit repeated log lines on successive polls while the dispatch is in progress, and MUST remain bounded by the existing review-cycle limits.
- **FR-006**: Review bodies that are durably recorded as addressed MUST NOT be re-dispatched as a result of this change.
- **FR-007**: The as-built documentation of the feedback hold (`docs/state-machine.md` §6.6.6 and the feedback-gate description) and the as-built notes of ADR-1953-a MUST be updated where this change alters them, and the documentation bundle regenerated.
- **FR-008**: The cause (or causes) that the Research stage identifies from the two candidate mechanisms MUST be fixed at its source, not only masked by the backstop.

### Key Entities

- **Review body**: The text submitted with a pull-request review that may require action, identified by the review's id.
- **Durable "addressed" marker**: The record on the PR that a given set of review ids was handed to a pass and addressed; survives restarts.
- **In-memory processed record**: The engine's per-item record of which comments/review bodies it considers handled; lost on restart.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim reproduction of the #2125 shape (second bot review body arriving during a Review pass that addresses only the first), the second review body is dispatched within one poll of the pass finishing and the item is never held for longer than that, with no restart. The test fails before the change and passes after.
- **SC-002**: In the injected-disagreement test (processed in memory, no durable marker), a dispatch occurs within one poll and exactly one disagreement log line is produced.
- **SC-003**: For every combination of in-memory record, durable marker and live review set covered by the shared-decision tests, the hold and the dispatcher report an identical outstanding set.
- **SC-004**: The full test suite (`go test -race ./...`) passes.

## Assumptions

- The observed failure is reproducible in the sim bed with scripted bot reviews arriving mid-run; if the sim cannot express it, the Research stage will say what is missing and the sim is fixed as part of this work (per the fidelity-drift policy).
- The backstop dispatches through the existing review-reinvoke path and its existing cycle limits; it introduces no new pause or label state.
- The disagreement log line is operator-facing and need not be persisted or surfaced elsewhere.
- Behaviour for comments and unresolved review threads (as opposed to review bodies) is unchanged unless Research shows they share the same defect.

## Out of Scope *(optional)*

- The unpushed engine rebase on #2125's branch (ahead 17 / behind 8); that is #2150.
- Changing the review-cycle limits or the review-reinvoke prompt contents.
- Any change to how bots post reviews.

## Source References *(optional)*

- Observed incident: #2125 / PR #2148, dev daemon `dev(3d0a73f)`, 2026-10-11; blocked dependent #2123.
- Feedback-gate behaviour: `docs/state-machine.md` §6.6.6 and ADR-1953-a.
