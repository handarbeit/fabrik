# Feature Specification: fabrik_set_blocked_by overseer action to add and remove blockedBy edges

**Feature Branch**: `fabrik/issue-2385`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description: "

> ## Problem
>
> An overseer session can promote, advance, park, set autonomy, revalidate and clear the Claude limit through the `fabrik` MCP tools, but it cannot order work. Setting a `blockedBy` edge today means leaving the MCP for raw GraphQL (`addBlockedBy`) under a separate token. That write leaves no Fabrik audit comment and gives the Store no write-through, so the cached `BlockedBy` stays stale until the next deep fetch.
>
> Ordering is the one thing an overseer does routinely when sequencing a milestone. On 2026-10-18 the statefix flight needed four edges set by hand before any item could be moved to Specify. Edges must exist **before** an item is given an autonomy label or a pipeline column, or Fabrik races ahead.
>
> ## Requirements
>
> 1. **`fabrik_set_blocked_by`** takes an issue, plus a list of blocker issues to add and a list to remove. Blockers may be in another repo, written as `owner/repo#N`; a bare `N` means the same repo. It follows the overseer-action pattern (ADR-1966-c, `engine/overseer_actions.go`):
>    - an optional `localapi.Actor` method;
>    - one `🏭 **Fabrik — overseer action: set_blocked_by**` audit comment naming the subscriber and the edges added and removed;
>    - write first, then the comment. A failed comment leaves the edges in place and reports `audit_comment: failed`.
> 2. **Add** uses the existing `AddBlockedByIssue`. **Remove** needs a new `RemoveBlockedByIssue` on `GitHubClient` (GraphQL `removeBlockedBy`), with the same error handling, plus a sim (`simgh`) model.
> 3. **Guards**, which refuse and return the current state, like the other actions:
>    - the target must be an open, non-PR issue on the managed board;
>    - each blocker must resolve to an issue;
>    - an issue cannot block itself;
>    - adding an edge that would close a cycle among cached edges is refused, and an unknown edge does not count as no cycle (unknown facts refuse, as elsewhere);
>    - adding an edge that already exists, or removing one that is absent, is a no-op reported as such, never an error.
> 4. **Store write-through.** After a successful write, the item's cached `BlockedBy` reflects the change, so `checkDependencies`, `fabrik_status` and the attention view see it at once. If the engine's `fabrik:blocked` decision is affected, it is still made only by `checkDependencies`' live read (ADR-1419). The tool never adds or removes `fabrik:blocked` itself.
> 5. **No dispatch side effects.** The tool starts no work and moves no column. An item that loses its last blocker is unblocked by the engine's normal path on its next evaluation.
> 6. **Docs.** Cover the new tool in:
>    - the tool list in the MCP server instructions;
>    - the PM plugin docs (`plugin/fabrik`);
>    - `docs/USER_GUIDE.md` and `docs/state-machine.md` §7.15.
>
>    Regenerate `docs/llms-full.txt`.
>
> ## Scope
>
> - In scope: `internal/localapi` (method and types), `internal/mcpstdio` (tool), a new `engine/overseer_*.go` file (kept out of `localapi*.go`/`channel_*.go` so the read-path GitHub scans stay strict), `github` (`RemoveBlockedByIssue`), `simgh`, and the docs.
> - Out of scope: GitHub sub-issues (parent/child) and bulk reordering beyond one issue per call.
>
> ## Acceptance
>
> - A `tests/sim` scenario adds an edge and sees the item held by `checkDependencies`, then removes it and sees the item released on the next evaluation. Both changes are visible in `fabrik_status` without a refetch.
> - Refusal tests cover self-block, a cycle, a closed or PR target, an off-board target and an unresolvable blocker. Each one makes no GitHub write.
> - A test shows the audit comment is not read as steering, alongside the existing overseer-audit tests (`findNewComments`, `resumeAuthorised`, `feedbackGateBlocks`).
> - `TestLocalAPISourceDoesNotReachGitHub` and the channel scan still pass unchanged."

## Background

An overseer session drives Fabrik through the `fabrik` MCP tools: promote, advance, park, set autonomy, revalidate, and clear the Claude limit. None of them can order work. To set a `blockedBy` edge today, the overseer must leave the MCP, call raw GraphQL (`addBlockedBy`) under a separate token, and accept two costs:

- No Fabrik audit comment records who ordered the work or why.
- The Store gets no write-through, so the cached `BlockedBy` stays stale until the next deep fetch. `checkDependencies`, `fabrik_status` and the attention view can disagree with GitHub in the meantime.

Ordering is routine when sequencing a milestone. On 2026-10-18 the statefix flight needed four edges set by hand before any item could be moved to Specify. Timing matters: edges must exist **before** an item is given an autonomy label or a pipeline column, or Fabrik races ahead of the intended order.

This extends the overseer-action family (ADR-1966-c, `docs/state-machine.md` §7.15). Those actions reuse transitions the engine already has, write first, then post one audit comment, and refuse with the item's current state when a guard fails. The `fabrik:blocked` decision stays with `checkDependencies`' live read of `BlockedBy` (ADR-1419).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Order work by adding blockers (Priority: P1)

An overseer sequencing a milestone makes issue B wait for issue A (possibly in another repo) with one MCP call, before B receives an autonomy label or a pipeline column.

**Why this priority**: This is the problem the issue exists to solve. Without it the overseer cannot order work through the MCP.

**Independent Test**: In the sim bed, call the action to add A as a blocker of B, then poll. B is held by `checkDependencies`, `fabrik_status` for B lists A as a blocker immediately, and one audit comment names the subscriber and the edge added.

**Acceptance Scenarios**:

1. **Given** open issues B and A on the managed board with no edge between them, **When** the overseer adds A as a blocker of B, **Then** the edge exists on GitHub, B's cached `BlockedBy` includes A, `fabrik_status` shows it without a refetch, and `checkDependencies` holds B on its next evaluation.
2. **Given** a blocker written as `owner/repo#N` in another repo, **When** it is added, **Then** it resolves to that repo's issue and the edge is created.
3. **Given** a blocker written as a bare `N`, **When** it is added, **Then** it means the same repo as the target.
4. **Given** a successful add, **When** the engine next evaluates B, **Then** only `checkDependencies` decides whether `fabrik:blocked` is applied. The action itself touches no label.

---

### User Story 2 - Release work by removing blockers (Priority: P1)

An overseer lifts an ordering constraint that no longer applies, and the item resumes through the engine's normal path.

**Why this priority**: Ordering needs to be reversible. A wrong edge would otherwise need raw GraphQL to undo.

**Independent Test**: Starting from the state in Story 1, remove the edge. `fabrik_status` shows no blocker at once, and on the next evaluation `checkDependencies` finds no unresolved blocker and the item is released.

**Acceptance Scenarios**:

1. **Given** B blocked by A, **When** the overseer removes A, **Then** the edge is gone on GitHub, B's cached `BlockedBy` no longer includes A, and the audit comment names the edge removed.
2. **Given** B loses its last blocker, **When** the engine next evaluates B, **Then** it clears `fabrik:blocked` through its normal path. The action itself starts no work, moves no column and removes no label.

---

### User Story 3 - Safe refusals and idempotence (Priority: P1)

A mistaken or repeated call never corrupts ordering or spends a GitHub write. It is refused with the item's current state, or reported as a no-op.

**Why this priority**: The socket is unauthenticated and callers are LLM sessions. The guards are what keep this write safe, so they ship with the first version.

**Independent Test**: For each refusal case below, call the action and assert that the result is a refusal carrying current state and that the fake GitHub recorded no write.

**Acceptance Scenarios**:

1. **Given** a target that is closed, a pull request, or not on the managed board, **When** the action is called, **Then** it is refused with the item's current state (or the reason when the item is not on the board), and nothing is written.
2. **Given** a blocker that does not resolve to an issue, **When** the action is called, **Then** it is refused and nothing is written.
3. **Given** a blocker equal to the target, **When** the action is called, **Then** it is refused as a self-block.
4. **Given** B blocked by A (cached), **When** the overseer adds B as a blocker of A, **Then** it is refused as a cycle.
5. **Given** a cycle check that must traverse an edge whose cached value is unknown, **When** an add is requested, **Then** it is refused, because an unknown edge never counts as no cycle.
6. **Given** an edge that already exists, **When** it is added again, or an absent edge is removed, **Then** the result reports a no-op for that edge, not an error, and no GitHub write is made for it.
7. **Given** a successful write whose audit comment fails to post, **When** the result is returned, **Then** the edges stay in place and the result reports `audit_comment: failed`.

---

### User Story 4 - The audit comment never steers (Priority: P2)

The audit comment is visible to humans on the issue but is never mistaken for operator steering by the engine.

**Why this priority**: Under PAT auth the engine's own comments are authored by the operator. The `🏭 **Fabrik` prefix is the only thing that keeps them from resuming a pause or counting as feedback.

**Independent Test**: Alongside the existing overseer-audit tests, assert that the `set_blocked_by` comment is ignored by `findNewComments`, `resumeAuthorised` and `feedbackGateBlocks`.

**Acceptance Scenarios**:

1. **Given** a paused item and a posted `set_blocked_by` audit comment, **When** `resumeAuthorised` is evaluated, **Then** the comment does not resume the pause.
2. **Given** a posted `set_blocked_by` audit comment, **When** `findNewComments` and `feedbackGateBlocks` run, **Then** it is neither new work nor unprocessed feedback.

---

### Edge Cases

- One call that both adds and removes edges: every guard is evaluated for all requested edges before any write. A blocker named in both lists is refused as contradictory.
- An add and remove of different blockers in one call: each edge is reported individually as applied, no-op, or refused.
- A blocker in a repo the managed board does not track: it must still resolve to an issue, but is not required to be on the board. Its edges count as unknown for cycle detection, per FR-009.
- A GitHub write that fails partway through a multi-edge call: edges already written stay, the cache reflects exactly what was written, and the result states which edges were applied and which were not.
- A blocker that is closed: the add is allowed, since a closed blocker is simply already satisfied and `checkDependencies` decides what that means.
- Repeating a call after a timeout: idempotence (FR-007) makes it safe.
- An empty add and remove list: refused as a malformed request, with no comment.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The `fabrik` MCP server exposes `fabrik_set_blocked_by`, taking one target issue, a list of blockers to add and a list of blockers to remove. Blockers are written as `owner/repo#N`, or a bare `N` meaning the target's repo. One issue per call.
- **FR-002**: The action follows the overseer-action pattern (ADR-1966-c). It is served through the optional `localapi.Actor` interface, so a server with no `Actor` answers `unknown_method` and `hello` omits the method. Refusals are `CodeRefused` and carry the target's current state in `Error.Data`.
- **FR-003**: After the edges are written, the engine posts exactly one `🏭 **Fabrik — overseer action: set_blocked_by**` audit comment naming the subscriber and the edges added and removed. Writes come first, then the comment. A failed comment leaves the edges in place and the result reports `audit_comment: failed`. A call with nothing to write, whether all no-ops or refused, posts no comment.
- **FR-004**: `GitHubClient` gains `RemoveBlockedByIssue` (GraphQL `removeBlockedBy`) with the same error handling as `AddBlockedByIssue`, and the sim GitHub (`simgh`) models it. Adding an edge uses the existing `AddBlockedByIssue`.
- **FR-005**: The target must be an open, non-PR issue that is on the managed board. Otherwise the action is refused with current state and writes nothing.
- **FR-006**: Each blocker must resolve to an issue, and an issue cannot block itself. Otherwise the action is refused and writes nothing.
- **FR-007**: Adding an edge that already exists, or removing one that is absent, is a no-op reported as such per edge, never an error, and makes no GitHub write for that edge.
- **FR-008**: Adding an edge that would close a cycle among cached `blockedBy` edges is refused and writes nothing.
- **FR-009**: Adding an edge is also refused when the cycle check needs an edge whose cached value is unknown. Unknown facts refuse and never count as no cycle. The refusal names the issue whose edges are unknown. Removals need no cycle check.
- **FR-010**: Every guard is evaluated for all requested edges before any GitHub write, so a refusal never leaves a partial change. A blocker named in both the add and remove lists is refused.
- **FR-011**: After each successful write, the target's cached `BlockedBy` reflects the change through a Store write-through, so `checkDependencies`, `fabrik_status` and the attention view see it without a refetch.
- **FR-012**: The action never adds or removes `fabrik:blocked`. The decision stays with `checkDependencies`' live read of `BlockedBy` (ADR-1419).
- **FR-013**: The action starts no work, moves no board column and changes no label. An item that loses its last blocker is released by the engine's normal path on its next evaluation.
- **FR-014**: The audit comment is not steering. It is ignored by `findNewComments`, `resumeAuthorised` and `feedbackGateBlocks`, and the compliance scan that lists `overseerAuditBody` covers the new action.
- **FR-015**: The engine side lives in a new `engine/overseer_*.go` file, not in `localapi*.go` or `channel_*.go`. `TestLocalAPISourceDoesNotReachGitHub` and the channel scan pass unchanged.
- **FR-016**: The new tool is documented in the MCP server instructions' tool list, the PM plugin docs (`plugin/fabrik`), `docs/USER_GUIDE.md` and `docs/state-machine.md` §7.15. `docs/llms-full.txt` is regenerated.

### Key Entities *(if applicable)*

- **Target issue**: The issue whose `blockedBy` edges change. It must be a managed, open, non-PR board item.
- **Blocker**: An issue, in the same or another repo, that the target waits on. It must resolve to an issue but need not be on the board.
- **Edge**: One `blockedBy` relation from target to blocker, which can be added, removed, or reported as a no-op.
- **Audit comment**: The single `🏭 **Fabrik — overseer action: set_blocked_by**` comment recording who changed which edges.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An overseer can add and remove `blockedBy` edges, including cross-repo ones, using only the `fabrik` MCP tools, with no separate GitHub token or raw GraphQL.
- **SC-002**: A sim scenario adds an edge and the item is held by `checkDependencies`. It then removes the edge and the item is released on the next evaluation. In both cases `fabrik_status` reflects the change before any refetch.
- **SC-003**: Tests for self-block, cycle, unknown-edge cycle check, closed target, PR target, off-board target and unresolvable blocker each show a refusal and zero GitHub writes.
- **SC-004**: Repeating any successful call reports all-no-op, makes zero writes and posts no comment.
- **SC-005**: A test shows the audit comment is ignored by `findNewComments`, `resumeAuthorised` and `feedbackGateBlocks`, alongside the existing overseer-audit tests.
- **SC-006**: Every call adds exactly one audit comment when it writes at least one edge, and none otherwise.
- **SC-007**: `TestLocalAPISourceDoesNotReachGitHub`, the channel scan and the `docs-drift` check pass.

## Assumptions

- A blocker not tracked in the Store has no cached edges, so a cycle check that must walk through it counts as unknown and refuses (FR-009). This is the conservative reading of "unknown facts refuse". If it proves too strict for cross-repo blockers, relax it in a follow-up.
- Cycle detection walks the transitive cached `blockedBy` edges of the proposed blocker looking for the target. Cached edges are the only source, with no live reads, matching the issue's "among cached edges".
- A blocker may be closed. Only the target must be open.
- Edge writes within one call run in order after all guards pass. A GitHub failure partway stops the call and the result reports what was applied.
- The subscriber name in the audit comment is the shim's resolved name, sanitised as for the other overseer actions (ADR-1966-c, ADR 2322). There is no per-call override.
- "Managed board" means the board this engine instance serves, as for the other overseer actions.

## Out of Scope *(optional)*

- GitHub sub-issues (parent/child relationships).
- Bulk reordering beyond one target issue per call.
- Adding or removing `fabrik:blocked`, which stays with `checkDependencies`.
- Moving columns, applying autonomy labels, or dispatching work as a side effect.
- Live reads for cycle detection.
