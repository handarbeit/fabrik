# Feature Specification: Clean expected-reviewer review must not eject Queued merge-train members

**Feature Branch**: `fabrik/issue-2525`
**Created**: 2026-10-30
**Status**: Specified
**Input**: User description:

> ## Problem
>
> The clean-review skip (#2489, ADR 2489) does not apply while an item is in the merge train's holding stage, so a clean Pruefer review ejects every Queued member until it pauses.
>
> `cleanReviewSkipper(status, number, labels)` (`engine/findings_marker.go`) takes `expected_reviewers` from the stage named by the item's **current** Status. In `Queued` that is the holding stage, which declares no `expected_reviewers`, so the skipper returns nil and nothing is skipped. `queuedReviewFindings` → `buildReviewBodyCommentsFromReviews` (`engine/reviews.go:1500`) then reports the clean review body as an unprocessed finding. At Validate the same review is skipped (Validate declares the reviewer), and because ADR 2489 derives "addressed" rather than writing a marker, nothing records it as processed. So:
>
> 1. Validate completes; the feedback gate sees nothing and the item advances to Queued.
> 2. `settleQueuedReviewFindings` sees "1 unprocessed review finding" and flags a pending eject.
> 3. The worker applies it and reroutes to Validate; Validate advances it straight back.
> 4. After `MaxMergeTrainEjections` (3) the item pauses with "pausing after 3 ejections".
>
> Observed on #2491 / PR #2524 on 2026-10-30 at 00:18:50–00:19:40Z. That PR's only review is a Pruefer review with a clean marker at head (`fabrik_status` shows `clean_pass: true`). The dev daemon log shows the full loop. Every yolo item on a merge-train repo whose landing review is clean will hit this, so it blocks the 0.0.91 release.
>
> ## Requirements
>
> 1. **One stage context for the skipper.** For an item in a holding stage, the skipper (and every other reader of the review-body funnel) must judge `expected_reviewers` against the stage the member was validated in: the reroute target / the stage before the holding stage (Validate here). It must never use the holding stage itself. All five funnel readers, plus `queuedReviewFindings`, must agree; extend `TestCleanReviewAgreesAcrossReaders` to cover a Queued item.
> 2. **No other change to ejection.** A Queued member with a real unprocessed finding (a non-clean body, an unresolved thread, a `CHANGES_REQUESTED`) still ejects exactly as today.
> 3. **Audit other `stageByName(item.Status)` reads on review/feedback paths** that a Queued item can reach (e.g. `effectiveExpectedReviewers`, `effectiveReviewAuthority` callers in the Queued settle scan or the landing gates), and fix any with the same holding-stage blind spot. List each one found in the PR, whether fixed or audited.
>
> ## Acceptance
>
> - A sim twin: a yolo item on a merge-train repo, whose only review is a declared reviewer's clean-marker review at head, lands with no ejection. With the fix neutralised it ejects and pauses after 3 ejections (reproduced red against `main` first).
> - A Queued member with a non-clean declared-reviewer review still ejects.
> - `docs/state-machine.md` (§6.6.6 / §6.16) updated; `docs/llms-full.txt` regenerated.
>
> ## Notes
>
> Regression from #2489 (merged 2026-10-29, unreleased). Once this lands, #2491 is resumed by removing `fabrik:paused`.

## Background

#2489 made a review from a declared (`expected_reviewers`) reviewer whose body ends in a Pruefer findings marker reading zero inline and zero body findings non-actionable: it starts no review re-invocation and holds no gate. Whether a reviewer is "declared" is decided per stage, and the check uses the stage the item is currently in.

When the merge train is on, a validated item moves to the holding stage (Queued). The holding stage declares no expected reviewers, so the clean-review rule stops applying the moment an item is Queued, even though the very same review was correctly ignored one column earlier at Validate. "Addressed" is derived, not recorded, so nothing remembers the review was already judged clean. The Queued-member review-finding scan therefore treats the clean review as a new unprocessed finding and ejects the member back to Validate. Validate advances it straight back to Queued, and the loop repeats until the ejection cap pauses the item.

Observed on #2491 / PR #2524 (2026-10-30, ~1 minute loop, ending in "pausing after 3 ejections"). Every yolo item on a merge-train repository whose landing review is clean is exposed, which blocks the 0.0.91 release. The defect is a regression from #2489 (merged 2026-10-29, unreleased).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A clean landing review lands without ejection (Priority: P1)

An operator runs a merge-train repository with a declared review bot. A yolo item completes Validate; its only review is the declared bot's clean-marker review at the current head. The item enters Queued and is landed by the train without being ejected or paused.

**Why this priority**: This is the release-blocking regression; without it every clean-reviewed yolo item on a train repo ends paused after three ejections.

**Independent Test**: In the sim bed, seed a train-enabled repo, a yolo item at Validate with a declared reviewer's clean-marker review at head, and run polls until it lands. Assert zero ejections, no pause, and a landed item. With the fix neutralised, the same scenario ejects three times and pauses.

**Acceptance Scenarios**:

1. **Given** a yolo item in Queued whose only review is a declared reviewer's clean-marker review at the live head, **When** the Queued review-finding scan runs, **Then** it flags no finding and no eject, and the item stays Queued until landed.
2. **Given** the same item before the fix, **When** polls run, **Then** it is ejected to Validate and returns, and pauses after `MaxMergeTrainEjections` ejections (the red reproduction).

---

### User Story 2 - Real findings on a Queued member still eject (Priority: P1)

A Queued member receives genuine review feedback. It is ejected exactly as it is today so the review path can address it.

**Why this priority**: The fix must not weaken the safety behaviour the scan exists for.

**Independent Test**: Seed a Queued member with each of: a declared reviewer's non-clean marker review, a review body with no marker, an unresolved review thread, and a `CHANGES_REQUESTED` review. Assert each still produces the same pending eject as before the change.

**Acceptance Scenarios**:

1. **Given** a Queued member with a declared reviewer's review whose marker reports one or more findings, **When** the scan runs, **Then** it ejects the member as it does today.
2. **Given** a Queued member with an unresolved thread or a `CHANGES_REQUESTED` review, **When** the scan runs, **Then** it ejects the member as it does today.

---

### User Story 3 - Every reader of the review-body funnel agrees for Queued items (Priority: P2)

Whichever code path inspects review feedback for a Queued item (the Queued scan, cached pending-feedback readers, the dispatcher-visible set, reviewer verdicts, the live feedback gate), they all reach the same answer on whether a given review is actionable, using the same expected-reviewers source.

**Why this priority**: The bug exists because one stage-context choice was made inconsistently relative to the item's real position; disagreement between readers is what produces loops and wedges.

**Independent Test**: Extend the existing cross-reader agreement test with a Queued item and confirm all readers drop the clean review and keep a non-clean one.

**Acceptance Scenarios**:

1. **Given** a Queued item and a clean expected-reviewer review, **When** each reader of the funnel is evaluated, **Then** all treat the review as non-actionable.
2. **Given** a Queued item and a non-clean review, **When** each reader is evaluated, **Then** all treat it as actionable.

---

### User Story 4 - Other holding-stage blind spots are found and recorded (Priority: P2)

Other review/feedback code paths a Queued item can reach that look up stage configuration from the item's current Status are audited. Any with the same holding-stage blind spot is fixed, and every one examined is listed in the PR.

**Why this priority**: The same class of defect may exist in sibling paths (expected-reviewer resolution, review-authority resolution in the Queued scan or landing gates); fixing only one site risks a repeat incident.

**Independent Test**: The PR description lists each audited site with a verdict (fixed, or audited-and-fine with the reason); each fixed site has a test showing a Queued item resolves the validating stage's configuration.

**Acceptance Scenarios**:

1. **Given** the audit list in the PR, **When** reviewed, **Then** every current-Status stage lookup reachable by a Queued item on a review/feedback path appears with a disposition.

---

### Edge Cases

- A holding stage that is not the first stage after Validate, or a train configured with a different stage immediately before the holding stage: the validating stage is the stage before the holding stage, not a hard-coded name.
- A Queued item whose validating stage cannot be determined: behaviour must be conservative (treat the review as actionable, i.e. today's behaviour) rather than skipping findings.
- A member rerouted off Queued to the reroute target: the context must agree with the stage it is then actually in.
- An item with an `expected-reviewers:<mode>` label override: the override still takes precedence exactly as in the other gates.
- A clean-marker review that is not at the live head, from an undeclared reviewer, or with a malformed marker: not skipped, as defined by #2489.
- A new push after the clean review: findings from the new head are judged normally.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: For an item whose current stage is a holding stage, `expected_reviewers` used by the clean-review skip MUST be taken from the stage the member was validated in (the stage before the holding stage, which is the reroute target), never from the holding stage itself.
- **FR-002**: Every reader of the review-body funnel (the live builder, the cached pending-feedback readers, the dispatcher-visible set, reviewer verdicts) and the Queued review-finding scan MUST use that same stage context and therefore reach the same actionable/non-actionable decision for the same review.
- **FR-003**: A Queued member with a real unprocessed finding (a non-clean or marker-less review body, an unresolved review thread, a `CHANGES_REQUESTED` review) MUST still be ejected exactly as today; ejection counting, pausing and rerouting behaviour are otherwise unchanged.
- **FR-004**: When the validating stage cannot be determined for a holding-stage item, the system MUST fall back to treating reviews as actionable (no skip).
- **FR-005**: The cross-reader agreement test MUST be extended to cover a Queued item, for both a clean and a non-clean review.
- **FR-006**: Every other lookup of stage configuration by the item's current Status on a review or feedback path reachable by a Queued item (including expected-reviewer resolution and review-authority resolution in the Queued scan and the landing gates) MUST be audited; any with the same holding-stage blind spot MUST be fixed, and each one examined MUST be listed in the PR with its disposition.
- **FR-007**: A sim twin MUST cover a yolo item on a merge-train repo whose only review is a declared reviewer's clean-marker review at head: it lands with no ejection, and with the fix neutralised (a per-Engine test seam, following the repository convention) it ejects and pauses after `MaxMergeTrainEjections` ejections. The red reproduction MUST be demonstrated against `main` before the fix.
- **FR-008**: A sim or unit test MUST show that a Queued member with a non-clean declared-reviewer review still ejects.
- **FR-009**: `docs/state-machine.md` (§6.6.6 and §6.16) MUST be updated to describe the holding-stage context rule, and `docs/llms-full.txt` MUST be regenerated in the same change.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A yolo item on a merge-train repo with a clean declared-reviewer review at head reaches landing with 0 ejections and no pause.
- **SC-002**: With the fix neutralised, the same scenario reproduces the observed loop: 3 ejections followed by a pause.
- **SC-003**: 100% of the previously ejecting finding types (non-clean marker, marker-less body, unresolved thread, `CHANGES_REQUESTED`) still eject a Queued member.
- **SC-004**: All readers of the review-body funnel return the same decision for a Queued item in the agreement test, for both clean and non-clean reviews.
- **SC-005**: The PR lists 100% of the audited current-Status stage lookups on review/feedback paths reachable by Queued items, each with a disposition.
- **SC-006**: The docs-drift check passes (bundle regenerated).

## Assumptions

- The merge train's holding stage is always immediately preceded by the stage that provides the reroute target (Validate in the default configuration); that stage is the authoritative source of `expected_reviewers` for a Queued member.
- The cleanness of a review continues to be determined solely by the #2489 marker rules; this issue changes only which stage's declared reviewers apply.
- Review authority and `expected-reviewers:<mode>` label overrides keep their existing precedence rules.
- Fixes follow the repository's per-Engine test-seam convention for neutralisation, so the sim twin can be run with and without the fix.

## Out of Scope *(optional)*

- Changes to the ejection cap, ejection counting, or the pause behaviour.
- Writing a durable "addressed" marker for clean reviews (ADR 2489 deliberately derives it).
- Changes to Pruefer's marker format or to the clean-review predicate itself.
- Re-requesting reviews from bots that only review on push.
- Resuming #2491 (done by an operator removing `fabrik:paused` once this lands).

## Source References *(optional)*

- Regression from #2489 / ADR 2489 (`adrs/2489-*`); observed on #2491 / PR #2524.
- `docs/state-machine.md` §6.6.6 (review gate / clean review) and §6.16 (Queued review-finding settle scan).
