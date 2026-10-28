# Feature Specification: Cold-base merge-train live test must not time out to FAIL when a bot review splits the batch

**Feature Branch**: `fabrik/issue-2481`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

`TestMergeTrainColdCacheBaseMember` failed on the 0.0.90 gate. The run was `app/on` on bed B, engine `d6866128`, invocation `20261026T045615Z-e31de8`. The same test passed on `pat/on` in the same run and on `app/on` in the previous run. This is a harness race, not an engine regression.

**Sequence:**
- The test seeds two members, #1278 and #1280, directly at Queued while the bed is stopped.
- The dev Pruefer, which watches the bed repos, posted a COMMENTED review on member PR #1279. The engine counted it as an unprocessed review body (`queued-review-settle`: 1 unprocessed review finding) and ejected #1278 mid-trial.
- #1280 then landed alone through the singleton fast path. The engine logged `landing complete for handarbeit/fabrik-test-gamma:e2e-cold-base-20261026-095451 (singleton fast path, PR #1281, 1 member)`.

`parseLandingComplete` (`tests/e2e/mergetrain_helpers.go`) matches only `(integration PR #N, M members)`. So the A1 wait never matched. It ran out its full 35 minutes and ended in a **FAIL**. The test's own guard (\"landed via the singleton fast path … the members did not batch together\") would have made it Inconclusive.

## Requirements

1. **Recognise the singleton-fast-path landing line.** The A1 wait must also recognise the `landing complete for <trainKey> (singleton fast path, PR #N, 1 member)` line. A batch that did not form (members split by an eject, or a singleton landing) must end as **Inconclusive** through the existing guard, never as a 35-minute timeout followed by FAIL. Other callers of `parseLandingComplete` keep their current meaning. Add a second parser or a variant if needed, rather than widening the shared one silently.
2. **Keep a bot review body from splitting the batch.** A bot review body on a seeded member must not be able to split the batch. Options:
   - seed the members so the engine treats any review body as already addressed (the `review-ids-addressed` marker), or
   - wait for the reviewer's review before restarting the bed and mark it processed, or
   - detect the eject in the bed log and report Inconclusive.

   Choose in Research. The test must not depend on Pruefer being slow.
3. **Check the other merge-train live tests.** Check whether the other merge-train live tests that seed members directly at Queued have the same exposure, and fix those that do.

## Acceptance

- A unit test of the A1 matcher shows that a singleton-fast-path landing line ends the wait as not-batched, not as a timeout.
- The scenario no longer fails when a bot reviews a seeded member before the bed restarts.

Mentioned for context: #1688."

## Background

`TestMergeTrainColdCacheBaseMember` is a live e2e test of the release gate. It seeds two merge-train members directly at Queued while the bed is stopped, restarts the bed, and expects both to land together as one batch (an integration PR with two members). The gate is accepted on per-(test, leg) PASS records, so a spurious FAIL in a single cell blocks the release even though the engine behaved correctly.

On the 0.0.90 gate the test failed on `app/on` (bed B, engine `d6866128`, invocation `20261026T045615Z-e31de8`) while passing on `pat/on` in the same run and on `app/on` in the previous run. The cause was a race between the test's seeding and the dev Pruefer reviewer, which watches the bed repositories:

1. Pruefer posted a COMMENTED review on a seeded member's PR (#1279). The engine counted the review body as unprocessed feedback (`queued-review-settle`) and ejected that member (#1278) mid-trial.
2. The other member (#1280) then landed alone through the singleton fast path, logging `landing complete for <trainKey> (singleton fast path, PR #N, 1 member)`.
3. The test's first wait (A1) only recognises the batch form of the landing line (`integration PR #N, M members`), so it never matched, ran out its 35-minute budget and ended in FAIL. The test already contains a guard that classifies "landed via the singleton fast path — the members did not batch together" as Inconclusive, but the wait never reaches it.

This is a test-harness defect, not an engine regression: ejecting a member with an unprocessed review is intended engine behaviour. The failure mode is a long timeout converted into a hard FAIL for a condition the test already knows how to classify as Inconclusive. The same exposure (a bot review landing on a member seeded straight into Queued before the bed starts) may exist in other merge-train live tests.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A split batch is reported promptly as Inconclusive (Priority: P1)

A release engineer runs the live gate. A seeded member is ejected (or one member lands alone) so the two members never batch. The test must notice that the batch will not form and report Inconclusive instead of waiting out the whole budget and failing.

**Why this priority**: This is the symptom that failed the gate. Even if the race can still occur, the outcome must never be a FAIL, and it must not burn 35 minutes first.

**Independent Test**: Feed the A1 landing-line matcher a singleton-fast-path landing line (and, separately, a batch landing line and unrelated lines) and assert the classification, with no bed or network.

**Acceptance Scenarios**:

1. **Given** the bed log contains `landing complete for <trainKey> (singleton fast path, PR #N, 1 member)` for the test's partition, **When** the A1 wait evaluates the log, **Then** the wait ends immediately as "not batched" and the test is reported Inconclusive through the existing guard.
2. **Given** the bed log contains `landing complete for <trainKey> (integration PR #N, 2 members)`, **When** the A1 wait evaluates the log, **Then** it behaves exactly as before and the test proceeds.
3. **Given** no landing line has appeared, **When** the wait budget elapses, **Then** the behaviour is unchanged from today (a genuine timeout is still a failure).

---

### User Story 2 - A bot review of a seeded member cannot split the batch (Priority: P1)

A release engineer runs the gate while the dev Pruefer is active. A bot review body lands on a seeded member before or after the bed restarts. The two seeded members must still batch together, independent of how fast or slow the reviewer is.

**Why this priority**: Reporting Inconclusive is correct but still leaves the cell uncovered and forces a rerun. Removing the race lets the test actually pass.

**Independent Test**: Seed the members, have a bot review body present on one member PR before the bed starts (and, where feasible, one arriving after), and confirm the members still land as one batch (or, if the chosen design cannot guarantee that, the test ends Inconclusive and never FAIL).

**Acceptance Scenarios**:

1. **Given** a bot review body exists on a seeded member before the bed restarts, **When** the engine evaluates the Queued members, **Then** the member is not ejected on account of that review and the members batch together.
2. **Given** a bot review arrives on a seeded member after the bed restarts, **When** it splits the batch, **Then** the test ends Inconclusive, not FAIL.
3. **Given** the reviewer is fast, slow, or absent, **When** the test runs, **Then** the outcome does not depend on its timing.

---

### User Story 3 - Other merge-train live tests are not exposed to the same race (Priority: P2)

A release engineer wants the whole merge-train live suite to be stable against bot reviews of seeded members, not just this one test.

**Why this priority**: The same seeding pattern in other tests carries the same gate-blocking risk, but none has failed yet.

**Independent Test**: For each merge-train live test that seeds members directly at Queued, review whether a bot review body could eject a member or split its batch, and apply the same protection to those that can.

**Acceptance Scenarios**:

1. **Given** the set of merge-train live tests that seed members directly at Queued, **When** each is examined, **Then** each is either shown not to be exposed (with the reason noted) or is given the same protection as User Story 2.
2. **Given** a test whose assertions legitimately depend on a review being processed (for example, a test of review-driven ejection), **When** it is examined, **Then** it is left unchanged and the reason is recorded.

---

### Edge Cases

- The singleton landing line appears for a different partition or trainKey than the test's own; the matcher must only react to the test's partition, not unrelated trains on a shared bed.
- Both a singleton landing line and a later batch landing line appear; the first-in-time outcome for the test's partition decides.
- The line format gains or loses whitespace/pluralisation (`1 member` vs `N members`); the matcher is tolerant of the plural.
- Other callers of the shared landing-line parser depend on its current batch-only meaning and must not change behaviour.
- A review arrives after the batch has already landed; this has no effect on the outcome.
- A bot review is a COMMENTED review body, not an inline thread; both kinds of bot feedback should be considered when judging exposure.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The A1 wait of `TestMergeTrainColdCacheBaseMember` MUST recognise the singleton-fast-path landing line `landing complete for <trainKey> (singleton fast path, PR #N, 1 member)` for the test's own partition.
- **FR-002**: When the singleton-fast-path landing line (or any other evidence that the two members did not batch) is observed, the A1 wait MUST end promptly and the test MUST be reported Inconclusive through its existing "members did not batch together" guard, never as a timeout followed by FAIL.
- **FR-003**: Recognition of the singleton line MUST NOT change the meaning of the existing shared batch-landing parser for its other callers; it is added as a separate parser or variant.
- **FR-004**: A bot review body on a seeded member MUST NOT be able to split the batch in this test. The mechanism is chosen during Research from: seeding the members so any review body is already treated as addressed; waiting for the reviewer's review before restarting the bed and marking it processed; or detecting the eject in the bed log and reporting Inconclusive.
- **FR-005**: The test's outcome MUST NOT depend on the dev Pruefer being slow, fast or absent.
- **FR-006**: A genuine failure to land (no landing line of either kind within the budget, with the members still batched or still queued) MUST remain a FAIL.
- **FR-007**: Every other merge-train live test that seeds members directly at Queued MUST be reviewed for the same exposure; those found exposed MUST be given the same protection, and those found not exposed MUST have the reason recorded in the change.
- **FR-008**: A unit test of the new matcher MUST show that a singleton-fast-path landing line ends the wait as not-batched, that a batch landing line is still recognised as before, and that an unrelated or other-partition line is ignored.

### Key Entities

- **Seeded member**: A merge-train member placed directly at Queued by the test while the bed is stopped, with its own PR.
- **Landing-complete log line**: The engine's bed-log record that a landing finished, in one of two forms — batch (`integration PR #N, M members`) or singleton fast path (`singleton fast path, PR #N, 1 member`).
- **Bot review body**: A COMMENTED review posted by a reviewer bot (the dev Pruefer) on a member PR, which the engine counts as unprocessed feedback.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: When a seeded member is ejected or a single member lands alone, the test ends Inconclusive within the same poll cycle that the line appears, instead of after the full 35-minute wait, and is never reported as FAIL.
- **SC-002**: Across repeated gate runs with the dev Pruefer active, the cold-base scenario produces no FAIL attributable to a bot review of a seeded member.
- **SC-003**: The new matcher's unit test passes in plain `go test ./...` with no bed, network or build tag, and existing tests of the shared batch-landing parser pass unchanged.
- **SC-004**: Every merge-train live test that seeds members directly at Queued has a recorded exposure verdict (protected or not exposed, with reason).

## Assumptions

- The singleton-fast-path outcome is correct engine behaviour when one member is ejected; only the test's classification is wrong.
- Reporting Inconclusive for a split batch is acceptable as the fallback; the primary goal (FR-004) is to prevent the split in the first place where practical.
- The log-line wording quoted in the issue is the current engine output and is stable enough to match structurally.
- The choice between the three protection mechanisms is a Research/Plan decision, not a product decision, since all three satisfy the requirement.

## Out of Scope *(optional)*

- Any change to the engine's handling of review bodies on Queued members (ejecting a member with an unprocessed review is intended).
- Changing the dev Pruefer's behaviour or timing.
- Changing the meaning of the shared batch-landing parser for its existing callers.
- Restructuring the live-test registry or the gate's coverage rules.
- Merge-train live tests that do not seed members directly at Queued.

## Source References *(optional)*

- Mentioned for context: #1688.
- Failed run: 0.0.90 gate, `app/on`, bed B, engine `d6866128`, invocation `20261026T045615Z-e31de8`.
- `tests/e2e/mergetrain_helpers.go` (`parseLandingComplete`), `tests/e2e/mergetrain_coldbase_test.go`
