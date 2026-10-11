# Feature Specification: Admission re-checks transient merge-gate claims, expired cooldowns stop admitting, paused backstop fetches once per baseline

**Feature Branch**: `fabrik/issue-2096`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "admission: transient merge-gate claims re-check next poll, expired cooldowns stop admitting, paused backstop fetches once per baseline — see the verbatim original issue text below."

<details>
<summary>Original request (verbatim)</summary>

## Problem

Two reports share one admission defect.

**#1943.** A Validate-complete yolo item that the merge gate claims as `PRMergeUnsettled (CI checks pending)`, with no `fabrik:awaiting-ci`, isn't re-evaluated until its `periodic-re-eval` cooldown expires, which is `githubRecheckInterval()` = 10 × PollSeconds (`engine/retry_backoff.go:42-44`). At `poll: 180` that is **30 minutes** after CI goes green.
- Investigated on the original incident (concept-maps #1059, 2026-09-28): the last admission at 03:22:27 set `CooldownAt["periodic-re-eval"]` to about 03:52 in the poll defer (`engine/poll.go:1467-1487`).
- `selectDeepFetchCandidates` then skipped the item (`poll.go:2153-2172`).
- Nothing else could readmit it: no awaiting-ci, no webhooks, and CI completion doesn't move the issue's `updatedAt`.
- The merge-gate claim (`engine/merge_gate.go:41-47`, `engine/catch_up_handlers.go:536-543`) records no short cooldown, unlike `review-blocked` and `comment-pending`.
- The stale check-run cache was ruled out: the mergeable read is live, and `mergeable_state=clean` short-circuits.

**#2062.** The paused-item backstop (#1944, `poll.go:2216-2225`) deep-fetches from GitHub on every poll for up to 20 polls after a pause. Two causes:
- **`CooldownAt` entries are never removed** (`internal/itemstate/store.go:678-683`). Once any cooldown has expired, `HasExpiredCooldown` (`snapshot.go:173`) is true forever, so `periodicReeval` is true on every poll.
- **The re-anchor doesn't end the window.** `ItemDeepFetched` sets the baseline to GitHub's `UpdatedAt`, which for a fresh pause *is* the pause write, so `recentBaseline` stays true for 2 × `githubRecheckInterval`.

The same never-cleared defect means any fix to #1943 that records a short cooldown would, on expiry, keep the item admitted on every poll forever. So the two must be fixed together.

## Requirements

**R1. Transient merge-gate claims re-check promptly.** When the merge gate claims an item as `PRMergeUnsettled` or `PRMergeQueued` (the `mergeBlocked` branch), the item is re-evaluated on the next poll, not after the periodic re-eval interval. Use the existing cooldown pattern (e.g. a `merge-unsettled` reason expiring at now + PollSeconds), not a new mechanism.

**R2. Expired cooldowns stop admitting.** An expired cooldown admits the item once, then no longer counts. Either:
- the cooldown entry is cleared or consumed when the item is admitted on its strength; or
- `HasExpiredCooldown` considers only entries that expired since the last admission.

Plan picks one and audits every `CooldownAt` reason and reader, so no current gate relies on the sticky behaviour.

**R3. The paused backstop fetches at most once per baseline.** The #1944 backstop does a live fetch at most once per distinct baseline value per item, and is not repeated while the baseline is unchanged. Correct the code comment that claims "at most one or two extra fetches" so it matches the new behaviour.

**R4. No regression in the periodic re-check itself.** Items not otherwise admitted are still re-evaluated at the periodic interval.

**R5.** Update `docs/state-machine.md` (admission / §7) and regenerate `docs/llms-full.txt`.

## Scope

- In scope: merge-gate claim cooldown, cooldown expiry semantics, paused-backstop fetch bound, tests, docs.
- Out of scope: giving `mergeable=null` its own escalation bound or naming the blocker in the CI-timeout pause (#1809); applying `fabrik:awaiting-ci` on merge-gate claims.
- Mention #1943 and #2062 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near either. This issue's own number gets `Closes`.

## Acceptance

- **Sim scenario (should fail on current main):** Validate-complete yolo item, CI pending, merge gate claims it; CI goes green with no label or `updatedAt` change; the gate clears within one or two polls, not 10.
- **Sim/unit test:** after a cooldown expires and the item is admitted once, subsequent polls do not admit it on that cooldown's strength.
- **Unit test:** a paused item triggers at most one backstop deep fetch per baseline value across 20 polls.
- Each new test is shown to be non-vacuous by neutralising its fix.

</details>

## Background

Fabrik decides each poll which board items deserve a deeper (live) look. Two reports showed that decision going wrong in opposite directions, from one shared root cause: a cooldown that, once expired, counts as "expired" forever.

**Too slow (#1943).** A Validate-complete item running with `fabrik:yolo` can be held by the merge gate because its linked PR's CI is still pending. The gate holds it without applying `fabrik:awaiting-ci`, and records no short re-check cooldown (unlike the review and comment gates, which do). The item then waits for the generic periodic re-evaluation, which is ten poll intervals — 30 minutes at `poll: 180`. During that time nothing else readmits the item: no awaiting-ci label, no webhook, and CI completion does not move the issue's `updatedAt`. Observed on concept-maps #1059 on 2026-09-28: CI went green but the item sat idle until the periodic interval lapsed.

**Too eager (#2062).** The backstop added in #1944 re-fetches a freshly paused item from GitHub to catch a missed human reply. In practice it fetches on every poll for up to 20 polls after the pause, for two reasons:
1. Cooldown entries are never removed, so after any cooldown has expired the item is treated as having an expired cooldown on every later poll, and is admitted every poll.
2. After a fetch, the item's baseline is re-anchored to GitHub's last-updated time, which for a fresh pause is the pause write itself, so the "recent baseline" window never closes early.

**Why together.** Adding a short cooldown for merge-gate claims (the natural fix for #1943) would, with the never-cleared defect still present, cause the item to be admitted on every poll indefinitely once that cooldown first expired. Fixing #1943 alone would therefore trade a slow re-check for a permanent hot loop; the cooldown-expiry semantics must be fixed in the same change.

Stale check-run cache was ruled out as a cause in #1943: the mergeable-state read is live, and a `clean` state short-circuits the gate.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Merge gate re-checks a pending-CI item on the next poll (Priority: P1)

An operator runs a yolo item that has finished Validate. Its PR's CI is still running, so the merge gate holds it. When CI turns green — with no label change and no change to the issue's last-updated time — the operator expects the item to land within a poll or two, not after the 30-minute periodic interval.

**Why this priority**: This is the user-visible incident (#1943): a finished, green item stalling for up to half an hour with no signal.

**Independent Test**: In the sim, seed a Validate-complete yolo item with pending CI that the merge gate holds; flip CI to green without touching labels or `updatedAt`; poll. The gate must clear within one or two polls. The scenario must fail on current `main`.

**Acceptance Scenarios**:

1. **Given** a Validate-complete yolo item whose PR's merge state is unsettled (CI pending) and which the merge gate claims, **When** the next poll runs, **Then** the item is re-evaluated by the merge gate on that poll.
2. **Given** the same item and CI has since turned green with no label or `updatedAt` change, **When** one or two polls elapse, **Then** the merge gate clears and the item proceeds.
3. **Given** a merge-gate claim because the PR is queued for landing (the `PRMergeQueued` outcome), **When** the next poll runs, **Then** the item is likewise re-evaluated on that poll.

---

### User Story 2 - An expired cooldown admits an item once, then stops (Priority: P1)

An item has had some cooldown (for any reason) that has since expired. The operator expects the item to be admitted for a live look once on the strength of that expiry, and thereafter to be treated like any other idle item — not re-admitted every poll forever.

**Why this priority**: This is the shared root cause. Without it, fixing Story 1 creates a permanent every-poll admission, and #2062's excess GitHub calls persist.

**Independent Test**: With a unit or sim test, give an item a cooldown that expires, run polls, and assert the item is admitted on the first poll after expiry and not admitted on later polls on that cooldown's strength alone.

**Acceptance Scenarios**:

1. **Given** an item whose cooldown has just expired, **When** the next poll runs, **Then** the item is admitted.
2. **Given** that item was admitted on the strength of the expired cooldown, **When** subsequent polls run with no other admission reason, **Then** the item is not admitted on that cooldown's strength.
3. **Given** a new cooldown is later recorded and expires, **When** the next poll runs, **Then** the item is admitted once again for that new expiry.
4. **Given** an item with no other admission reason, **When** the periodic re-check interval elapses, **Then** the item is admitted by the periodic re-check (see User Story 4).

---

### User Story 3 - The paused-item backstop fetches at most once per baseline (Priority: P2)

After an item is paused, Fabrik makes a bounded safety-net live fetch to catch a human reply it may have missed. The operator expects this to cost at most one GitHub fetch for a given baseline value, not one per poll for 20 polls.

**Why this priority**: It removes wasteful GitHub API spend (#2062) but causes no functional failure, so it ranks below the stall fix.

**Independent Test**: Unit test: pause an item and run 20 polls with an unchanged baseline; count backstop deep fetches. At most one per distinct baseline value.

**Acceptance Scenarios**:

1. **Given** an item paused moments ago with a recent baseline, **When** the first poll runs, **Then** the backstop may perform one live fetch.
2. **Given** that fetch has happened and the baseline value is unchanged, **When** later polls run, **Then** no further backstop fetch occurs for that baseline.
3. **Given** the baseline later changes to a new distinct value, **When** a poll runs, **Then** the backstop may perform one more fetch for that new value.
4. **Given** across 20 polls the baseline never changes, **Then** the total backstop fetches for the item is at most one.

---

### User Story 4 - Periodic re-check still happens (Priority: P2)

An operator has idle items that no gate, webhook or label currently admits. They expect these to keep being re-evaluated at the periodic interval, as today.

**Why this priority**: Guards against the cooldown fix over-correcting and silencing the periodic safety net that catches silent drift.

**Independent Test**: With an item not otherwise admitted, advance the clock by the periodic interval and assert it is admitted; confirm again after the next interval.

**Acceptance Scenarios**:

1. **Given** an idle item with no other admission reason, **When** the periodic interval elapses, **Then** the item is admitted.
2. **Given** that periodic admission occurred, **When** another full interval elapses, **Then** the item is admitted again.

---

### Edge Cases

- The merge gate claims an item on consecutive polls (CI stays pending for a long time): each poll re-evaluates it, but a pending CI must still be bounded by the existing CI-timeout machinery; the short cooldown must not create unbounded GitHub calls beyond one re-evaluation per poll.
- A merge-gate claim that is not transient (a gate outcome other than unsettled/queued) must not receive the short cooldown.
- An item carrying several cooldowns with different reasons, some expired and some not: only expiries not yet "used" for admission may admit it, and one reason's use must not consume another reason's expiry.
- A cooldown that expires while the item is already admitted for another reason on the same poll: the expiry is still treated as used, so it does not cause a second admission on the next poll.
- Restart of the daemon: cooldown state is in memory, so after a restart behavior is as for a never-cooled-down item; no persisted state needs migration.
- An existing gate that currently relies on an expired cooldown continuing to admit the item (the sticky behavior) must keep working under the new semantics; the audit in FR-005 identifies any such reliance.
- A paused item whose baseline changes during the 20-poll window (for example a new human comment): the backstop may fetch again for that new baseline.
- A paused item whose backstop fetch fails: a failed fetch must not be recorded as "done for this baseline" in a way that suppresses the next legitimate attempt. [Assumption recorded below.]

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the merge gate claims an item with an unsettled (`PRMergeUnsettled`) or queued (`PRMergeQueued`) outcome, the item MUST be re-evaluated on the next poll rather than waiting for the periodic re-evaluation interval.
- **FR-002**: FR-001 MUST be achieved with the existing per-item cooldown mechanism (a short cooldown with its own reason, expiring after roughly one poll interval, as `review-blocked` and `comment-pending` already do), not a new admission mechanism.
- **FR-003**: Merge-gate claims other than unsettled or queued MUST NOT receive the short cooldown.
- **FR-004**: An expired cooldown MUST admit the item once and then MUST NOT count toward admission again until a new cooldown is recorded and expires. This may be achieved either by clearing/consuming the entry when the item is admitted on its strength, or by treating only entries that expired since the last admission as admitting; the Plan stage chooses one.
- **FR-005**: The Plan stage MUST audit every cooldown reason and every reader of the cooldown state, and confirm that no current gate depends on an expired cooldown continuing to admit indefinitely. Any gate that does MUST be adjusted so its behavior is preserved or deliberately and documentedly changed.
- **FR-006**: The paused-item backstop (#1944) MUST perform a live fetch at most once per distinct baseline value per item, and MUST NOT repeat it while that baseline is unchanged.
- **FR-007**: The code comment that describes the backstop as costing "at most one or two extra fetches" MUST be corrected to describe the new behavior accurately.
- **FR-008**: Items not otherwise admitted MUST continue to be re-evaluated at the periodic interval (no regression of the periodic re-check).
- **FR-009**: `docs/state-machine.md` (the admission description, §7) MUST be updated to describe the merge-gate short cooldown, the consumed-once cooldown-expiry semantics, and the per-baseline paused backstop, and `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-010**: The change MUST include a sim scenario that fails on current `main`: a Validate-complete yolo item with pending CI held by the merge gate clears within one or two polls after CI turns green with no label or `updatedAt` change.
- **FR-011**: The change MUST include a sim or unit test that, after a cooldown expires and the item is admitted once, subsequent polls do not admit the item on that cooldown's strength.
- **FR-012**: The change MUST include a unit test that a paused item triggers at most one backstop deep fetch per baseline value across 20 polls.
- **FR-013**: Each new test MUST be demonstrated to be non-vacuous by showing it fails when its corresponding fix is neutralised.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A yolo item held by the merge gate for pending CI lands within two polls of CI turning green (versus up to ten poll intervals — 30 minutes at `poll: 180` — before the change).
- **SC-002**: After an item's cooldown expires, the number of polls in which that expiry alone admits the item is exactly one.
- **SC-003**: Across 20 polls following a pause with an unchanged baseline, the paused-item backstop makes at most one live GitHub fetch for that item (versus up to 20 before).
- **SC-004**: An item with no other admission reason is still admitted by the periodic re-check once per periodic interval.
- **SC-005**: All three new acceptance tests fail when their respective fixes are removed and pass with them in place.
- **SC-006**: The canonical docs describe the new admission behavior and the bundled `docs/llms-full.txt` matches a fresh regeneration (docs-drift check passes).

## Assumptions

- The cooldown state is in-memory only and resets on daemon restart; no persistence or migration is needed.
- The short merge-gate cooldown is one poll interval, matching the existing `review-blocked` and `comment-pending` cooldowns; the exact reason name (e.g. `merge-unsettled`) is a Plan-stage choice.
- A failed backstop fetch should not count as having served the baseline, so the next poll may retry once; this is the low-impact, reversible default.
- The existing CI-timeout backstop continues to bound how long an item can sit in a pending-CI merge-gate hold; this change only affects how soon the gate is re-checked.
- "Admitted" means selected for a live deep fetch and re-evaluation on a poll, as described in the Background.
- This issue is blocked on #2094 per the engine's dependency check; its content does not otherwise depend on #2094.

## Out of Scope *(optional)*

- Giving `mergeable=null` its own escalation bound, or naming the blocker in the CI-timeout pause (#1809).
- Applying `fabrik:awaiting-ci` on merge-gate claims.
- Changing the length of the periodic re-evaluation interval itself.
- Changing the cooldown mechanism for gates other than as needed to preserve their behavior under FR-004.
- PR body wording: the PR mentions #1943 and #2062 by bare number only, with no closing keyword near either; only this issue's own number gets `Closes`.

## Source References *(optional)*

- #1943 — merge-gate claim stalls until the periodic re-eval (concept-maps #1059, 2026-09-28)
- #2062 — paused-item backstop fetches on every poll
- #1944 — original paused-item backstop
- #1809 — related, out of scope
- `docs/state-machine.md` — admission / §7
