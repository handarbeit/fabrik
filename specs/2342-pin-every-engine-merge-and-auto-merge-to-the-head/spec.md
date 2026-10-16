# Feature Specification: Pin every engine merge and auto-merge to the head the gates judged

**Feature Branch**: `fabrik/issue-2342`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "## Problem

The ordinary landing merges a PR head nobody judged. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.2.

The CI gate reads the head from the cached `FetchLinkedPR` (up to `linkedPRCacheTTL` = 45 s old, boardcache/boardcache.go:197). `attemptMergeOnValidate` then re-reads the item live, and `landValidated` (engine/stages.go:469) calls `EnablePullRequestAutoMerge` or `MergePR` with no expected head. `MergePRAtHeadSHA` exists but only the merge train's singleton fast path uses it (engine/merge_train.go:2337). `settleSHAInvalidationScan` runs after the landing in the same poll, so it cannot stop it.

Scenario: a push lands between the gate and the merge on a repo with no required checks. Enabling auto-merge fails on a clean state, the code falls back to `MergePR`, and an unchecked commit merges.

## Requirements

- **R1:** Every engine merge outside the train (`MergePR` fallback, `EnablePullRequestAutoMerge`, the merge-queue enqueue) is pinned to the head SHA the CI and review gates evaluated in this decision (`expectedHeadOid` / `MergePRAtHeadSHA`).
- **R2:** That head SHA is taken once per landing decision and passed through; the gates and the merge never read it separately.
- **R3:** A head mismatch is not a failure: the landing is deferred, nothing is labelled or commented, and the next poll evaluates the new head.
- **R4:** The train's batch and integration landings are audited for the same gap and pinned where they are not.

## Acceptance

- Unit tests: a head that moves between the gate and the merge is not merged, for the auto-merge, direct-merge and enqueue paths.
- A sim twin: a push after the gate clears defers the landing, and the new head lands after its own CI.
- The live e2e suite gains a case for a push between gate and merge if the sim cannot model GitHub's refusal; otherwise record why not."

## Background

Fabrik only merges a PR after its gates (CI, reviews, feedback) have judged it clean. Today the gates and the merge do not agree on *which commit* was judged. The CI gate works from a cached read of the linked PR that can be up to 45 seconds old. The landing decision then re-reads the item live, and the ordinary landing (auto-merge enablement, the direct-merge fallback and the merge-queue enqueue) asks GitHub to merge "whatever the head is now", with no statement of the head that was judged. Only the merge train's singleton fast path already pins its merge to a validated head. The post-landing SHA-invalidation scan runs after the landing in the same poll, so it cannot stop a merge that has already happened.

The failure scenario: a commit is pushed to the PR branch in the window between the gate clearing and the merge request, on a repository with no required checks. Enabling auto-merge is refused because the state is already clean, the engine falls back to a direct merge, and a commit that no gate ever evaluated is merged into the base branch. The same exposure exists wherever a branch writable by someone other than the engine is merged without naming the commit that was validated.

The fix is to decide the head once per landing decision, evaluate the gates against that head, and bind the merge to that same head so GitHub itself refuses a merge of anything else. A refusal for that reason is expected, benign and self-correcting: the next poll sees the new head and judges it on its own merits.

Found by the engine state audit of 2026-10-15, §1.2.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A push between the gate and the merge is never merged unjudged (Priority: P1)

An operator running Fabrik on a repository with no required status checks has an item whose Validate stage and gates have cleared. Before the engine's merge request reaches GitHub, a contributor pushes another commit to the PR branch. The engine must not merge the new commit; it must wait and judge it.

**Why this priority**: This is the defect itself: an unchecked commit reaching the base branch. Everything else in this issue supports or verifies it.

**Independent Test**: With a landing candidate whose gates are green at head A, move the PR head to B after the gate evaluates and before the merge request is issued, for each of the three ordinary landing paths. Verify B is not merged on any of them.

**Acceptance Scenarios**:

1. **Given** the gates cleared at head A and auto-merge is the chosen path, **When** the head moves to B before auto-merge is enabled, **Then** no auto-merge is enabled on B and nothing merges.
2. **Given** the gates cleared at head A and auto-merge enablement is refused because the PR is already clean, **When** the engine falls back to a direct merge and the head is now B, **Then** the direct merge is refused and B is not merged.
3. **Given** the gates cleared at head A and the repository uses a merge queue, **When** the head moves to B before the enqueue, **Then** B is not enqueued.
4. **Given** the head did not move, **When** the engine lands the item, **Then** it merges, auto-merges or enqueues exactly as it does today.

---

### User Story 2 - A head mismatch defers quietly and the new head lands on its own merits (Priority: P1)

When the engine's merge is refused because the head moved, the operator sees no failure: no label, no comment, no pause, no retry-budget spend. The next poll evaluates the new head through the normal gates and lands it only once it has passed them.

**Why this priority**: Treating the mismatch as a failure would trade an unsafe merge for noisy or stuck items. The deferral behaviour is half of the correctness of the fix.

**Independent Test**: In the sim, push a commit after the gate clears; verify the landing is deferred with no labels or comments added, then that after the new head's CI completes the new head lands.

**Acceptance Scenarios**:

1. **Given** a landing deferred because the head moved, **When** the engine finishes that poll, **Then** the item carries no new label, has no new comment, is not paused, and no retry or cycle counter has been incremented.
2. **Given** that deferral, **When** the next poll runs and the new head's CI is pending, **Then** the item waits on CI as for any new push.
3. **Given** that deferral, **When** the new head's CI and other gates pass, **Then** the new head is merged, pinned to the new head.

---

### User Story 3 - One head per landing decision, shared by gates and merge (Priority: P1)

For one landing decision the engine determines the PR head once, evaluates the CI and review gates against that value, and passes the identical value to the merge. The gates and the merge never obtain the head from separate reads.

**Why this priority**: Pinning the merge to a head that was read separately from the gates' head still leaves a window. Single-sourcing the value is what closes it.

**Independent Test**: Instrument the landing path so successive reads would return different heads; verify the head used for gate evaluation and for the merge request is the same value, and that the decision makes no separate head read for the merge.

**Acceptance Scenarios**:

1. **Given** a landing decision, **When** the gates are evaluated, **Then** they are evaluated against one head value captured for that decision.
2. **Given** that decision proceeds to merge, **When** the merge is requested, **Then** it names that same captured head and no other.

---

### User Story 4 - Merge-train landings carry the same guarantee (Priority: P2)

The merge train's batch and integration-PR landings are examined for the same unpinned-merge gap. Any that merge without naming the validated head are pinned. Paths already pinned (the singleton fast path) are unchanged.

**Why this priority**: Same defect class, smaller exposure (the train assembles and validates its own trial branch), but the issue asks for an explicit audit, and an unaudited path would leave the guarantee incomplete.

**Independent Test**: For each train landing path, verify the merge request names the head that train CI validated, and that a moved head refuses the merge and leaves members queued for re-evaluation.

**Acceptance Scenarios**:

1. **Given** a train batch or integration PR validated at head A, **When** the head moves to B before the merge, **Then** B is not merged and the members are not marked landed.
2. **Given** the singleton fast path, **When** it lands, **Then** behaviour is unchanged.

---

### Edge Cases

- The head moves *after* the merge request is accepted by GitHub: out of the engine's control; the guarantee is that the engine never asks to merge a head other than the one it judged.
- The head moves twice (A → B → C) between gate and merge: still a single deferral; the next poll judges C.
- The head moves and then moves back to A: the pinned request succeeds and merges A, which is the judged commit.
- Auto-merge is already enabled on the PR and the head then moves: GitHub's own auto-merge handling and the post-landing SHA-invalidation scan keep their existing behaviour; this issue concerns only the engine's own requests.
- GitHub refuses the merge for a different reason (conflicts, required checks, branch protection): existing handling applies unchanged and is distinguishable from a head mismatch.
- The head cannot be determined for the decision (read error): the landing is not attempted on a guessed head; existing fail-closed behaviour for unreadable gate inputs applies.
- A head mismatch occurs repeatedly because someone keeps pushing: each poll defers and re-evaluates; no failure is accumulated and no cap is consumed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every engine merge of a linked PR outside the merge train — the direct-merge call, including the fallback taken when auto-merge enablement is refused — is bound to the head SHA the CI and review gates evaluated for that landing decision, so GitHub refuses it if the PR head differs.
- **FR-002**: The engine's request to enable auto-merge on a PR is bound to the same judged head SHA.
- **FR-003**: The engine's enqueue of a PR into a merge queue is bound to the same judged head SHA.
- **FR-004**: The head SHA for a landing decision is determined once per decision; the CI gate, the review gate, the feedback gate and the merge all use that single value, and the merge step performs no separate read of the head.
- **FR-005**: A refusal caused by the PR head differing from the pinned head is classified as a deferral, not a failure: no label is added or removed, no comment is posted, the item is not paused, and no retry, cycle or ejection counter is incremented.
- **FR-006**: After such a deferral the item is re-evaluated on a subsequent poll against its current head, and lands only once that head has passed the gates.
- **FR-007**: A head-mismatch refusal is distinguishable from every other merge refusal (not mergeable, required checks pending, protection rules, permission errors); all other refusals keep their existing handling unchanged.
- **FR-008**: When the head to pin cannot be determined for a decision, the engine does not issue an unpinned merge, auto-merge enablement or enqueue; it defers using the existing fail-closed handling for unreadable gate inputs.
- **FR-009**: The merge train's batch landing and integration-PR landing are audited for unpinned merges; every such merge that does not already name the validated head is bound to the head train CI validated, with a mismatch deferring and leaving members queued, without being counted as a train failure.
- **FR-010**: The merge train's singleton fast path, which is already pinned, is unchanged in behaviour.
- **FR-011**: Behaviour when the head has not moved is unchanged from today for all paths: same merge method, same ordering of side effects, same labels and comments.
- **FR-012**: Automated tests cover, for the auto-merge, direct-merge and enqueue paths, that a head moved between the gate and the merge is not merged, and that the resulting deferral is silent (FR-005).
- **FR-013**: A sim-level scenario demonstrates that a push after the gate clears defers the landing, and that the new head lands after its own CI passes.
- **FR-014**: The live end-to-end suite gains a case that pushes between the gate and the merge, unless the sim demonstrably models GitHub's head-mismatch refusal faithfully; in that case the reason the live case is unnecessary is recorded with the change.

### Key Entities *(if applicable)*

- **Landing decision**: One evaluation, on one poll, of whether an item's PR may be merged now; it owns the single captured head SHA.
- **Judged head**: The PR head commit the CI, review and feedback gates evaluated in a landing decision; the only commit the engine may ask GitHub to merge in that decision.
- **Head-mismatch deferral**: The outcome of a merge refused because the PR head is no longer the judged head; silent and self-correcting.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In tests covering auto-merge, direct merge, merge-queue enqueue and the train's batch/integration landings, a head that changes between the gate and the merge request results in zero merges of the new head (100% of cases).
- **SC-002**: In every head-mismatch case, zero labels, zero comments, zero pauses and zero counter increments are produced by the deferral.
- **SC-003**: In the sim scenario, the new head is merged only after its own CI passes, within the normal number of polls for a fresh push.
- **SC-004**: With an unmoved head, the sequence of GitHub writes for a landing is identical to the pre-change sequence apart from the added head binding.
- **SC-005**: No engine merge, auto-merge enablement or enqueue exists anywhere outside tests that is not bound to a judged head, with any intentional exception explicitly recorded with a reason.

## Assumptions

- GitHub's merge, auto-merge-enable and merge-queue-enqueue operations each support a mechanism to state the expected head commit, and refuse with a distinguishable error when it differs; the exact capability of each is to be confirmed in Research. If any of the three cannot be pinned by GitHub, Research must propose the closest equivalent guarantee and flag it.
- The existing pinned merge used by the train's singleton fast path is the reference behaviour for what "pinned" means.
- A silent deferral is safe because the existing per-poll evaluation re-runs the gates on the new head without further engine state.
- The post-landing SHA-invalidation scan stays as a backstop and is not changed by this issue.
- The sim can be extended to refuse a merge whose expected head differs from the PR head; if its fidelity falls short, FR-014 requires a live e2e case instead.

## Out of Scope *(optional)*

- Changing how the CI, review or feedback gates themselves decide (only which head they are given and that the merge uses it).
- Reducing the cache age of the linked-PR read used elsewhere.
- Changes to the post-landing SHA-invalidation scan.
- Pinning merges performed by humans or by GitHub itself (for example auto-merge already enabled before a push).
- New configuration, flags or labels.

## Source References *(optional)*

- Engine state audit, 2026-10-15, §1.2 (`docs/design/engine-state-audit-2026-10.md`, as cited by the issue; not present in this checkout).
- Issue-cited code: `landValidated` and `attemptMergeOnValidate` in the engine's stage/merge-gate code; the merge train's singleton fast path as the existing pinned example.
