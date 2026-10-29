# Feature Specification: Review gate: do not dispatch a worker for an expected reviewer's review with no findings

**Feature Branch**: `fabrik/issue-2489`
**Created**: 2026-10-29
**Status**: Specified
**Input**: User description: "## Problem

Since #1045 every non-dismissed review body is actionable, so a clean `COMMENTED` review from an expected reviewer still dispatches a `review-reinvoke`. The worker reads it, finds nothing to do, pushes nothing and the cycle is refunded (`ReviewCycleDecremented`) — but it held a worker slot and cost a session. The report counts **41 such rounds** that pushed nothing.

## Requirements

1. A review from an author in the stage's effective `expected_reviewers` whose body carries the Pruefer findings marker (#2488) with zero inline and zero body findings, and which has no unresolved review threads, is not actionable: no reinvoke, and it does not hold the feedback gate.
2. The marker is trusted only on a review **authored by** an expected reviewer (a marker typed by anyone else is ignored), and only on the review it is in.
3. Anything else is unchanged: no marker, a non-zero count, an unparseable marker, or an undeclared author → today's behaviour (actionable).
4. Addressed state for the review is recorded like any other addressed body (the #1555 `review-ids-addressed` marker), so it is never reconsidered.
5. The review-gate verdict is not changed here (#2465 owns it); this is only about dispatch and the feedback gate.

## Acceptance

- Sim twin: clean Pruefer review → no reinvoke, item advances; same body from a non-expected author → reinvoke; non-zero count → reinvoke. Neutralise the check → first test fails.
- `docs/state-machine.md` feedback-gate section updated; `docs/llms-full.txt` regenerated."

## Background

Since #1045, the body of every review that is not `DISMISSED` or `PENDING` is treated as potentially actionable. This was deliberate: a bot reviewer such as Pruefer submits its whole finding set as a `COMMENTED` review, and treating that as non-actionable silently dropped real findings.

The cost is that a review which found nothing is also actionable. When an expected reviewer posts a clean `COMMENTED` review, the engine dispatches a `review-reinvoke`. The worker reads the review, finds nothing to change, pushes nothing, and the review cycle is refunded. The round still occupied a worker slot and consumed a Claude session. The community flow analysis (#2453, covering `totalslacker/chronicle`, Oct 1–26) counts 41 such rounds that pushed nothing.

Pruefer now ends every review body with a machine-readable findings-count marker (#2488), giving the engine a structural way to tell a clean review from one with findings without a worker reading prose. This issue makes the engine's dispatch and feedback-gate decisions use that marker, with strict trust rules so a marker can never suppress real feedback.

Scope is limited to dispatch and the feedback gate. The review-gate verdict (whether the gate clears or blocks merging) belongs to #2465 and is not changed here.

Mentioned for context: #2453 (source report; closed by hand when a release ships).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A clean review from an expected reviewer starts no worker (Priority: P1)

An operator runs a stage with `wait_for_reviews: true` and Pruefer declared in `expected_reviewers`. Pruefer posts a `COMMENTED` review whose findings marker reports zero inline and zero body findings, and the review has no unresolved threads. Fabrik does not start a review-reinvoke worker and the review does not hold the feedback gate, so the item advances without spending a slot or a session.

**Why this priority**: This is the whole purpose of the issue. It removes the wasted rounds, and the slot-starvation that comes with them, from the common clean-review case.

**Independent Test**: In the sim, seed a PR whose only review is from a declared expected reviewer with a clean marker and no unresolved threads. Run polls and assert that no review-reinvoke is dispatched and that the item advances.

**Acceptance Scenarios**:

1. **Given** a declared expected reviewer's review with a well-formed marker reading zero inline and zero body findings and no unresolved threads, **When** the engine evaluates feedback for the item, **Then** the review is not actionable, no review-reinvoke is dispatched, and the feedback gate does not hold on it.
2. **Given** the same review, **When** the item reaches an advance or landing decision, **Then** the review does not delay the decision as unaddressed feedback.

---

### User Story 2 - Anything that is not provably clean still gets worked (Priority: P1)

A review that cannot be proven clean must behave exactly as it does today: the marker is absent, reports any non-zero count, is malformed, or the author is not an expected reviewer. A marker can never be used to suppress feedback from anyone who is not a declared expected reviewer.

**Why this priority**: The change must be safe. The failure mode to avoid is silently dropping real review feedback, so the untrusted paths are as important as the optimisation.

**Independent Test**: In the sim, vary one input at a time against the Story 1 fixture: a non-expected author, a non-zero inline count, a non-zero body count, no marker, an unparseable marker, an unresolved thread. Assert a review-reinvoke is dispatched in every case.

**Acceptance Scenarios**:

1. **Given** the same clean-marker body authored by someone not in the stage's effective `expected_reviewers`, **When** the engine evaluates feedback, **Then** the review is actionable and a review-reinvoke is dispatched.
2. **Given** an expected reviewer's review whose marker has a non-zero inline or body count, **When** the engine evaluates feedback, **Then** the review is actionable.
3. **Given** a review with no marker, or a marker that does not parse, **When** the engine evaluates feedback, **Then** the review is actionable.
4. **Given** a clean marker on an expected reviewer's review that has an unresolved review thread, **When** the engine evaluates feedback, **Then** the work is not suppressed and the thread is handled as actionable feedback.
5. **Given** a human or other non-reviewer comment that quotes a clean marker, **When** the engine evaluates feedback, **Then** the quoted marker has no effect.

---

### User Story 3 - A skipped clean review is never reconsidered (Priority: P2)

Once Fabrik has decided a clean review is not actionable, it is recorded as addressed in the same durable way as any other addressed review body, so it is not re-evaluated on later polls or after a restart.

**Why this priority**: Without durable addressed state the skip would be recomputed every poll and could flap if the declared reviewer list changed, but it is a hygiene property of Story 1.

**Independent Test**: In the sim, run the Story 1 scenario, restart the engine and poll again. Assert that no worker is dispatched and the review remains addressed.

**Acceptance Scenarios**:

1. **Given** a clean review that was skipped, **When** later polls or a restart occur, **Then** the review is already recorded as addressed and does not reappear as unprocessed feedback.

---

### Edge Cases

- A review whose marker reads `coverage=partial` but zero findings: the counts alone govern (see Assumptions).
- A marker present but not in the position Pruefer emits it, or appearing more than once, is treated as unparseable.
- A stage with no `expected_reviewers` declared, or the `expected-reviewers:none` label: no author is an expected reviewer, so no marker is trusted.
- The `expected-reviewers:declared` label resolves to a synthetic identity that never posts a real review, so no real review is trusted.
- An expected reviewer posts a clean review and later a second review with findings: only the second is actionable.
- A clean review is later edited to add text: the review is judged by its current body.
- A `CHANGES_REQUESTED` review that carries a clean marker is still judged by the same rules; the review-gate verdict itself is unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A review is non-actionable for dispatch and for the feedback gate when all of the following hold: its author is in the stage's effective `expected_reviewers`; its body carries a well-formed Pruefer findings marker (#2488) reporting zero inline and zero body findings; and it has no unresolved review threads.
- **FR-002**: A non-actionable review under FR-001 starts no review-reinvoke worker and does not cause the feedback gate to hold an advance or landing.
- **FR-003**: The marker is honoured only on a review whose author is in the effective `expected_reviewers`; a marker in a review by any other author, or in an issue comment, PR comment or review-thread comment by anyone, is ignored.
- **FR-004**: The marker applies only to the review whose body contains it; it does not mark other reviews, earlier reviews by the same author, or the PR as a whole.
- **FR-005**: Behaviour is unchanged (the review is actionable) when the marker is absent, any count is non-zero, either count is `unknown`, the marker is unparseable or duplicated, or the author is not an expected reviewer.
- **FR-006**: The set of expected reviewers is the same effective set used elsewhere for the stage, including per-issue `expected-reviewers:<mode>` label overrides, so dispatch and the review gate cannot disagree on who is expected.
- **FR-007**: When a review is judged non-actionable under FR-001, its addressed state is recorded through the existing durable review-body addressed marker (`review-ids-addressed`, #1555), so it is not reconsidered on later polls or after a restart.
- **FR-008**: The review-gate verdict (#2465) and the review-gate's clearing and timeout behaviour are unchanged; this change affects only whether a review dispatches a worker and whether it holds the feedback gate.
- **FR-009**: Unresolved review threads keep their existing handling and are never suppressed by a clean marker on a review body.
- **FR-010**: The behaviour is covered by a sim scenario set as described in Success Criteria, and the feedback-gate section of `docs/state-machine.md` is updated and `docs/llms-full.txt` regenerated in the same change.

### Key Entities

- **Review**: a pull-request review with an author, state, body and the threads it opened. Actionability is decided per review.
- **Findings marker**: the hidden final line of a Pruefer review body (#2488) carrying `inline`, `body` and `coverage` values, each possibly `unknown`.
- **Expected reviewers**: the stage's effective declared list of self-submitting reviewers, after label overrides.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim, a clean marker review from an expected reviewer with no unresolved threads causes zero review-reinvoke dispatches and the item advances.
- **SC-002**: In the sim, each of these dispatches exactly one review-reinvoke: the same body from a non-expected author, a non-zero inline count, a non-zero body count, a missing marker, and an unparseable marker.
- **SC-003**: With the new check neutralised through a test seam, the SC-001 scenario fails (a worker is dispatched), proving the scenario is not vacuous.
- **SC-004**: After a skipped clean review and an engine restart, the review does not reappear as unprocessed feedback and no worker is dispatched.
- **SC-005**: The review-gate verdict for the same fixtures is identical with and without this change.
- **SC-006**: `docs/state-machine.md` documents the rule and `docs/llms-full.txt` matches a fresh regeneration.

## Assumptions

- "No unresolved review threads" means no unresolved threads opened by that review. Unresolved threads from other reviews already have their own feedback path and are not suppressed or newly attributed by this change.
- The `coverage` field is not consulted. A zero-findings review with partial coverage is treated as clean, since a worker can act only on findings, not on entries the reviewer did not read.
- The marker is read only in the position and grammar #2488 defines (final line of the review body); anything else counts as unparseable.
- A review judged non-actionable only because it is clean is recorded as addressed in the same way as any other addressed body, with no new comment, label or reaction beyond what that path already does.
- Identifying "authored by an expected reviewer" uses the same author-identity matching the review gate already uses for declared reviewers.
- #2498 is listed as a dependency of this issue on the board. This spec assumes the marker work (#2488) is merged and does not depend on anything else from it.

## Out of Scope

- Changing the review-gate verdict, clearing, bot re-prompt ladder or timeouts (#2465 and ADR-1283/1250 own these).
- Changing Pruefer or the marker grammar (#2488).
- Suppressing non-clean feedback of any kind, or trusting a marker from reviewers other than declared expected reviewers.
- Changing how unresolved review threads or human comments are handled.
- Reducing worker cost for reviews that do have findings.

## Source References

- Community report #2453 (flow analysis, mentioned by bare number only): https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874
- Data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec
- #1045 (every non-dismissed review body is actionable), #1555 (`review-ids-addressed` marker), #2488 (findings-count marker), #2465 (review-gate verdict)
