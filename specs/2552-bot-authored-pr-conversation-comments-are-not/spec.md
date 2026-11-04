# Feature Specification: Bot-authored PR conversation comments are not feedback unless the bot is opted in

**Feature Branch**: `fabrik/issue-2552`
**Created**: 2026-11-04
**Status**: Specified
**Input**: User description: "## Problem

The linked PR's conversation comments are merged into `item.Comments` (`github/project.go`, tagged `FromPR`). `filterNewComments` (`engine/comments.go`) is the one funnel used by `processItem`, `unprocessedFeedback` and the cache-only channel path. It drops a bot comment only when it matches a known service-notice pattern (`isBotServiceNotice`). Any other bot comment, such as Vercel's deploy notice on every `fabrik/*` PR, is treated as feedback. It starts a paid comment-review pass that concludes there is nothing to act on, and it holds the feedback gate until then. Measured on 0.0.89: about $1.40–$3.20 per issue, recurring on every issue that reaches a PR.

Reported in community report #2497 (mention by number only).

## Requirements

- R1: In `filterNewComments`, a comment whose author is a bot login (`gh.IsBotLogin`) is not feedback, unless that bot is opted in.
- R2: The stage's effective `expected_reviewers` (`effectiveExpectedReviewers` + `reviewerIdentityMatches`, as used by `cleanReviewSkipper`) opts a bot in. If that is too narrow for comment-only bots, add an explicit opt-in key. Decide in the ADR.
- R3: Every reader of the funnel agrees, because they share the function. Add an agreement test in the style of `TestCleanReviewAgreesAcrossReaders`.
- R4: Review threads and review bodies (separate builders) are unchanged. `CHANGES_REQUESTED` handling and the review gate are untouched.
- R5: A skipped bot comment is derived state. Write no reaction and no processed marker, and add no comment.

## Acceptance

- A sim twin: a `vercel[bot]` PR comment after Implement opens the PR starts no comment-review worker and holds no gate. A declared bot's comment still does. It fails with R1 neutralised.

## Linkage

Mention #2497 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) with it. Related for context: #2489, #2414."

## Background

The linked PR's conversation comments are merged into the item's comment list (tagged as coming from the PR). One shared function decides which of those comments count as unprocessed feedback, and every reader of "does this item have feedback?" goes through it: the stage/comment dispatcher, the feedback gate that holds advance and landing, and the cache-only channel-event path.

Today that function drops a bot comment only when it matches a short list of known service-notice phrasings (quota banners, rate-limit notices, CodeRabbit acknowledgements). Every other bot comment is treated as human-style feedback. Deploy-preview bots such as Vercel post a notice on every `fabrik/*` PR, so on every issue that reaches a PR the engine:

- starts a paid comment-review pass that concludes there is nothing to act on, and
- holds the feedback gate until that pass finishes.

Measured on 0.0.89 this costs about $1.40–$3.20 per issue and recurs on every issue that reaches a PR. It was reported in community report #2497 (mentioned here by number only, for context).

The engine already has the opposite, narrower rule for review bodies: a clean review from an *expected* reviewer is not actionable (#2489), and a bot is "declared" through a stage's effective `expected_reviewers`. This issue applies the same idea to conversation comments: a bot is ignored as a source of feedback unless the operator has said it is one whose comments matter.

Related for context: #2489, #2414.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A deploy-preview bot's comment no longer costs a worker (Priority: P1)

An operator runs Fabrik on a repository where Vercel (or any similar bot) comments on every pull request. After Implement opens the PR, the bot posts its deploy notice. Fabrik should treat that comment as noise: no comment-review worker starts, and the item is not held waiting for one.

**Why this priority**: This is the reported cost leak, recurring on every issue that reaches a PR.

**Independent Test**: Seed an item with an open PR carrying a single `vercel[bot]` conversation comment and no human comments; poll. Assert no comment-review worker is dispatched and the feedback gate reports nothing unprocessed.

**Acceptance Scenarios**:

1. **Given** an item at a stage after Implement whose linked PR has one unprocessed `vercel[bot]` conversation comment, **When** the engine polls, **Then** no comment-review worker starts for that comment.
2. **Given** the same item, **When** the stage completes and the engine evaluates advance or landing, **Then** the feedback gate does not hold on that comment.
3. **Given** the same item, **When** the comment is skipped, **Then** the engine has added no reaction, no processed marker and no comment because of it.

---

### User Story 2 - An opted-in bot's comment is still feedback (Priority: P1)

An operator has a bot whose conversation comments genuinely carry actionable feedback and has declared that bot as expected for the stage. Its comments must keep working as today.

**Why this priority**: Without this, the change would silently disable a working workflow for bots that comment instead of submitting formal reviews.

**Independent Test**: Same fixture as Story 1, but the bot is declared for the stage's effective expected reviewers (or the explicit opt-in chosen in the ADR). Assert a comment-review worker starts and the gate holds until it is processed.

**Acceptance Scenarios**:

1. **Given** a stage that declares a bot as opted in, **When** that bot posts an unprocessed conversation comment on the linked PR, **Then** it is treated as feedback: it is dispatched and holds the gate as before.
2. **Given** two bots on the same PR, one declared and one not, **When** both comment, **Then** only the declared bot's comment counts as feedback.
3. **Given** a per-issue `expected-reviewers:` label that changes the effective declaration, **When** the engine evaluates the comments, **Then** the effective (label-resolved) declaration decides, as it does for review bodies.

---

### User Story 3 - Every reader of the funnel agrees (Priority: P2)

The dispatcher, the feedback gate, the settle/admission paths and the cache-only channel path must give the same answer for the same comment, so that nothing is dispatched that the gate ignores, or held on something that is never dispatched.

**Why this priority**: The shared funnel is the reason the fix can be small; disagreement between readers would reintroduce hold-without-worker or worker-without-hold loops.

**Independent Test**: An agreement test, in the style of `TestCleanReviewAgreesAcrossReaders`, runs every reader over a bot-comment fixture (undeclared and declared) and asserts identical verdicts.

**Acceptance Scenarios**:

1. **Given** an undeclared bot comment, **When** each reader is asked whether it is feedback, **Then** all answer no.
2. **Given** a declared bot comment, **When** each reader is asked, **Then** all answer yes.
3. **Given** the item sits in the holding stage (Queued), **When** the readers resolve the stage, **Then** they use the same stage context the other review-feedback readers use there, so a decision made at Validate is not reversed at Queued.

---

### User Story 4 - Reviews and gates are untouched (Priority: P2)

Bots that submit formal reviews, or review threads, keep behaving exactly as today.

**Why this priority**: The review gate and `CHANGES_REQUESTED` handling are safety mechanisms; this change must not reach them.

**Independent Test**: Existing review-thread, review-body and review-gate tests pass unchanged; a bot `CHANGES_REQUESTED` review and a bot review-thread comment still count as feedback.

**Acceptance Scenarios**:

1. **Given** an undeclared bot that posts a review thread comment or a `CHANGES_REQUESTED` review, **When** the engine evaluates feedback, **Then** it is handled exactly as before this change.

---

### Edge Cases

- A bot comment that is also a known service notice: it stays excluded for undeclared bots (now simply because it is a bot) and, for declared bots, the existing service-notice exclusion still applies.
- A bot comment already carrying a rocket reaction or already processed: unaffected; still skipped as before.
- A Fabrik-authored comment (`🏭 **Fabrik` prefix, including the App's own bot login): already excluded, remains excluded.
- A human comment: never affected, even if the human's text quotes a bot.
- A comment with no resolvable author (deleted account): not treated as a bot by this rule; behaviour is whatever it is today.
- An undeclared bot comment that is skipped sits unprocessed forever; that must not cause re-admission on every poll or a perpetual hold.
- A bot that was declared and later removed from the declaration: its earlier unprocessed comments stop counting as feedback; a later re-declaration makes them count again (the skip is derived, not recorded).
- Bot comments on the issue itself (not the PR): same rule, since both come through the same function.
- Bots that currently rely on comment-based feedback without being declared (for example a bot summary that contains findings): after this change they are ignored until declared. This is an accepted behaviour change and must be documented.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The shared "unprocessed feedback comments" funnel MUST exclude a comment whose author is a bot login (as recognised by the engine's existing bot-login test) unless that bot is opted in for the stage being evaluated.
- **FR-002**: A bot MUST be opted in when it matches the stage's effective expected reviewers (stage declaration as overridden by the `expected-reviewers:` label), using the same identity matching the review-body skip uses. If this proves too narrow for comment-only bots, an explicit comment opt-in setting MAY be added; the choice and its name are recorded in the ADR.
- **FR-003**: An opted-in bot's comments MUST be treated exactly as bot comments are today (including the existing service-notice exclusion).
- **FR-004**: Every reader of the funnel (stage/comment dispatch, the feedback gate and its landing/advance consumers, the unprocessed-feedback predicate, the admission/dispatch views and the cache-only channel path) MUST apply the same rule with the same stage context, and an agreement test MUST fail if any reader diverges.
- **FR-005**: The rule MUST apply to bot-authored comments from either source (issue comments or PR conversation comments).
- **FR-006**: Review threads, review bodies, `CHANGES_REQUESTED` handling and the review gate MUST NOT change behaviour.
- **FR-007**: Skipping a bot comment MUST be derived state: the engine MUST NOT add a reaction, write a processed marker, post a comment, or change any label because of the skip.
- **FR-008**: Human comments, Fabrik-authored comments and the pause-resume predicate (human comments only) MUST be unaffected.
- **FR-009**: The behaviour change and the opt-in mechanism MUST be documented in the user guide and the as-built state-machine page, and the canonical documentation bundle regenerated.
- **FR-010**: A sim scenario MUST demonstrate that an undeclared `vercel[bot]` PR comment after Implement starts no comment-review worker and holds no gate, that a declared bot's comment still does, and MUST fail when FR-001 is neutralised via a test seam.

### Key Entities

- **Bot-authored comment**: a conversation comment whose author login is recognised as a bot.
- **Opt-in (declared bot)**: a bot named by the stage's effective expected reviewers (or the explicit opt-in key, if the ADR adds one).
- **Feedback funnel**: the single shared function that decides which comments are unprocessed feedback.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An issue whose PR receives only undeclared-bot conversation comments dispatches zero comment-review workers for those comments and incurs zero token cost from them.
- **SC-002**: The feedback gate never holds an advance or landing solely because of an undeclared bot's conversation comment.
- **SC-003**: A declared bot's conversation comment is dispatched and holds the gate, with behaviour identical to before the change.
- **SC-004**: All readers of the funnel return the same verdict for the same comment across undeclared, declared, and holding-stage fixtures (agreement test passes; it fails if any reader is made to differ).
- **SC-005**: No existing review-thread, review-body, review-gate or `CHANGES_REQUESTED` test changes outcome.
- **SC-006**: The new sim scenario passes, and fails with the bot-comment rule neutralised.

## Assumptions

- "Bot" means whatever the engine's existing bot-login test recognises; this issue does not redefine it.
- Whether declaring a bot through `expected_reviewers` is sufficient for comment-only bots is a design decision for the ADR; the default assumption is that it is, and a separate key is added only if a real need appears. The spec requires only that some operator opt-in exists.
- Opting a bot in is a stage-level (and label-overridable) decision, not repository- or global-level.
- The known service-notice patterns stay in place; they continue to apply to opted-in bots.
- Undeclared bots that previously produced useful comment-based feedback are rare compared with notice-only bots; operators who rely on them can declare them.

## Out of Scope *(optional)*

- Review threads, review bodies and the review gate (including the #2489 clean-review skip and #2414).
- Redefining what counts as a bot login or expanding the service-notice pattern list.
- Changing how the engine acknowledges comments (reactions) for human comments.
- Any cost-accounting or reporting changes beyond the behaviour above.
- Handling the community report #2497 thread itself (kept human-owned and closed by hand after a release ships).

## Source References *(optional)*

- Community report #2497 (mentioned for context only)
- #2489 and ADR 2489 (clean review from an expected reviewer is not actionable)
- #2414 (related)
- `docs/state-machine.md` §6.6.6 (feedback gate and review feedback)
