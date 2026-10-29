# Feature Specification: Webhook deliveries for a merge-train integration PR must not attach to a single train member

**Feature Branch**: `fabrik/issue-2483`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

With webhooks on, a `pull_request` or `check_run` delivery for a **merge-train integration PR** goes through the board cache's PR auto-heal path (`boardcache/delta.go`, `resolveOrHealPRLinkage` and its callers). The integration PR carries `Closes` for every batch member. The heal resolves it to **one** member, the first closing issue. It then:

- applies `PRDetailsUpdated` and `PRHeadSHAUpdated` to that member with `LinkedPRNum` set to the integration PR, so the member's cached linked PR and head SHA become the integration PR's (its own PR is a different one);
- applies `DeepFetchInvalidated`, so the member is \"not yet hydrated\" and the next batch formation excludes it.

### Seen on the 0.0.90 gate

Engine `d6866128`, bed A, `app/on/webhook`, invocation `20261026T045615Z-e31de8`. `TestMergeTrainQueuedDeeperThanBatchCap` failed its A2 check: 3 batch snapshots before the batch-1 merge, want exactly 1. The bed log shows:

```
[merge-train] opened draft CI PR #10036 for handarbeit/fabrik-test-alpha (5 survivor(s))
[cache] auto-heal: PR #10036 → issue #10022; deep cache invalidated and delta re-applied
[cache] auto-heal: check_run SHA e155df06… → PR #10036 → issue #10022; deep cache invalidated
[#10022 merge-train] cache entry for #10022 not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry
[merge-train] batch snapshot for handarbeit/fabrik-test-alpha: 5 item(s) — #10024 … (without #10022)
```

The landing still completed, with all 5 members merged and closed. The test passed on the same cell at `624e0e56`, so how often this shows up depends on timing. It is reachable only with a webhook transport (direct, tailscale, Hookdeck); poll-only daemons never take this path.

## Requirements

1. **Don't attach an integration PR to a member.** A delivery whose PR is a merge-train integration or trial PR (head branch `fabrik/merge-train/…`) must not overwrite any member's linked-PR facts or invalidate a member's deep cache through the auto-heal. Either skip the heal for those PRs, or route their facts to the train's own state if anything needs them. The decision should not depend on which member is named first.
2. **More generally:** when a PR closes several cached issues, the heal must not attach it to one of them arbitrarily. Decide in Research whether it should refuse or apply to none.
3. **Sim twin:** the sim (`tests/sim`, with webhook deltas driven through the board cache) reproduces it. An integration-PR `pull_request` delivery during an open trial must leave every member's `LinkedPR*` and hydration unchanged, and the batch snapshot must not change composition. The twin must fail with the fix neutralised.

## Acceptance

- The sim twin passes with the fix and fails without it.
- `TestMergeTrainQueuedDeeperThanBatchCap` passes on `app/on/webhook`.

Mentioned for context: #2051, #2348."

## Background

With a webhook transport active (direct, tailscale or Hookdeck), the board cache applies `pull_request` and `check_run` deliveries to the item they belong to. When a delivery's PR is not yet linked to a cached item, an auto-heal path resolves the PR to an issue from its closing references and applies the PR's facts to that issue.

A merge-train integration (or trial) PR is not any one member's PR: it carries `Closes` for every member of the batch. The heal picks the first closing issue and treats the integration PR as that member's linked PR. Two harmful effects follow for that member: its cached linked-PR number and head SHA are replaced by the integration PR's (its real PR is a different one), and its deep cache is invalidated, so the member reads as "not yet hydrated" and the next batch formation drops it from the batch.

This was observed on the 0.0.90 release gate (engine `d6866128`, bed A, `app/on/webhook`): `TestMergeTrainQueuedDeeperThanBatchCap` failed its A2 check with 3 batch snapshots where exactly 1 was expected, because member #10022 was excluded after the heal attached trial PR #10036 to it. The landing itself still completed (all 5 members merged and closed), and the same test passed on the same cell at `624e0e56`, so the failure is timing-dependent. Poll-only daemons never take this path.

Related context: #2051 (persisted merge-train run), #2348 (observed-at ordering of Store writes).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Integration PR deliveries leave train members untouched (Priority: P1)

An operator runs the merge train with webhooks enabled. While the train has an open trial or integration PR for a batch, GitHub delivers `pull_request` and `check_run` events for that PR. The daemon must keep every member's own linked-PR facts and hydration state exactly as they were, so batch composition is not disturbed.

**Why this priority**: This is the observed defect. It causes spurious batch re-formation and a failed release-gate test on webhook cells.

**Independent Test**: In the sim with webhook deltas driven through the board cache, open a trial for a batch of several members, deliver `pull_request` (and `check_run`) events for the trial PR, and assert that no member's `LinkedPR*` fields or deep-fetch hydration changed and that the next batch snapshot has the same composition.

**Acceptance Scenarios**:

1. **Given** a train with an open trial PR whose head branch is `fabrik/merge-train/…` and which closes members A, B and C, **When** a `pull_request` delivery for that PR arrives, **Then** none of A, B or C has its linked PR, head SHA or hydration changed.
2. **Given** the same trial, **When** a `check_run` delivery whose SHA belongs to the trial PR arrives, **Then** no member's deep cache is invalidated and no member is excluded from batching as "not yet hydrated".
3. **Given** the same trial, **When** the order in which the PR names its closing issues is changed, **Then** the outcome is identical (no member is affected, whichever is named first).
4. **Given** the batch-cap scenario of `TestMergeTrainQueuedDeeperThanBatchCap` on `app/on/webhook`, **When** the batch-1 trial is open and its deliveries arrive, **Then** exactly one batch snapshot occurs before the batch-1 merge.

---

### User Story 2 - A PR closing several cached issues is never attached to one arbitrarily (Priority: P2)

For any PR (not only train PRs) that closes more than one cached issue, the auto-heal must not pick one of those issues by position and attach the PR to it.

**Why this priority**: It is the general form of the same defect and guards against non-train variants (for example a manually opened PR with several `Closes` lines), but the observed failure is covered by Story 1.

**Independent Test**: Deliver a `pull_request` event for a non-train PR that closes two cached issues, neither of which is linked to it yet, and assert the outcome does not depend on closing-reference order and attaches the PR to neither issue arbitrarily.

**Acceptance Scenarios**:

1. **Given** a PR that closes cached issues X and Y and is linked to neither, **When** a delivery for that PR arrives, **Then** the heal does not attach it to whichever of X or Y is named first.
2. **Given** a PR that closes exactly one cached issue, **When** a delivery arrives, **Then** the heal behaves as it does today.

---

### Edge Cases

- A delivery for a train PR arrives before the train has recorded the PR in its own state: the head-branch rule (`fabrik/merge-train/…`) must still recognise it.
- A `check_run` delivery resolves to the trial PR only through its head SHA; the trial must still be excluded.
- A train PR that is also legitimately linked to a member (singleton landing PR or a member's own PR) — only integration and trial PRs are in scope; a member's own `fabrik/issue-N` PR keeps healing as today.
- A delivery that arrives while a different poll is mid-flight must not leave partial member state behind.
- Redelivered or replayed copies of the same delivery must produce the same (no-op) result.
- A PR that closes several cached issues where one of them is already linked to it: behaviour for the already-linked item is unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A webhook delivery (`pull_request`, `check_run`, and any other delivery that reaches the PR auto-heal) whose PR is a merge-train integration or trial PR (head branch `fabrik/merge-train/…`) MUST NOT change any member's cached linked-PR number, head SHA or PR details.
- **FR-002**: The same deliveries MUST NOT invalidate any member's deep cache, so no member becomes "not yet hydrated" and is excluded from batch formation because of them.
- **FR-003**: The handling of such a delivery MUST be independent of the order in which the PR's closing issues are listed or of which member is named first.
- **FR-004**: If any state needs the integration/trial PR's facts, they MUST go to the merge train's own state, never to a member's item state; if nothing needs them, they may be ignored.
- **FR-005**: When a PR closes more than one cached issue, the auto-heal MUST NOT attach it to one of them by position or other arbitrary choice. Whether it refuses outright or applies to none is decided in Research; either way no single issue is chosen arbitrarily.
- **FR-006**: A PR that closes exactly one cached issue, and a member's own `fabrik/issue-N` PR, MUST continue to heal as today.
- **FR-007**: A sim-bed twin MUST drive webhook deltas through the board cache during an open trial, assert unchanged member `LinkedPR*` and hydration and unchanged batch-snapshot composition, and MUST fail when the fix is neutralised (a per-engine test seam, following the repository's existing neutralisation-seam convention).
- **FR-008**: The fix MUST not alter poll-only behaviour; it applies only to the webhook delta path.

### Key Entities *(if applicable)*

- **Merge-train integration/trial PR**: a PR on a head branch `fabrik/merge-train/…`, opened by the engine for a batch; closes every member of the batch and belongs to no single member.
- **Train member**: a Queued item whose own linked PR is its `fabrik/issue-N` PR; its cached linked-PR facts and hydration state feed batch formation.
- **PR auto-heal**: the board cache path that resolves a delivery's PR to an issue via closing references when the PR is not yet linked, and applies PR facts and a deep-cache invalidation to it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: During an open trial with webhooks on, zero member items have their linked-PR number, head SHA or hydration state changed by deliveries for the trial/integration PR.
- **SC-002**: `TestMergeTrainQueuedDeeperThanBatchCap` passes on `app/on/webhook` (exactly 1 batch snapshot before the batch-1 merge).
- **SC-003**: The new sim twin passes with the fix and fails with the fix neutralised.
- **SC-004**: No existing test for the single-closing-issue heal path or for poll-only operation changes outcome.

## Assumptions

- A merge-train integration or trial PR is reliably identified by its head branch prefix `fabrik/merge-train/`, as stated in the issue; no other PRs use that prefix.
- Whether the general multi-closing-issue case should refuse the heal or apply to none is a Research-stage decision; both satisfy FR-005 and are equivalent from the user's point of view.
- Where train PR facts are needed at all is determined in Research; "ignore" is acceptable if nothing consumes them.
- The reconcile and listing paths already carry the correct member facts, so skipping the heal for train PRs loses nothing a member needs.

## Out of Scope *(optional)*

- Changes to merge-train batch formation, admission or landing logic beyond what is needed to stop the heal from disturbing members.
- Poll-only daemons (they do not take this path).
- Transport-specific changes to the direct, tailscale or Hookdeck receivers.
- Reworking how webhook deliveries are ordered or deduplicated (#2348, intake).

## Source References *(optional)*

- 0.0.90 gate failure: engine `d6866128`, invocation `20261026T045615Z-e31de8`, `TestMergeTrainQueuedDeeperThanBatchCap` A2 check.
- `boardcache/delta.go` (`resolveOrHealPRLinkage` and callers)
- Related: #2051, #2348
