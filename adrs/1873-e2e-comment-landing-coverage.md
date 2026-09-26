# ADR 1873: e2e coverage for unprocessed comments at the landing boundary

**Status:** Accepted
**Date:** 2026-09-26
**Issue:** [#1873](https://github.com/handarbeit/fabrik/issues/1873)

## Context

0.0.83 changed what an unprocessed comment does at the landing boundary — mentioned for
context: #1862 (ADR-1862: an unprocessed comment blocks auto-merge, direct merge and the
advance to Queued; a comment arriving after the work landed gets a "comment not applied"
reply and no 🚀) and #1863 (ADR-1863: an unprocessed human comment on a Queued merge-train
member ejects it, uncounted and unpaused). Unit and sim tests cover the logic; nothing covered
the live wire — real comment timing against real polls, real reactions, the real merge. Per
ADR-1454 the sim bed is a pre-gate, never a replacement for live coverage.

This is test-only work: no engine behaviour or logging changes. Three scenarios were added
under `tests/e2e/` (`TestCommentLandingGateHolds`, `TestQueuedMemberCommentEjection`,
`TestPostMergeCommentNotApplied`). The decisions below record why they are built the way they
are; a future contributor would otherwise have to rediscover each of them.

## Decisions

### 1. arbeithand reads as human in both auth legs

The harness posts as `FABRIK_TOKEN`'s account (arbeithand). `IsBotLogin("arbeithand")` is
false, so `filterHuman` keeps it, and `findNewComments` excludes only `🏭 **Fabrik`-prefixed
bodies, 🚀'd/watermarked comments and bot service notices. The comment gate, the advance guard,
the Queued-eject detector and the post-merge guard all read that same predicate.

- **PAT leg:** Fabrik posts as the same account, so a harness comment is distinguished from an
  engine comment *only* by the `🏭 **Fabrik` prefix. `postHumanIssueComment` therefore refuses
  any body with that prefix (`humanCommentBodyOK`).
- **App leg:** Fabrik posts as `<slug>[bot]`; arbeithand is a genuine human to it.

`FABRIK_REVIEWER_TOKEN` is not needed for classification. It supplies a deterministic
`APPROVE` so the review gate never decides when the item lands, and the two scenarios that
need it skip cleanly without it. No assertion keys on a reaction's `user.login`, which differs
by leg; only existence and timestamps are checked. Every harness comment also says "no code
change", so the comment worker never pushes (a push would re-run Validate via the SHA
invalidation scan and shift the landing timeline).

### 2. S1 places the comment by leaving the item Status-less

The comment must be present before the landing decision, and the assertion must not depend on
timing luck. The item is seeded (yolo, `stage:Validate:complete`, non-draft member PR) with no
board Status, so the engine cannot act on it; the scenario waits for CI green and submits the
approval, posts the comment and verifies it through REST, and only then moves the item into
Validate. The first landing evaluation always sees the comment. Rejected: holding the item with
`wait_for_ci` or a label — Phase 1's CI gate could claim the item and process the comment
earlier, voiding the hold assertion.

The scenario asserts the `comment-gate` line under **both** train modes. The issue text names
the `advance` "skipping stage" line for `merge_train: on`, but that line fires only for
non-Validate stages (`engine/poll.go`); at Validate the gate sits ahead of the merge-train
fork inside `attemptMergeOnValidate`, and `advanceToQueued` is reached only after it clears. So
under `merge_train: on` the extra observable is that the item is never Queued while the
comment has no 🚀. A Review-stage sub-case for the `advance` line was rejected as costing an
extra Claude comment-review for little gain.

### 3. S2 uses an occupant member so the eject is direct

`handleMergeTrainBatch` runs before `settleQueuedReviewFindings` in a poll, so queuing a member
and commenting on it cannot deterministically produce a direct eject: a member in the
just-formed batch is ejected via the pending-signal route, which a worker on the singleton fast
path can miss. An occupant member M1 (own CI still pending) keeps a worker in flight for the
(repo, base) partition for the bed's ~10 min `slow-gate` trial. After
`opened draft CI PR … (1 survivor(s))`, M2 is queued and commented on; it is never in the fixed
`batchNumbers`, so the settle scan ejects it directly. The scenario fails with an explicit
"occupant window closed" message if M2 ever appears in a batch snapshot, so a harness problem
is never reported as an engine regression. It is not parallel, with a stale-Queued pre-flight,
like the other partition-sensitive train scenarios.

### 4. "Not counted" is asserted indirectly

`mergeTrainEjectionCounts` is in memory and not observable, and adding engine logging is out of
scope. A counted ejection (`ejectMember`) leaves distinct artifacts: a
`🏭 **Fabrik merge-train — ejected**` comment (note `ejected**` directly after the em dash, not
`ejected (unprocessed comment)**`), and at the cap a `pausing after N ejections` comment, an
`ejected N time(s) — pausing` log line and `fabrik:paused`. The scenario asserts exactly one
comment-eject comment and none of those artifacts. This is the strongest observable available
without engine changes, and it is described as indirect in the PR.

### 5. S3 parks at Implement with a non-closing PR

"Merged and still open" is a tiny window: GitHub closes the issue via `Closes #N`, and at
Validate the terminal advance moves the item to Done. The member PR is opened without a closing
keyword (the engine resolves the linked PR by the `fabrik/issue-<N>` branch, not the body) and
merged with `--admin`, and the item is parked at Implement, which has neither `wait_for_ci` nor
`wait_for_reviews`. Rejected: reopening a merged issue as the sim does (racy live — the
terminal advance or `settleClosedItemsToDone` can act between merge and reopen), and faking
`fabrik:credited-pr:<N>` (`settleLandingVerification` would fail a made-up PR and reopen the
item, ADR-1616). A defensive re-read of issue state, labels, board Status and PR merged state
immediately before commenting fails loudly if the item was already disturbed, so the scenario
can never pass vacuously. It needs no Claude and is mode-invariant.

### 6. Ordering evidence comes from GitHub state

Ordering claims use REST timestamps only: reaction `created_at` (👀 <= 🚀) and the issue's
`closed_at` (and `merged_at` when the member PR itself merged) against the 🚀. GitHub timestamps
have 1-second resolution, so comparisons use `>=`, never `>`. The pure comparison and
log-parsing helpers are pinned by `comment_landing_helpers_test.go`, whose fixtures are the
engine's own format strings with the real `<RFC3339> [#N tag]` prefix.

## Non-vacuity

Argued, not run (the live suite is release-gate only):

| Assertion | Pre-fix behaviour that would make it fail |
|---|---|
| S1: `comment-gate` line, no auto-merge label / Queued / merge while the comment has no 🚀 | Without #1862, the first poll enables auto-merge or queues: no hold line, and the label/Status/merge shows up before the 🚀. |
| S1: 🚀 exists and `closed_at` >= 🚀 | Without the gate the merge precedes processing; the post-merge guard would then reply "not applied" with no 🚀. |
| S2: eject comment and `ejected for an unprocessed comment` line | Without #1863 the member stays Queued, nothing is posted, and the comment sits unprocessed. |
| S2: never paused, no counted-ejection artifact | Had the eject gone through `ejectMember`, the counted comment and cap logic would appear. |
| S3: reply present, no reaction, no `comments … processing` line, tip unchanged | Without #1862 a worker is dispatched (👀/🚀, possibly a push) and no reply is posted. |

## Consequences

- The release gate gains three scenarios; costs and wall-clocks are in `tests/e2e/README.md`.
- If the engine's log or comment wording changes, the unit-pinned parsers fail in CI and the
  live scenarios fail loudly rather than pass vacuously.
- Bed stage YAML (Validate `wait_for_*`, Implement having neither) is assumed to match the
  shipped defaults; S3's pre-comment re-read is the guard if the bed differs.
