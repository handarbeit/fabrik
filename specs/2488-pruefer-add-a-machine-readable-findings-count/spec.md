# Feature Specification: Machine-readable findings-count marker in every Pruefer review body

**Feature Branch**: `fabrik/issue-2488`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

Pruefer's review body (`buildReviewBody` + `appendCoverage`, `pruefer/findings.go`, `pruefer/review.go`) carries no machine-readable statement of how many findings the review had. A clean review is a `COMMENTED` review with a prose summary, and to the engine that is indistinguishable from a `COMMENTED` review whose findings were all demoted into the body. The engine therefore has to treat every Pruefer body as potentially actionable (#1045), which costs a worker round per clean review (#2489).

## Requirements

1. Every Pruefer review body ends with one hidden marker giving the counts it posted: inline findings, findings demoted into the body, and the coverage verdict — for example `<!-- pruefer:findings inline=0 body=0 coverage=complete -->`. Computed from the parsed findings, never from the model's prose.
2. The format is documented as a stable contract (ADR) and pinned by a test; it is additive and changes nothing visible.
3. A fallback review (ADR 2307) and a partial-coverage review carry it too; a review that posts nothing posts no marker.

## Acceptance

- Unit tests for zero, inline-only, demoted-only and partial-coverage cases.
- A wire fixture of a real body with the marker, for the engine-side consumer.

## Notes

- Source: community report #2453 (totalslacker's flow analysis of `totalslacker/chronicle`, Oct 1–26: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874; data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec).
- Mention #2453 in the PR body by bare number only. Do **not** use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it — the report is closed by hand when a release ships. The PR closes this work issue only."

## Background

Pruefer's review body has no machine-readable statement of how many findings the review had. A clean review is a `COMMENTED` review with a prose summary. To the engine, that looks the same as a `COMMENTED` review whose findings were all demoted into the body because they could not be anchored to the diff. Since #1045 the engine has to treat every Pruefer review body as potentially actionable, which costs a worker round on every clean review (the engine-side consumer is tracked in #2489).

A review today is assembled from three parts: the prose summary, any demoted findings under an "Additional findings" heading, and a fixed coverage line. Optional notices (review-skill fallback, read-only review) are prepended. The inline findings go out as separate review comments, so the body itself says nothing about them.

This work issue only produces the marker, the producer side. Consuming it is out of scope here. It comes from community report #2453 (a flow analysis of `totalslacker/chronicle`, Oct 1–26). That report is mentioned for context only, and this work does not close it.

Finding the counts is not enough. When the model's findings block is missing or cannot be parsed, the review path today treats that as "no findings" without saying so. A marker computed naively from that would assert zero findings for a review whose findings were lost, which would make the marker unsafe for exactly the consumer it is built for. The spec therefore requires such a review to say "unknown", never zero.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Engine can tell a clean review from one with findings (Priority: P1)

A consumer of a Pruefer review body (initially the engine's feedback logic) reads one hidden line at the end of the body and learns how many findings were posted inline, how many were demoted into the body, and whether coverage was complete. It never has to interpret the prose.

**Why this priority**: This is the whole purpose of the issue. Without it, the engine cannot skip a worker round for a clean review.

**Independent Test**: Produce review bodies for a zero-finding, an inline-only, a demoted-only and a mixed review. Parse the last line of each and check the counts equal the findings actually submitted.

**Acceptance Scenarios**:

1. **Given** a review with no findings and complete coverage, **When** the body is built, **Then** it ends with a single marker reporting `inline=0`, `body=0` and complete coverage.
2. **Given** a review with three findings that all anchor to the diff, **When** the body is built, **Then** the marker reports `inline=3`, `body=0`.
3. **Given** a review with two findings that could not be anchored and so were demoted into the body, **When** the body is built, **Then** the marker reports `inline=0`, `body=2`.
4. **Given** a review whose coverage was partial, **When** the body is built, **Then** the marker still ends the body, after the partial-coverage line and its unread-path list, and reports the partial verdict.

---

### User Story 2 - The marker is a stable, documented contract (Priority: P1)

The marker's format is written down as a stable contract in an ADR and pinned by a test, so the engine-side consumer can depend on it and any later change is deliberate.

**Why this priority**: The engine will parse this format in a separate change. An unpinned format would break that silently.

**Independent Test**: A contract test fails if the marker's literal shape, field names, field order or value vocabulary change. A recorded wire fixture of a full real-shaped body with the marker parses with the documented grammar.

**Acceptance Scenarios**:

1. **Given** the ADR's documented grammar, **When** the pinned test renders a marker, **Then** the output matches the grammar exactly.
2. **Given** a recorded body fixture, **When** the engine-side consumer's parser reads its last line, **Then** it extracts the three counts and the coverage verdict.

---

### User Story 3 - Special review shapes carry the marker too (Priority: P2)

A review produced by the fallback path (ADR 2307) or carrying a read-only or fallback notice carries the same marker as any other. A review that posts nothing posts no marker.

**Why this priority**: A consumer that only trusts the marker on some reviews would still need the prose heuristic for the rest.

**Independent Test**: Build a fallback review and a notice-prefixed review and check the marker is present at the end. Run a path where nothing is posted and check no body, and so no marker, is produced.

**Acceptance Scenarios**:

1. **Given** a review that ran the default skill after a fallback, **When** the body is built, **Then** the fallback notice is at the top and the marker is still the last line.
2. **Given** a review that failed before submission, **When** the run ends, **Then** no review is posted and no marker exists anywhere.

---

### User Story 4 - A review with unknown findings never reads as clean (Priority: P1)

When the model's findings block is missing or unparseable, the marker states that the counts are unknown rather than reporting zero.

**Why this priority**: A false "zero findings" would make the engine skip a review that has findings — the opposite of the intended saving.

**Independent Test**: Feed the build path model output with no findings block, and with a malformed one, and check the marker does not report zero counts.

**Acceptance Scenarios**:

1. **Given** model output whose findings block is absent, **When** the body is built, **Then** the marker reports the findings counts as unknown, not `0`.
2. **Given** model output whose findings block is not valid JSON, **When** the body is built, **Then** the marker reports the counts as unknown, not `0`.

---

### Edge Cases

- The model's prose or a demoted finding contains text that looks like the marker. It must not be mistaken for the real one: the real marker is the last line, and any look-alike text earlier in the body is neutralised the way a look-alike coverage line already is.
- A body is empty apart from the marker and the coverage line (no summary, no findings): the marker still goes last.
- Findings that are duplicates are collapsed before counting; the counts describe what was actually posted, not what the model emitted.
- Both notices (fallback and read-only) are present: the marker is still last, with the notices at the top.
- A partial-coverage review with a long unread-path list: the marker is last, after the list and its "and N more" line.
- Zero inline findings but a request-changes or comment event decision: the marker reports counts only; it carries no event decision.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every Pruefer review body that is posted ends with exactly one hidden (HTML-comment) marker as its final line, reporting the number of findings posted inline, the number of findings demoted into the body, and the coverage verdict (complete or partial).
- **FR-002**: The counts are computed from the parsed findings the review actually submits — inline comments and demoted findings, after de-duplication — never from the model's prose or its own claimed counts.
- **FR-003**: The coverage verdict in the marker is taken from the computed coverage result that also produces the visible coverage line, so the two cannot disagree.
- **FR-004**: When the model's findings block is missing or cannot be parsed, the marker reports the finding counts as unknown instead of zero.
- **FR-005**: A review produced after a skill fallback (ADR 2307), a review with a read-only or fallback notice, and a partial-coverage review each carry the marker as their final line.
- **FR-006**: A review that posts nothing (invocation failure, submission failure, skipped PR) produces no marker and no body.
- **FR-007**: Any text earlier in the body that would parse as the marker (from model prose, a demoted finding, or a PR-derived string) is neutralised so the consumer cannot mistake it for the real one, including when it appears on the final line of model-derived text.
- **FR-008**: The marker adds nothing visible: it renders as nothing on GitHub, and no existing visible text, review event decision or inline comment changes.
- **FR-009**: The marker's exact format (literal prefix, field names, field order, value vocabulary, how "unknown" is expressed, and what a parser must tolerate) is documented as a stable contract in an ADR numbered after this issue, and pinned by a test that fails on any change to it.
- **FR-010**: Unit tests cover the zero-findings, inline-only, demoted-only, mixed and partial-coverage cases, plus the unknown-findings case (FR-004) and the position and uniqueness of the marker (last line, appearing once).
- **FR-011**: A wire fixture of a full body with the marker is added under the Pruefer test data so the engine-side consumer can reuse it unchanged.

### Key Entities

- **Findings marker**: The single hidden line ending a review body, carrying inline count, demoted count and coverage verdict (or an explicit unknown for the counts).
- **Demoted finding**: A finding that could not be anchored to the diff and so appears in the review body under the "Additional findings" heading.
- **Coverage verdict**: The existing complete/partial result of the coverage computation.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of posted Pruefer review bodies (including fallback, notice-prefixed and partial-coverage reviews) end with exactly one marker line.
- **SC-002**: For every test case, the counts in the marker equal the number of inline comments and demoted findings actually submitted with the review.
- **SC-003**: No review whose findings could not be parsed reports zero counts.
- **SC-004**: A change to the marker's format fails at least one test.
- **SC-005**: The rendered review on GitHub is visually unchanged: the marker is not visible in the rendered body.
- **SC-006**: An engine-side consumer can read the recorded wire fixture's counts using only the documented grammar, with no reference to the review prose.

## Assumptions

- The `pruefer:findings` prefix and the key set suggested in the issue (`inline`, `body`, `coverage`) are the intended contract, and the final grammar is fixed in the ADR. Coverage values are the existing `complete` and `partial`.
- "Unknown" for an unparseable findings block is expressed inside the same marker, as an explicit non-numeric value for the counts. It is not expressed by omitting the marker, because a missing marker already means "a review from before this contract" to a consumer. Reviewers may challenge this choice in the ADR.
- The marker is added to every review as the last step of body assembly, after the optional notices are prepended, so "ends the body" holds regardless of what comes before.
- Counts describe findings only. The marker carries no review event decision, severity breakdown or head SHA.
- Existing reviews posted before this change carry no marker. Consumers treat absence as "unknown" and keep today's behaviour.
- Existing visible text and the `PRUEFER_COVERAGE` line are unchanged.

## Out of Scope *(optional)*

- Reading the marker in the engine and skipping worker rounds for clean reviews (#2489).
- Changing how findings are anchored, partitioned or de-duplicated.
- Making findings inside the body consumable by the engine's inline-comment review path.
- Adding the marker to non-review Pruefer comments, such as the diff-too-large notice or the skill notices.
- Closing community report #2453, which is closed by hand once a release ships.

## Source References *(optional)*

- Community report #2453 and the flow analysis it links (comment: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874; data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec)
- #1045 (engine treats every Pruefer body as potentially actionable), #2489 (engine-side consumer), ADR 2307 (review skill fallback)
- Producer code: `pruefer/findings.go` (`buildReviewBody`, `parseReviewFindings`), `pruefer/coverage.go` (`appendCoverage`), `pruefer/review.go` (body assembly)
