# Feature Specification: Restore the Validate completion SHA after a restart

**Feature Branch**: `fabrik/issue-2343`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "## Problem

The Validate completion SHA is lost on restart. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.3.

`ValidateCompletedSHA` is written at engine/ci.go:453, engine/stages.go:226, engine/no_work_needed_settle.go:103 and engine/poll_settle.go:151, and held only in the in-memory Store. Nothing rebuilds it at startup. `settleSHAInvalidationScan` skips an item whose value is empty (engine/poll_settle.go:116, \"FR-5\").

Scenario: an item sits at `stage:Validate:complete` (waiting on CI, reviews, or cruise). The daemon restarts (SIGHUP, self-upgrade, crash). Someone pushes to the PR. Validate is never invalidated; under yolo the PR lands on gates that do not re-run Validate.

## Requirements

- **R1:** After a restart, an item carrying `stage:Validate:complete` has a completion SHA again before `settleSHAInvalidationScan` evaluates it, or that scan treats a missing SHA conservatively.
- **R2:** Choose and record in an ADR one of: persist the SHA (beside the session meta or in `.fabrik/state`), or rebuild it from durable GitHub evidence (the Validate stage comment's recorded commit, which Fabrik already writes as `commit: <sha>` in the stage header). A rebuild must never pick a head newer than the one Validate ran on.
- **R3:** If neither source yields a SHA, the item is not landed on an unverified head: the conservative choice (re-run Validate, or hold the landing with one log line) is stated in the ADR.
- **R4:** The merge-train catch-up rule (#2158) keeps working after a restart.

## Acceptance

- A unit test that restarts an engine (new Store) over an item at Validate-complete, pushes a new head, and shows Validate is invalidated.
- A sim twin using `RestartEnv`.
- Non-vacuity: with the rebuild disabled the twin fails."

## Background

When Validate completes, the engine records the linked PR's head SHA at that moment (the "Validate completion SHA"). A per-poll scan compares it with the PR's current head: if someone pushes afterwards, the completion is stale and Validate is invalidated and re-run. This is what stops a PR from being merged on code Validate never saw.

The recorded SHA lives only in the engine's in-memory item store. After a daemon restart (SIGHUP restart, self-upgrade, crash) the value is empty. The scan deliberately skips items with no recorded SHA, a rule written for items that pre-date the feature. So after a restart an item that already carries `stage:Validate:complete` is no longer watched for new pushes.

Failure scenario: an item sits at `stage:Validate:complete` while waiting on CI, reviews or cruise. The daemon restarts. Someone then pushes to the PR. Validate is not invalidated. Under yolo the PR can land on gates that do not re-run Validate, so unvalidated code reaches the base branch. The gap was found by the engine state audit of 2026-10-15 (§1.3).

Related existing behavior that must keep working: the merge-train catch-up exemption (#2158, ADR-2158). The scan leaves landed or terminal items alone and does not treat the engine's own pure catch-up merge as an external push. It re-records the new head as the completion SHA, and that re-recorded value is also in memory only.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A push after a restart still invalidates Validate (Priority: P1)

An operator restarts the daemon while an item sits at `stage:Validate:complete`. After the restart someone pushes a new commit to the PR. The engine notices that the head differs from the head Validate ran on, removes the Validate completion and re-runs Validate.

**Why this priority**: This is the reported defect. Without it, unvalidated code can land under yolo or cruise.

**Independent Test**: Build an engine with an item at Validate-complete and a known completion SHA. Replace the engine and its store with a fresh one over the same GitHub state. Push a new head to the PR and run the SHA-invalidation scan. Validate is invalidated.

**Acceptance Scenarios**:

1. **Given** an item at `stage:Validate:complete` whose Validate ran on head A, **When** the daemon restarts and the PR head later becomes B, **Then** `stage:Validate:complete` is removed and Validate is re-dispatched.
2. **Given** the same item after a restart and the PR head is still A, **When** the scan runs, **Then** nothing changes and the item is not invalidated.
3. **Given** an item that was never pushed to after Validate, **When** the daemon restarts repeatedly, **Then** the item is never invalidated spuriously.

---

### User Story 2 - A rebuilt SHA is never newer than the Validate-time head (Priority: P1)

When the completion SHA is restored from durable evidence, it identifies the head Validate actually ran on, or something older. It is never the PR's current head if that head was pushed after Validate.

**Why this priority**: Restoring the value by simply reading the PR's current head would mark an unvalidated push as validated. That would hide exactly the push this issue is about.

**Independent Test**: Run Validate on head A and let it complete. Push B while the daemon is down, then start the daemon. The restored completion SHA is A (or absent), never B, so the scan invalidates Validate.

**Acceptance Scenarios**:

1. **Given** Validate completed on head A and the PR head moved to B while the daemon was down, **When** the daemon starts and the scan runs, **Then** the completion SHA is not B and Validate is invalidated.
2. **Given** durable evidence that names head A, **When** the SHA is restored, **Then** it equals A.

---

### User Story 3 - No recoverable SHA means no landing on an unverified head (Priority: P2)

If a completion SHA cannot be restored from any source, the engine does not silently treat the item as unwatched. It either re-runs Validate or holds the landing with one log line. The chosen behavior is recorded in an ADR.

**Why this priority**: Closes the remaining hole where evidence is missing or unreadable. It is rarer than User Stories 1 and 2 but must fail safe.

**Independent Test**: Seed an item at Validate-complete with no recoverable SHA, then run the scan and the landing path. The item is not landed on an unverified head and the chosen conservative outcome is observable.

**Acceptance Scenarios**:

1. **Given** an item at `stage:Validate:complete` with no recoverable SHA, **When** the scan runs after a restart, **Then** the engine applies the documented conservative behavior and logs one line naming the item.
2. **Given** the same item under yolo, **When** a landing is attempted, **Then** the PR is not merged on an unverified head.

---

### User Story 4 - Merge-train catch-up still works after a restart (Priority: P2)

After a restart the #2158 rules still hold. A landed or terminal item is left alone. The engine's own pure catch-up merge is not treated as an external push.

**Why this priority**: Restoring the SHA must not reintroduce the bug #2158 fixed, where Validate was stripped from just-landed or just-caught-up items.

**Independent Test**: Restart over an item whose branch the merge train caught up before the restart, then run the scan. The item is not invalidated and the catch-up is not read as an external push.

**Acceptance Scenarios**:

1. **Given** a member caught up by the merge train before a restart, **When** the scan runs after the restart, **Then** Validate is not invalidated by the catch-up commit.
2. **Given** a landed or Done item after a restart, **When** the scan runs, **Then** its completion labels are left in place.

---

### Edge Cases

- The PR head changes between the engine going down and coming back up (covered by User Story 2).
- The item has no linked PR, or the PR is closed, merged or unreadable at startup.
- The Validate stage comment is missing, was deleted or edited, or does not carry a readable commit.
- Evidence is readable but names a commit that is no longer an ancestor of the PR head (force-push).
- Validate was re-run several times; the restored SHA must belong to the most recent completed run.
- Several restarts in a row, and a restart while a Validate worker was in flight.
- The first scan after a restart runs before the evidence has been read.
- A pre-feature item that never had a recorded SHA at all.
- The SHA was last re-recorded by the #2158 pure-catch-up path (see Assumptions).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a restart, every item carrying `stage:Validate:complete` MUST have its Validate completion SHA available, or MUST be handled by a defined conservative rule, before the SHA-invalidation scan first evaluates it.
- **FR-002**: The restored SHA MUST be the head Validate ran on, or an older commit. It MUST NOT be a head the PR acquired after Validate completed.
- **FR-003**: The mechanism for restoring the SHA (persisting it durably, or rebuilding it from durable GitHub evidence such as the commit recorded in the Validate stage comment) MUST be chosen and recorded in an ADR, with the reasoning for the rejected alternative.
- **FR-004**: When no SHA can be restored from any source, the engine MUST NOT let the item land on an unverified head. The ADR MUST state which conservative behavior applies (re-run Validate, or hold the landing with one log line).
- **FR-005**: A restored or conservatively handled item MUST NOT be invalidated when the PR head still equals the head Validate ran on.
- **FR-006**: After a restart, the #2158 behavior MUST be unchanged. A landed or terminal item keeps its completion, and the engine's own pure catch-up merge is not treated as an external push.
- **FR-007**: Restoring the SHA MUST NOT change behavior for items that do not carry `stage:Validate:complete`.
- **FR-008**: Any restoration or conservative action MUST emit a log line that names the item and the source used (or that none was available), so an operator can tell the cause after the fact.
- **FR-009**: If the restore mechanism is not usable (evidence unreadable, state file corrupt or absent), the engine MUST fall back to the FR-004 conservative behavior and MUST NOT silently skip the item.
- **FR-010**: Tests: a unit test that replaces the engine and store over an item at Validate-complete, pushes a new head and shows Validate is invalidated; a sim scenario using `RestartEnv` doing the same; and a non-vacuity check in which disabling the restore makes that sim scenario fail.

### Key Entities

- **Validate completion SHA**: The PR head the most recent completed Validate run ran on. It is the baseline the invalidation scan compares against.
- **Validate stage comment**: The durable GitHub comment the engine writes when Validate runs, which records a commit in its header.
- **Restoration source**: Whatever durable record the ADR selects to rebuild the completion SHA after a restart.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For an item at Validate-complete that is restarted and then pushed to, Validate is invalidated within one poll of the new head being observed, in 100% of the test runs.
- **SC-002**: In no tested scenario is the restored SHA equal to a head pushed after Validate completed.
- **SC-003**: With no recoverable SHA, no tested scenario merges the PR before Validate has run on its current head.
- **SC-004**: The #2158 tests (landed/terminal exemption and pure catch-up) pass both without and with a restart in between.
- **SC-005**: The new sim scenario fails when the restore is disabled and passes when it is enabled.

## Assumptions

- The selection between persisting the SHA and rebuilding it from GitHub evidence is deferred to Research and Plan and recorded in the ADR. The spec constrains the outcome (FR-002, FR-004, FR-009), not the mechanism.
- Fabrik already writes a commit identifier into the Validate stage comment header; whether it is reliable enough as a source is for Research to confirm.
- A SHA re-recorded after a pure catch-up merge (#2158) is also lost on restart. Treating this as covered by FR-006 and FR-002 means the restored value may be older than the live head and must still not produce a false invalidation for the engine's own catch-up. The marker-based check from #2158 is expected to cover that case.
- Items without `stage:Validate:complete` need no restoration.
- A one-time extra Validate run after a restart in an ambiguous case is acceptable; landing unvalidated code is not.
- The existing pre-feature behavior of skipping items with no recorded SHA ("FR-5" in the scan) is superseded for items that carry `stage:Validate:complete`.

## Out of Scope *(optional)*

- Persisting other in-memory item state flagged by the same audit (this issue covers only the Validate completion SHA).
- Changing what counts as an external push versus the engine's own catch-up beyond keeping #2158 working.
- Changing Validate's content, gates, or the yolo/cruise landing rules.
- Restoring any state for stages other than Validate.

## Source References *(optional)*

- Engine state audit, `docs/design/engine-state-audit-2026-10.md` §1.3 (2026-10-15), cited in the issue
- ADR-2158 and `docs/state-machine.md` §2.16 (SHA invalidation, landed/terminal and pure catch-up exemptions)
