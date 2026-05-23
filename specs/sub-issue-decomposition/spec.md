# Feature Specification: Sub-Issue Decomposition (Cross-Repo Capable) via blockedBy

**Feature Branch**: `feat/sub-issue-decomposition`
**Created**: 2026-05-23
**Status**: Draft
**Input**: User description: "Allow the Plan stage to decompose a Fabrik issue into multiple sub-issues — each in any repo on the same project board, including the parent's own repo — linked as cross-repo `blockedBy` dependencies of the parent. The engine performs the actual sub-issue creation as a pre-Implement step after the user accepts Plan's output, not during Plan itself, so Plan can be revised by comment without side effects. The parent then waits at the head of the chain via the existing `fabrik:blocked` machinery until all sub-issues close. Implement must be prevented from leaving its assigned worktree."

## Background and Motivation *(non-mandatory context)*

Three real problems motivate this feature:

**Problem 1: Multi-repo work is invisible to Fabrik.** When an issue's spec implies work in another repo, the Implement skill currently goes "off-script": it reaches into the user's local working copies via absolute paths, creates branches and PRs outside Fabrik's worktree model, and produces output Fabrik cannot see or review. This was observed concretely on `verveguy/liminis-graph#42`, which produced an invisible PR `verveguy/liminis#785` while Fabrik moved the parent to Done based on a stub PR (`liminis-graph#45`) containing only an ADR and a spec doc.

**Problem 2: Large work has nowhere to go.** Today, an issue is either small enough for one Implement turn budget, or the user splits it manually before submitting. The existing `FABRIK_DECOMPOSED` mechanism (ADR-017) addresses this partially — see Problem 3.

**Problem 3: The existing `FABRIK_DECOMPOSED` mechanism is limited and out of step with engine ownership principles.** Today's `FABRIK_DECOMPOSED` (ADR-017, `engine/stages.go:handleDecomposed`) lets Plan split an oversized issue into sub-issues, but it has structural issues:
- **Plan's skill prompt calls `gh` CLI directly** to create sub-issues, add them to the board, and link them — violating the engine-owns-state principle the ADR itself cites.
- **Hard depth-1 limit**: sub-issues cannot themselves decompose (enforced by a skill-side check for the `fabrik:sub-issue` label).
- **Parent goes straight to Done** with no own-work pipeline; there is no way for a parent to wait on its children and then run its own Implement.
- **No cross-repo design**: while `gh issue create --repo` accepts any repo, the rest of the flow assumes flat same-repo sub-issues with no blockedBy gating against the parent.

A single mechanism solves all three: let Plan decompose work into sub-issues, linked to the parent via `blockedBy`, with the engine owning the mutations. Whether those sub-issues land in the same repo or different repos is just a property of the spawn, not a different feature. Cross-repo `blockedBy` works on GitHub today (verified empirically in this session against `verveguy/liminis` and `verveguy/liminis-framework`), and Fabrik's existing `checkDependencies` machinery already handles cross-repo dependencies correctly — it reads `repository.nameWithOwner` for each dep, keys the Store lookup by `(depRepo, depNumber)`, and the `fabrik:blocked` gate already gives us the parent-waits semantics for free.

**The pure-coordinator case is handled by composition, not a special path.** When a parent has no implementation work of its own after its children close, its Implement stage runs, finds nothing to do, and emits `FABRIK_STAGE_COMPLETE` + `FABRIK_NO_WORK_NEEDED` (existing machinery). The engine moves the parent directly to Done. This subsumes the "parent → Done immediately" semantics of the old `FABRIK_DECOMPOSED` without a dedicated marker.

The remaining work is concentrated in three places: a Plan-skill convention for declaring sub-issues as part of the plan output, a new pre-Implement engine step that performs the actual GitHub mutations after the user (or yolo) accepts the plan, and removal of the existing `FABRIK_DECOMPOSED` mechanism.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Plan decomposes work into a sub-issue chain (Priority: P1)

When a parent issue's plan calls for work in one or more sub-units (whether different repos, or just independently-shippable chunks in the same repo), Fabrik creates one sub-issue per chunk, links each as `blockedBy` of the parent, and gates the parent at Implement until all children close.

**Why this priority**: This is the entire feature. Without it, multi-repo specs fail silently and epic-sized work has no clean home.

**Independent Test**: File a parent issue whose plan emits three `FABRIK_SPAWN_CHILD` blocks (one targeting a different repo, two targeting the parent's own repo). Advance the parent to Implement. Verify three sub-issues exist on the same project board with the correct titles and scoped bodies; the parent has `fabrik:blocked` and `fabrik:children-spawned`; the cross-repo and same-repo blockers all appear in the parent's "blocked on dependencies" comment.

**Acceptance Scenarios**:

1. **Given** a parent issue whose Plan output contains `FABRIK_SPAWN_CHILD` blocks for two repos other than the parent's, **When** the parent advances from Plan to Implement, **Then** the engine's pre-Implement step creates two new sub-issues (one per block), adds each to the same project board, and links each as `blockedBy` of the parent.
2. **Given** a parent issue whose Plan output contains `FABRIK_SPAWN_CHILD` blocks targeting the parent's own repo, **When** the parent advances to Implement, **Then** the engine creates same-repo sub-issues using the same mechanism; no special case is needed.
3. **Given** any sub-issue created by the spawn step, **When** the new issue is inspected, **Then** its body contains the scoped spec from Plan's `FABRIK_SPAWN_CHILD` block verbatim, followed by an engine-appended footer linking back to the parent issue.
4. **Given** the parent has `fabrik:children-spawned` and at least one open child, **When** Fabrik next polls, **Then** the parent has `fabrik:blocked` and the existing "waiting for" comment lists all open children using `owner/repo#N` format for cross-repo and `#N` for same-repo.
5. **Given** all children close, **When** Fabrik observes the state changes, **Then** `fabrik:blocked` clears on the parent and Implement's Claude invocation finally fires.

### User Story 2 — Plan output stays freely revisable until the user advances (Priority: P1)

Spawning happens at the Plan→Implement transition, not during Plan. The user can comment on the parent during Plan as many times as needed; each revision re-runs Plan and may emit a different set of spawn blocks. No sub-issues exist on GitHub until the user explicitly advances (or yolo auto-advances).

**Why this priority**: Without this property, comment-driven Plan revision would have to delete and recreate sub-issues each time, which is the reconciliation hell we explicitly want to avoid (see Out of Scope).

**Independent Test**: Run Plan on a parent; verify no sub-issues are created. Comment on the parent asking for a tweak; Plan re-runs; verify still no sub-issues. Advance the parent to Implement; verify sub-issues are now created based on the latest Plan output.

**Acceptance Scenarios**:

1. **Given** a parent issue in the Plan column with completed Plan output, **When** Plan re-runs (via comment processing or manual invocation), **Then** no sub-issues are created and no `addBlockedBy` mutations are made.
2. **Given** the user advances the parent to Implement, **When** the engine's pre-Implement step runs, **Then** it parses only the most-recent Plan output and spawns sub-issues from that single source.
3. **Given** a parent that already has `fabrik:children-spawned`, **When** the user moves the parent back to Plan and then forward again, **Then** the engine MUST NOT re-spawn sub-issues; the label is the idempotency guard. The user must manually remove `fabrik:children-spawned` (and close obsolete children) to trigger a fresh spawn.

### User Story 3 — Children can themselves be epics (Priority: P2)

A sub-issue is just a Fabrik issue. It runs its own Specify→Research→Plan→Implement pipeline. If its own Plan decides further decomposition is warranted, it spawns grandchildren using the same mechanism. There is no depth limit and no special "epic" entity — the decomposition is recursive by construction. This explicitly removes the hard depth-1 limit imposed by the existing `FABRIK_DECOMPOSED` mechanism.

**Why this priority**: The user explicitly wants epic → sub-epic → leaf-issue decomposition. The mechanism naturally supports it; this story is here to confirm we don't accidentally block it with a guard.

**Independent Test**: Spawn a child whose own Plan emits spawn blocks. Verify the grandchildren are created, linked as blockedBy of the child, and gate the child's Implement just like the parent's.

**Acceptance Scenarios**:

1. **Given** a child issue created by pre-Implement on its parent, **When** the child runs its own Plan and emits `FABRIK_SPAWN_CHILD` blocks, **Then** the child's pre-Implement step spawns grandchildren without any "depth" check.
2. **Given** a grandchild closes, **When** Fabrik observes the change, **Then** the child's `fabrik:blocked` re-evaluates exactly as the parent's does; the grandchild and child relationships are independent.

### User Story 4 — Implement stays inside its assigned worktree (Priority: P1)

The Implement skill cannot escape its assigned worktree by `cd`-ing into other working copies, operating on absolute paths outside the worktree, or creating branches/PRs in repos other than the one its worktree belongs to.

**Why this priority**: Without this guardrail, even with the spawn chain in place, Implement may still produce off-script output. The whole spawn architecture only works if implementation truly happens in the children's worktrees.

**Independent Test**: Run Implement on a child issue whose spec references files in another repo by absolute path. Implement must refuse to edit those files, must not push to other repos, and must surface the mis-scoped spec as a blocker rather than silently fix it.

**Acceptance Scenarios**:

1. **Given** an Implement worktree at `.fabrik/worktrees/<owner-repo>/issue-N/`, **When** Implement is invoked, **Then** the skill prompt explicitly prohibits writes, branch creation, or `gh pr create` calls targeting any path or repo outside the worktree.
2. **Given** a Plan output that references files outside the worktree's repo, **When** Implement encounters this, **Then** Implement signals `FABRIK_BLOCKED_ON_INPUT` with an explanatory comment rather than reaching outside.

> Hard tool-restriction enforcement of these guardrails is tracked in [handarbeit/fabrik#761](https://github.com/handarbeit/fabrik/issues/761). This spec relies on prompt-level prohibition only.

### User Story 5 — Cross-repo and same-repo blocker state changes propagate to the parent (Priority: P1)

When a sub-issue closes — in any repo, including the parent's own — the parent sees the unblock in real time (webhook path) or at the next poll (pull path), and Fabrik resumes work on the parent without manual intervention.

**Why this priority**: The chain is useless if the unblock signal doesn't arrive. Fabrik already supports cross-repo blockedBy in `checkDependencies`; this story is about verifying the push-unblock observer covers all participating repos.

**Independent Test**: With a parent blocked by one cross-repo and one same-repo child, close each in turn. Confirm Fabrik removes `fabrik:blocked` from the parent within one poll cycle (pull path) and within seconds (push path) once both have closed.

**Acceptance Scenarios**:

1. **Given** the parent's repo is configured for webhook delivery, **When** a same-repo child closes, **Then** the parent unblocks within Fabrik's standard push-unblock latency.
2. **Given** a cross-repo child's repo is configured for webhook delivery, **When** that child closes, **Then** the parent unblocks within Fabrik's standard push-unblock latency.
3. **Given** a child's repo is not configured for webhooks (poll-only), **When** the child closes, **Then** the parent unblocks at the next full board poll.
4. **Given** the parent has multiple children across multiple repos, **When** some but not all close, **Then** `fabrik:blocked` remains set and the "waiting for" comment lists only still-open blockers.

### User Story 6 — Parent issue has a readable rollup of child state (Priority: P3)

A human looking at the parent issue can see at a glance which children exist, what state each is in, and which PR (if any) is attached. The Fabrik "blocked on dependencies" comment is updated rather than re-posted as new children appear or change state.

**Why this priority**: Quality-of-life. The blockedBy machinery already works without this, but humans need to navigate the chain.

**Independent Test**: Open the parent issue in the GitHub UI. Confirm the Fabrik-posted dependency comment lists all current blockers with their state, and is the only such comment (not one per change).

**Acceptance Scenarios**:

1. **Given** the parent's blocker set changes (a child closes, or pre-Implement just ran), **When** Fabrik next evaluates dependencies, **Then** the existing dependency comment is edited in place rather than a new one being posted.
2. **Given** a child has a linked PR, **When** the parent's dependency comment renders, **Then** the child's PR number is included next to the child issue reference.

### Edge Cases

- **Child fails repeatedly.** A child exhausts `max_retries` and is paused with `fabrik:paused`. The parent remains `fabrik:blocked`. Fabrik must escalate visibly on the parent (e.g., an updated dependency comment noting the child is paused) rather than letting the parent sit silently forever.
- **Child closed without merge.** A user manually closes a child without merging its PR. The parent treats this as resolved (consistent with the existing `checkDependencies` behavior — `state == CLOSED` is sufficient). Documented in the user guide as a known gotcha: closing a child unblocks the parent regardless of merge state, so close deliberately.
- **Same-repo siblings.** Multiple sub-issues can be active in the same repo at once. Each gets its own worktree under `.fabrik/worktrees/<owner-repo>/issue-N/` per existing convention. Fabrik's existing concurrency model (per-worktree mutex, parallel dispatch up to `MaxConcurrent`) handles this without changes. When a sibling's PR merges to main, other active worktrees in the same repo may be behind — the existing "update from main" step at each stage handles drift.
- **Deep decomposition.** Children spawn grandchildren spawn great-grandchildren. No depth limit. Each level uses the same mechanism. If decomposition is pathologically deep, the user will notice via the project board and can intervene; no engine-level guard is added.
- **Plan re-emits spawn blocks after children are spawned.** If `fabrik:children-spawned` is present, pre-Implement skips entirely. Plan's re-emitted blocks are ignored. To re-trigger, the user removes the label and closes obsolete children (see Story 2).
- **Race between `addBlockedBy` and board admission.** The engine performs `create → addProjectV2ItemById → addBlockedBy` in that fixed order per child; if any step fails, subsequent steps for that child don't run. Per prior incident, `addBlockedBy` MUST complete before any `fabrik:yolo` label is acted on for the parent.
- **Child in a repo not on the project board.** Engine adds every child via `addProjectV2ItemById` immediately after creation. "Linked repos" on the project are only a hint for auto-add; manual adds work for any repo with access.
- **Child in a repo Fabrik isn't configured to drive.** Pre-Implement validates each `FABRIK_SPAWN_CHILD` block's target repo against Fabrik's managed-repos set BEFORE any mutation. If any target is unmanaged, pre-Implement refuses to spawn any children, posts a comment on the parent listing the gaps, applies `fabrik:paused`, and stops. The user resolves the infrastructure gap (adds the repo to managed-repos / webhook config / clone access), removes `fabrik:paused`, and re-advances.
- **Partial spawn failure.** If the engine successfully creates child 1 of 3 and then the GraphQL call for child 2 fails, the engine MUST attempt to leave the parent in a recoverable state: post an error comment naming what was and was not created, apply `fabrik:paused`, do not apply `fabrik:children-spawned`. Manual cleanup is then required (user closes the orphaned child and re-advances). v1 does not attempt automatic rollback of the partial spawn.
- **Cycle detection.** If a sub-issue somehow ends up blocking an ancestor (e.g., faulty Plan output, manual misconfiguration), Fabrik must not deadlock. `checkDependencies` should detect cycles and surface them with a clear error.
- **Parent has its own implementation work.** Plan may emit spawn blocks AND describe parent-only implementation work. After all children close, the parent's Implement runs normally in its own worktree; children's merged code is already on the parent's base branch via the standard "update from main" step.
- **Parent is purely a coordinator (no own work).** Plan emits spawn blocks but no parent-side implementation guidance. After all children close, the parent's Implement runs, finds nothing to do, and emits `FABRIK_STAGE_COMPLETE` + `FABRIK_NO_WORK_NEEDED`. The existing `handleNoWorkNeeded` engine path then moves the parent directly to Done without creating a PR. This composes the existing `FABRIK_NO_WORK_NEEDED` machinery with the new spawn mechanism — no special-case path for "coordinator-only" parents.
- **Sub-issues co-existing with GitHub's sub-issues UI.** Users may manually add native GitHub sub-issue relationships in addition to Fabrik's blockedBy. The engine ignores native sub-issue relationships and keys only on `blockedBy`; the UI affordance is fine, but MUST NOT be a second source of truth.

## Requirements *(mandatory)*

### Functional Requirements

**Research skill changes**:

- **FR-001**: The Research skill MUST inspect the parent's spec and codebase for cross-repo signals (file paths in other repos, explicit references to other repos by name, code structure that implies a sibling repo's involvement). When such signals are present, Research MUST emit a `## Repositories` section in its output naming every potentially-relevant repo as `owner/repo` (one per line, including the parent's own repo). When no cross-repo signal is present, Research MUST still emit a `## Repositories` section listing only the parent's repo, so downstream stages can rely on the section's presence.
- **FR-002**: Research's `## Repositories` section is the *candidate* set of participating repos. Plan is NOT required to spawn a sub-issue for every named repo — Plan filters this set based on the actual work it decomposes (see FR-004). If Plan determines a Research-named repo needs no work, Plan SHOULD note the omission in its plan body so the user can challenge it.

**Plan skill changes**:

- **FR-003**: Plan MUST consume Research's `## Repositories` section as the authoritative set of repos it MAY spawn into. Plan MUST NOT spawn into any repo not named by Research. If Research missed a repo, the user re-runs Research; Plan does not re-infer.
- **FR-004**: For each unit of work Plan determines should be tracked as an independent sub-issue — whether in a different repo or in the parent's own repo — Plan MUST emit one `FABRIK_SPAWN_CHILD` block. Plan MAY emit zero blocks (no decomposition), one block, or many. There is no upper bound; pathological cases are a Plan-prompt-quality issue, not an engine concern.
- **FR-005**: Each `FABRIK_SPAWN_CHILD` block MUST follow this format exactly:

  ```
  FABRIK_SPAWN_CHILD_BEGIN owner/repo
  TITLE: <single-line title for the new issue>

  <full scoped spec body — markdown, multiple paragraphs OK, no nested FABRIK_* markers>
  FABRIK_SPAWN_CHILD_END
  ```

  The `BEGIN` and `END` lines MUST be on their own lines. The `TITLE:` line MUST be the first non-empty line after `BEGIN`. The body starts after a blank line following the title and continues until the `END` marker. The body becomes the new issue's body verbatim.
- **FR-006**: The scoped spec body in each block MUST describe only the work belonging to that child's repo / unit, and MUST include enough context for the child to run autonomously through its own Specify/Research/Plan stages without consulting the parent.

**Engine: pre-Implement step**:

- **FR-007**: The engine MUST introduce a `preImplement(item)` step that runs as the first action of the Implement stage dispatcher, BEFORE the Claude invocation. This step is engine code, not a Claude skill, and is invisible to the project board.
- **FR-008**: `preImplement` MUST be a no-op if Plan's output contains no `FABRIK_SPAWN_CHILD` blocks. The Implement Claude invocation proceeds immediately.
- **FR-009**: `preImplement` MUST be a no-op if the parent issue already has the `fabrik:children-spawned` label. The label is the idempotency guard. Manual removal of the label (and cleanup of orphaned children) is the only way to trigger a fresh spawn.
- **FR-010**: When `preImplement` has work to do, it MUST execute these steps in order, atomically per child:
  1. Validate every target repo across all blocks is in Fabrik's managed-repos set. If any is not, post an error comment on the parent listing the gap(s), apply `fabrik:paused`, and STOP. Do not partially spawn.
  2. For each `FABRIK_SPAWN_CHILD` block, in the order it appears in Plan's output:
     1. Create the child issue via `POST /repos/{owner}/{repo}/issues` with the block's title and body (plus the engine-appended footer per FR-011).
     2. Add the new issue to the parent's project board via `addProjectV2ItemById`.
     3. Add the new issue as a blocker of the parent via `addBlockedBy`.
  3. After all children are successfully created and linked, apply the `fabrik:children-spawned` label to the parent.
- **FR-011**: The engine MUST append a back-reference footer to each spawned child's body, of the form:

  ```
  ---

  *Spawned by Fabrik from parent issue {parent-owner}/{parent-repo}#{parent-number} as a multi-issue decomposition. The parent's plan is at the link above.*
  ```

  The footer is engine-generated, not skill-generated, so the format is consistent.
- **FR-012**: If any step in FR-010 fails (creation, project add, or blockedBy add), `preImplement` MUST stop, post an error comment on the parent naming what was created and what was not, apply `fabrik:paused`, and NOT apply `fabrik:children-spawned`. v1 does not attempt automatic rollback of partial spawns. (Edge case: partial spawn failure.)
- **FR-013**: When `preImplement` successfully spawns children, the Implement Claude invocation on the parent MUST be deferred. The existing `checkDependencies` mechanism applies `fabrik:blocked` on the next evaluation cycle; the existing dispatcher already refuses to invoke Claude when `fabrik:blocked` is present.

**Engine: existing machinery (verification, not new code)**:

- **FR-014**: The engine MUST continue to gate parent issues via the existing `fabrik:blocked` label and `checkDependencies` logic. No changes to gate semantics. *(Verified: `engine/dependencies.go:52-69` already reads `dep.Repo` and keys the Store by `(depRepo, depNumber)`.)*
- **FR-015**: The engine MUST observe state changes for child issues in their respective repos. For repos with webhook delivery configured, push-unblock observers MUST fire on child close events. For other repos, the next full board poll picks up the change.
- **FR-016**: The Fabrik "blocked on dependencies" comment on the parent MUST be edited in place when the blocker set changes. *(Spike #4 — verify whether this is already the case in `checkDependencies`.)*
- **FR-017**: The engine MUST detect cyclic `blockedBy` relationships and surface a clear error rather than entering a deadlock. *(Spike #3 — verify whether `checkDependencies` already guards this; if not, add a cycle check.)*

**Implement skill changes (guardrails)**:

- **FR-018**: The Implement skill prompt MUST explicitly prohibit writes outside the assigned worktree's working directory.
- **FR-019**: The Implement skill prompt MUST explicitly prohibit `gh pr create`, `git push`, or branch creation for any repo other than the one the worktree belongs to.
- **FR-020**: When Implement encounters spec content that references files or repos outside its worktree, it MUST emit `FABRIK_BLOCKED_ON_INPUT` with an explanatory comment rather than reaching outside.

  > Hard tool-restriction enforcement is tracked separately in [handarbeit/fabrik#761](https://github.com/handarbeit/fabrik/issues/761).

**Removal of the existing `FABRIK_DECOMPOSED` mechanism**:

- **FR-021**: The `FABRIK_DECOMPOSED` marker MUST be removed. Specifically:
  - `decomposedRE` and `CheckDecomposed` in `engine/claude.go` MUST be deleted.
  - `handleDecomposed` in `engine/stages.go` MUST be deleted.
  - The `decomposed` branch in `engine/item.go` (around lines 987–1052) MUST be removed.
  - The marker-stripping calls in `engine/comments.go:268` and `engine/item.go:867` MUST be removed.
  - `engine/decomposed_test.go` MUST be deleted; any test cases worth preserving (e.g., idempotency under partial spawn) MUST be rewritten against the new `preImplement` path.
- **FR-022**: The `## Decomposition` section in `plugin/fabrik-workflows/skills/fabrik-plan/SKILL.md` (lines 119–170) MUST be removed and replaced with documentation of the new `FABRIK_SPAWN_CHILD` convention. The depth-gate check (line 125) and idempotency check (lines 127–129) MUST be removed — both concerns move to engine code (FR-009 idempotency via label; no depth gate, recursive by design).
- **FR-023**: ADR-017 (`adrs/017-decomposed-marker-state-machine.md`) MUST be marked as **Superseded** with a forward pointer to this spec's issue/PR. The ADR file is preserved (do not delete) per repo convention; only its status changes.
- **FR-024**: The `fabrik:sub-issue` label MAY be retained for human-visible filtering, applied engine-side by `preImplement` to each spawned child. It MUST NOT carry any engine semantics under the new design — no skill-side checks, no engine-side gates. If retained, document this clearly so future contributors don't reintroduce label-keyed logic.

**Documentation**:

- **FR-025**: `docs/state-machine.md` MUST be updated to describe: the `FABRIK_SPAWN_CHILD_*` marker convention, the pre-Implement engine step, the `fabrik:children-spawned` label, and the parent-child gate behavior. The existing `FABRIK_DECOMPOSED` section MUST be removed.
- **FR-026**: `docs/USER_GUIDE.md` MUST be updated with a "Sub-issue decomposition" section explaining how Plan fans out work, what labels/state to expect, the same-repo decomposition use case, the pure-coordinator case (composed with `FABRIK_NO_WORK_NEEDED`), and the "closed = resolved" gotcha (Edge Cases). The existing decomposition section (if any) MUST be replaced.
- **FR-027**: `docs/llms-full.txt` MUST be regenerated in the same commit that touches the canonical doc pages, per the project convention.

### Key Entities *(include if feature involves data)*

- **Parent issue**: The issue whose plan emits one or more `FABRIK_SPAWN_CHILD` blocks. Owns the blockedBy chain. Gets `fabrik:children-spawned` after pre-Implement; gets `fabrik:blocked` while any child is open; resumes its own Implement pipeline once all children close.
- **Child issue (sub-issue)**: An issue created by `preImplement` on a parent's Implement dispatch. Lives in any repo named by Research (cross-repo or same-repo as parent). Has a scoped spec written by Plan plus an engine-appended back-reference footer. Linked as `blockedBy` of the parent. Runs through Fabrik's normal pipeline. May itself become a parent with its own children.
- **Project board**: The GitHub Project that holds all participating issues. Pre-Implement adds children to this board explicitly via `addProjectV2ItemById`.
- **blockedBy dependency**: A GitHub-native issue dependency edge created via `addBlockedBy` and read via the `blockedBy(first: N)` GraphQL field. Carries `repository.nameWithOwner` for cross-repo resolution.
- **`FABRIK_SPAWN_CHILD` marker block**: A structured section in Plan's output declaring one sub-issue to spawn. Parsed by `preImplement`; not interpreted at Plan time.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test parent issue whose Plan emits 3 `FABRIK_SPAWN_CHILD` blocks (2 cross-repo, 1 same-repo) completes end-to-end through Fabrik without manual intervention: pre-Implement spawns all 3 children, parent is gated until all close, parent's Implement then runs normally.
- **SC-002**: An issue advanced into Implement with no `FABRIK_SPAWN_CHILD` blocks in its Plan output behaves identically to today's Fabrik flow — Claude is invoked immediately, pre-Implement adds no measurable latency.
- **SC-003**: Across a sample of 10 multi-issue parent runs, zero Implement runs produce file mutations, branches, or PRs outside the assigned worktree.
- **SC-004**: When a child closes, the parent unblocks within one poll interval (pull path) or within the push-unblock observer's standard latency (push path), as measured by `fabrik:blocked` label removal time. Cross-repo and same-repo children behave identically.
- **SC-005**: Plan re-runs during the Plan column (via comment processing) create zero GitHub mutations — no issues created, no project items added, no blockedBy edges added.
- **SC-006**: The Fabrik dependency comment on a parent is edited in place across the lifetime of the blocker set; no parent has more than one such comment at any time.
- **SC-007**: No Fabrik issue is moved to Done while it still has open blockers. *(Should be guaranteed by the existing gate; success is "still true after this feature ships.")*
- **SC-008**: A child issue's own Plan emitting `FABRIK_SPAWN_CHILD` blocks produces grandchildren via the same mechanism, with no special handling for depth.

## Assumptions

- All target repos for a given decomposition are owned by the same user or organization. (Cross-repo `blockedBy` and project-board admission both work across two repos under the same owner, including personal accounts — verified empirically against `verveguy/liminis` and `verveguy/liminis-framework` in this session.)
- Fabrik is configured (clone access, webhook subscription, managed-repos list) to drive every repo that may appear in a `FABRIK_SPAWN_CHILD` block. If not, pre-Implement refuses before mutating.
- The user's GitHub token has `repo` scope sufficient to create issues, add project items, and call `addBlockedBy` across all participating repos.
- The Research and Plan skills both have access to a tool or pattern for inspecting repository structure. Plan does not need to call GitHub APIs from within its prompt — all mutations are engine-side (FR-010).
- Plan's stored output is accessible to the engine at pre-Implement time. *(Spike: verify the storage location — likely the parent issue body or `.fabrik-context/stage-Plan.md`.)*
- Cross-repo `addBlockedBy` ordering matters: it must complete before any `fabrik:yolo` label triggers further dispatch. The engine's fixed `create → project add → blockedBy → children-spawned label → blocked-by-checkDependencies` order handles this.
- GitHub-native sub-issues are intentionally NOT used by this design. Users may add them manually in the UI for visual affordance, but the engine MUST NOT key on them.

## Out of Scope

- **Native GitHub sub-issues integration.** Even though `addSubIssue` works cross-repo, this design uses `blockedBy` exclusively for engine-level coordination.
- **Cross-org dependencies.** `blockedBy` works within a single user or org; cross-org `blockedBy` is not validated by this design.
- **Hard tool-restriction enforcement of worktree boundaries.** Tracked separately as [handarbeit/fabrik#761](https://github.com/handarbeit/fabrik/issues/761). This spec relies on prompt-level prohibition (FR-018 / FR-019 / FR-020) only.
- **Idempotent / reconciling Plan re-run AFTER children are spawned.** When `fabrik:children-spawned` is present, the engine refuses to re-spawn. The user manually closes obsolete children and removes the label to trigger a fresh spawn. The engine does not attempt to diff Plan output against existing children.
- **Automatic rollback of partial spawn failure.** If creation/linking partially fails, the engine posts an error and pauses; v1 does not attempt to undo the successful steps. Manual cleanup required.
- **Depth limit on decomposition.** A child can spawn grandchildren can spawn great-grandchildren without bound. Pathological cases are a prompt-quality problem, not an engine concern.
- **Inter-sibling sequential dependencies (`A blocks B` between two siblings of the same parent).** The existing `FABRIK_DECOMPOSED` mechanism supported this via `gh issue link --type blocks` calls between sub-issues. The new design's `FABRIK_SPAWN_CHILD` block format does not include a way to express inter-sibling blockedBy edges. To achieve sequencing, use recursive decomposition: spawn one child whose own Plan further decomposes the sequential chunks (so A and B become A and (B-as-child-of-coordinator-C) with the natural blockedBy chain). The existing capability is deliberately not preserved in v1; revisit if real use cases emerge.
- **Renaming / restructuring the existing single-issue Fabrik flow.** This feature is additive (and replaces `FABRIK_DECOMPOSED`); an issue whose Plan emits no `FABRIK_SPAWN_CHILD` blocks continues to behave exactly as today.

## Pre-implementation spikes (Research stage — file findings as comments)

1. **Where is Plan's output stored?** The engine's `preImplement` step needs to read Plan's most-recent output to find `FABRIK_SPAWN_CHILD` blocks. Likely candidates: the parent's issue body (if Plan updates it via `FABRIK_ISSUE_UPDATE`), the most-recent Plan comment, or `.fabrik-context/stage-Plan.md` in the worktree. Confirm and pick one.
2. **Cross-repo push-unblock observer verification.** Confirm `engine/observers.go:PushUnblockObserver` correctly identifies parent issues blocked by child issues in *other* repos. The dependency read path (`checkDependencies`) is verified; the push path needs trace-level confirmation.
3. **Cycle detection in `checkDependencies`.** Read the current implementation and determine whether cyclic `blockedBy` is already guarded. If not, scope the addition (FR-017).
4. **Idempotent dependency comment.** Verify whether the current "🏭 Fabrik — blocked on dependencies" comment is edited in place or re-posted per change (FR-016).
5. **Managed-repo / webhook-coverage detection.** Determine how `preImplement` can reliably check whether a given `owner/repo` is in Fabrik's drivable set. Likely an engine-internal API; no skill involvement needed.
6. **`FABRIK_SPAWN_CHILD` parser placement.** Decide where the marker parser lives in the engine code: alongside the existing `FABRIK_*` marker handlers in `engine/claude.go`, or in a new file. The parser is shared between the Plan-output capture step and the `preImplement` step.
7. **Engine-appended back-reference footer.** Pin down the exact footer text and the GitHub-flavored-markdown rendering so it looks right on the spawned child's issue page (FR-011).
8. **Worktree cleanup for spawned-child parents.** Confirm that when a parent's Implement eventually runs (after children close), the parent's worktree update-from-main step correctly merges the children's work. No code change expected, but the merge behavior needs verification.
9. **Existing `FABRIK_DECOMPOSED` test coverage.** Read `engine/decomposed_test.go` and decide which test cases (if any) are worth porting to the new `preImplement` path. The marker detection tests are obsolete; the idempotency tests and the "moves parent to Done" tests may have analogues in the new design (idempotency via `fabrik:children-spawned`; coordinator-only parents going to Done via `FABRIK_NO_WORK_NEEDED`).
10. **Existing in-the-wild use of `FABRIK_DECOMPOSED`.** Search the project boards in use (handarbeit/fabrik, verveguy/liminis, etc.) for issues that were processed via the old decomposition path. Identify any active parent issues that emitted `FABRIK_DECOMPOSED` and went to Done with open sub-issues still being driven. These must continue to function after the marker is removed; verify the existing `fabrik:sub-issue` children don't break.
