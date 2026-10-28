# Feature Specification: Pause cause as a label; reserve awaiting-input for real questions

**Feature Branch**: `fabrik/issue-2487`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "pauses: put the pause cause on the issue as a label; reserve awaiting-input for real questions

> **Problem** — Every engine pause applies `fabrik:paused` + `fabrik:awaiting-input`, whether a worker asked a question (`FABRIK_BLOCKED_ON_INPUT`) or the engine stopped on a timeout, a cycle limit, a clean stop or a breaker. On the board, a review-wait timeout looks exactly like a worker waiting for an answer. The report counts 43 real questions but many more `awaiting-input` labels; the reporter had to reconstruct causes from comments. The cause already exists internally (`attention.Result.Cause`, the pause-cause markers), but not where people look.
>
> **Requirements** — 1. Each engine pause states its cause on the issue as a label from a fixed vocabulary (for example `fabrik:pause:question`, `…:review-timeout`, `…:ci-timeout`, `…:cycle-limit`, `…:daemon-stop`, `…:breaker`, `…:other`), written with the pause and removed with it. 2. `fabrik:awaiting-input` is applied only when someone actually asked a question (`FABRIK_BLOCKED_ON_INPUT`, an ungrantable-denial question, a spawn servability question — Research enumerates them) — **or** Research shows that changing its meaning is unsafe, in which case keep it and the cause label alone carries the distinction. Decide in an ADR; either way, resume semantics (`resumeAuthorised`, ADR-1813) stay exactly as they are. 3. Every `pauseIssue` call site maps to one cause; a test fails when a new call site has no cause (static scan, like the warnings-panel scan). 4. Every new label gets an `attention.MeaningOf` entry (the labels scan enforces it).
>
> **Acceptance** — Unit/sim coverage for each cause; scan test proves non-vacuous by adding an uncaused call. `docs/state-machine.md` label table and `CLAUDE.md` label list updated; `docs/llms-full.txt` regenerated.
>
> **Notes** — Source: community report #2453 (totalslacker's flow analysis of `totalslacker/chronicle`, Oct 1–26: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874; data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec). Mention #2453 in the PR body by bare number only. Do **not** use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it — the report is closed by hand when a release ships. The PR closes this work issue only."

## Background

Today every engine pause applies the same pair of labels, `fabrik:paused` + `fabrik:awaiting-input`. That pair is used both when a worker has genuinely asked a human a question (`FABRIK_BLOCKED_ON_INPUT`) and when the engine itself stopped for a reason that is not a question at all: a review-wait or CI-wait timeout, a cycle limit, a clean daemon stop, a circuit breaker, and so on. On the board, a review-wait timeout therefore looks identical to a worker waiting for an answer.

Community report #2453 (a flow analysis of a real board over Oct 1–26) counted 43 real questions but many more `awaiting-input` labels, and the reporter had to reconstruct each pause's cause from its comment text. The cause is already known inside the engine (the attention classifier's `Cause`, and the pause-cause markers embedded in pause comments), but it is not visible where people look: as a label on the issue, filterable on the board.

This work makes the cause a first-class, visible, filterable fact of every engine pause, and stops `awaiting-input` from implying "a human was asked something" when no one asked anything — unless Research shows that narrowing it is unsafe, in which case the cause label alone carries the distinction.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See why an issue is paused from the board (Priority: P1)

An operator looks at the project board (or filters issues by label) and can tell, for every paused issue, *why* it is paused — a question for a human, a review timeout, a CI timeout, a cycle limit, a daemon stop, a breaker, or something else — without opening the issue or reading comments.

**Why this priority**: This is the core pain in the report. Everything else supports it.

**Independent Test**: Trigger one pause of each cause in the sim bed and assert the paused issue carries exactly one cause label matching the trigger.

**Acceptance Scenarios**:

1. **Given** a worker emits `FABRIK_BLOCKED_ON_INPUT`, **When** the engine pauses the issue, **Then** the issue carries `fabrik:paused` and the "question" cause label.
2. **Given** the review gate times out, **When** the engine pauses the issue, **Then** the issue carries `fabrik:paused` and the "review-timeout" cause label.
3. **Given** a paused issue is resumed (by a human comment, as today), **When** the engine lifts the pause, **Then** the cause label is removed together with `fabrik:paused`.
4. **Given** an issue was paused for one cause and is later paused again for a different cause, **When** the second pause is applied, **Then** the issue carries only the second cause label.

---

### User Story 2 - `awaiting-input` means someone was asked something (Priority: P1)

A person scanning for "things waiting on me" can trust that `fabrik:awaiting-input` marks only pauses where a question was actually put to a human (a worker's blocked-on-input question, an ungrantable-denial question, a spawn-servability question — Research enumerates the full set). Timeouts, limits, stops and breakers do not carry it.

**Why this priority**: Equal to Story 1 in value, but conditional: Research must first confirm that narrowing the label is safe. If it is not, the cause label alone carries the distinction (see FR-005).

**Independent Test**: For each pause cause, assert whether `fabrik:awaiting-input` is present according to the decision recorded in the ADR; count of `awaiting-input` issues equals count of question pauses in a mixed scenario.

**Acceptance Scenarios**:

1. **Given** the ADR decides to narrow `awaiting-input`, **When** a review-wait timeout pauses an issue, **Then** the issue has `fabrik:paused` and the review-timeout cause label but not `fabrik:awaiting-input`.
2. **Given** the ADR decides to narrow it, **When** a worker asks a question, **Then** the issue has `fabrik:paused`, `fabrik:awaiting-input` and the question cause label.
3. **Given** the ADR decides to keep it on every pause, **When** any pause is applied, **Then** behaviour of `awaiting-input` is unchanged and the cause label is the sole distinguishing signal.

---

### User Story 3 - Resume behaviour does not change (Priority: P1)

An operator who resumes paused issues today (by a human comment created at or after the pause) sees no change in what resumes an issue, when, or how — whatever the cause, and whether or not `awaiting-input` is present.

**Why this priority**: The pause/resume contract (ADR-1813, `resumeAuthorised`) was hard-won; this change is presentation and classification only.

**Independent Test**: The existing resume tests (admission and both `processItem` resume gates) pass unmodified, and a new test resumes an issue of each cause with a post-pause human comment and fails to resume it with a pre-pause comment or a bot comment.

**Acceptance Scenarios**:

1. **Given** an issue paused with any cause label, **When** a human comments after the pause, **Then** it resumes exactly as it would today.
2. **Given** an issue paused with any cause label, **When** only a pre-pause or bot comment exists, **Then** it does not resume.
3. **Given** a paused issue without `awaiting-input` (if narrowed), **When** a human comments after the pause, **Then** it still resumes.

---

### User Story 4 - A new pause site cannot ship without a cause (Priority: P2)

A maintainer adding a new place where the engine pauses an issue is stopped by a failing test until the site declares which cause it maps to, so the vocabulary cannot silently rot.

**Why this priority**: Protects the invariant over time; follows the pattern of the warnings-panel scan.

**Independent Test**: Add a deliberately uncaused pause call to a scratch copy and show the scan fails; remove it and show the scan passes.

**Acceptance Scenarios**:

1. **Given** every existing `pauseIssue` call site declares a cause, **When** the scan runs, **Then** it passes.
2. **Given** a new `pauseIssue` call with no cause, **When** the scan runs, **Then** it fails naming the call site.

---

### User Story 5 - Labels are explained and documented (Priority: P2)

Anyone reading the label list, the state-machine doc, or the attention/MCP views sees each new label with a one-line meaning.

**Why this priority**: Required by existing repo conventions (the labels scan) and by the docs-drift CI.

**Independent Test**: The `attention.MeaningOf` labels scan passes with the new labels; the docs-drift check passes after regeneration.

**Acceptance Scenarios**:

1. **Given** the new cause labels exist in the engine, **When** the labels scan runs, **Then** each has a `MeaningOf` entry and the scan passes.
2. **Given** the docs are updated, **When** `docs/llms-full.txt` is regenerated, **Then** the committed bundle matches the regeneration output.

---

### Edge Cases

- **Pause replaced by a different pause**: the old cause label is removed when the new one is written; at most one cause label is ever present.
- **Engine pause lifted by any path** (human comment resume, `fabrik:revalidate`, forced re-run, clear-failed-stage, Done cleanup, and so on): every path that removes `fabrik:paused` also removes the cause label.
- **Human removes `fabrik:paused` by hand in the UI**: a cause label left behind with no pause is stale and must be cleaned up by the engine rather than left to mislead.
- **Human applies `fabrik:paused` by hand**: there is no engine cause; the item shows no cause label (not a fabricated one), and still resumes as today.
- **Issues paused before the upgrade** (no cause label): behave as they do today; they are not required to be migrated, and the attention classifier falls back to its existing cause derivation for them.
- **Cause label write fails while the pause itself succeeds**: the pause must not be lost; the failure is logged and the cause label is retried or the item is left in the existing, still-correct paused state.
- **A pause site whose cause differs by situation** (one call site, several causes): the scan requires the cause to be decided per call, not per file.
- **Pause comment dedup, pause-cause markers in comments, and `attention.Result.Cause`** continue to work; the label must not disagree with the cause they report for the same pause.
- **Webhook-less (poll-only) operation**: the label appears on the next poll like any other label write; no ordering assumptions beyond the existing label write-through.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every pause the engine applies (every path that adds `fabrik:paused`, including the `pauseIssue` helper and any direct label writes Research enumerates) also applies exactly one cause label from a fixed, closed vocabulary, written with the pause.
- **FR-002**: The vocabulary covers at least: a question put to a human, review-wait timeout, CI-wait timeout, cycle limit, daemon stop, circuit breaker, and an "other" catch-all. Research may split or rename causes (the names above are provisional) but the final set is fixed, documented in one place, and every label in it uses one shared, recognizable prefix.
- **FR-003**: The cause label is removed together with `fabrik:paused` on every path that lifts, clears or supersedes a pause; at most one cause label is present on an item at any time; a cause label with no `fabrik:paused` on the item is treated as stale and removed.
- **FR-004**: Research enumerates every place the engine pauses (including the question-type pauses: `FABRIK_BLOCKED_ON_INPUT`, ungrantable-denial question, spawn-servability question) and every place `fabrik:awaiting-input` is read or written, and assigns each pause exactly one cause.
- **FR-005**: `fabrik:awaiting-input` is applied only to pauses where a question was actually put to a human — **or**, if Research shows narrowing its meaning is unsafe, it is left on every pause and the cause label alone carries the distinction. The choice and its reasoning are recorded in an ADR numbered for this issue (`adrs/2487-…`).
- **FR-006**: Pause and resume semantics are unchanged: a paused item is resumed only by a human comment created at or after the latest `fabrik:paused` label event (`resumeAuthorised`, ADR-1813), regardless of cause and regardless of whether `fabrik:awaiting-input` is present. No admission or resume decision may depend on the cause label.
- **FR-007**: Every `pauseIssue` call site maps to exactly one cause, and a static scan test fails when a call site has no declared cause. The test is proven non-vacuous by demonstrating it fails when an uncaused call is added.
- **FR-008**: Every new label has an `attention.MeaningOf` entry with a one-line meaning, and the existing labels scan passes.
- **FR-009**: The cause shown by the label agrees with the cause the attention classifier and the pause-cause markers report for the same pause; where the classifier currently derives a cause some other way, it is made consistent (or reads the label) so the two cannot disagree.
- **FR-010**: Where the question-vs-not distinction already feeds attention views, channel events or MCP outputs (for example the "needs a human reply" classification and next-action hints), those outputs remain correct under the chosen `awaiting-input` decision for every cause.
- **FR-011**: Test coverage exists for each cause (unit and/or sim): the correct cause label is applied with the pause, removed on resume, and replaced on a differing re-pause; and for `awaiting-input` presence per the ADR decision.
- **FR-012**: `docs/state-machine.md` (label table and pause description) and the `CLAUDE.md` label list are updated, and `docs/llms-full.txt` is regenerated in the same change.
- **FR-013**: The PR body mentions community report #2453 by bare number only, with no closing keyword near it; the PR closes only this work issue.

### Key Entities *(if applicable)*

- **Pause cause label**: a `fabrik:*` label from the fixed vocabulary naming why the engine paused an item; engine-owned, never human-applied; lives and dies with `fabrik:paused`.
- **`fabrik:awaiting-input`**: the label meaning (after this work, subject to the ADR) "a question was put to a human".
- **Pause cause (internal)**: the existing `attention.Result.Cause` and pause-cause comment markers, which the label must agree with.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For 100% of engine-applied pauses in the test suites, the paused item carries exactly one cause label that matches the triggering condition; an operator can filter the board by that label to list all pauses of one cause.
- **SC-002**: If the ADR narrows `awaiting-input`, then in a mixed scenario covering every cause, the number of items carrying `fabrik:awaiting-input` equals the number of question pauses; if the ADR keeps it, the cause label alone partitions the paused items with no overlap and no gaps.
- **SC-003**: All pre-existing pause/resume tests pass without modification, and the new per-cause resume tests show identical resume behavior across all causes.
- **SC-004**: The static scan fails on an added uncaused pause call and passes on the real tree; the labels scan and docs-drift checks pass.
- **SC-005**: No paused item in any test ends a scenario with a stale cause label (cause label present, pause absent) or with two cause labels.

## Assumptions

- The vocabulary names in the issue are illustrative; the exact label names are finalized during Research/Plan, subject to one shared prefix and a closed set.
- Items already paused when the new version starts are not migrated; they simply have no cause label until next paused, and keep their present `awaiting-input` behavior.
- Human-applied `fabrik:paused` has no engine cause and gets no fabricated cause label.
- Whether `awaiting-input` is narrowed is a Research/ADR decision (FR-005); this spec is valid under either outcome, which is why it is not an open question.
- Cause labels are engine state, written and removed through the engine's normal label-write path, with the same ordering, echo and write-through behavior as other engine labels.
- The PM plugin docs and MCP/attention outputs that name `awaiting-input` are updated only as far as FR-010 requires.

## Out of Scope *(optional)*

- Any change to who or what can resume a paused item, or to `resumeAuthorised`.
- New pause conditions, new timeouts, or changes to when the engine decides to pause.
- Migrating existing paused issues or backfilling cause labels onto history.
- New overseer/MCP tools to set or lift pauses.
- Changing the content of pause comments beyond what consistency with the label requires.
- Reworking the report author's analysis tooling or the public triage board (report #2453 is closed by hand when a release ships).

## Source References *(optional)*

- Community report #2453 and its flow-analysis comment: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874
- Analysis data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec
- ADR-1813 (pause resume anchoring) and `docs/state-machine.md` §4.4; existing `attention.Result.Cause` vocabulary and pause-cause markers (`docs/state-machine.md` §7.13/§7.14).
