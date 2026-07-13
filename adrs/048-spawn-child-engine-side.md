# ADR 048: Engine-Side Sub-Issue Spawning via blockedBy

**Date**: 2026-05-23  
**Status**: Accepted  
**Supersedes**: [ADR 017](017-decomposed-marker-state-machine.md)

## Context

ADR 017 introduced the `FABRIK_DECOMPOSED` marker: Plan calls `gh issue create` and `gh project item-add` directly inside its Claude session, then outputs `FABRIK_DECOMPOSED` to signal the engine to move the parent to Done. This mechanism has three structural problems:

1. **Engine-owns-state principle violated**: GitHub mutations (issue creation, board admission, blockedBy linking) happen inside a Claude subprocess, invisible to the engine and untestable via `GitHubClient`.
2. **Hard depth-1 limit**: The skill refuses to decompose a `fabrik:sub-issue`, preventing recursive epic decomposition.
3. **No cross-repo design**: Sub-issues are created in the parent's repo only; there is no mechanism for spawning work into other repos linked by `blockedBy`.

Additionally, a concrete multi-repo incident (`acme/widgets-graph#42`) showed Implement reaching outside its worktree to edit files in `acme/widgets`, producing a PR invisible to Fabrik. This established that Implement needs an explicit worktree boundary guardrail.

## Decision

Replace `FABRIK_DECOMPOSED` with an engine-side pre-Implement spawn step, keeping Plan's role declarative.

**Plan** emits structured `FABRIK_SPAWN_CHILD_BEGIN/END` blocks in its output — one per sub-issue to create — with a target `owner/repo`, `TITLE:` line, and scoped spec body. These blocks are preserved in the Plan stage comment; Plan still signals completion with `FABRIK_STAGE_COMPLETE`. Plan makes no GitHub API calls.

**Engine pre-Implement step** (`preImplement` in `engine/spawn.go`) fires before every Implement Claude invocation. It:
1. Returns immediately if `fabrik:children-spawned` is present (idempotency guard).
2. Reads the most-recent Plan comment via `findStageComment(item.Comments, "Plan")`.
3. Parses `FABRIK_SPAWN_CHILD_BEGIN/END` blocks via `ParseSpawnBlocks`.
4. Validates every target repo against `e.worktreeManagers` (Fabrik's managed-repo set); refuses to spawn into any unmanaged repo.
5. For each block in order: `CreateIssue` → `AddProjectV2ItemById` → `AddBlockedByIssue(parentNodeID, childNodeID)` → `AddLabelToIssue("fabrik:sub-issue")`.
6. On complete success: `AddLabelToIssue("fabrik:children-spawned")` on the parent and returns `(true, nil)` to skip the Claude invocation on this cycle.
7. On any failure: posts an error comment, adds `fabrik:paused`, returns an error without adding `fabrik:children-spawned`.

**Parent gating**: `checkDependencies` detects the new `blockedBy` edges on the next evaluation cycle and adds `fabrik:blocked`. The existing blocked gate prevents further Claude invocations until all children close.

**`FABRIK_DECOMPOSED` is removed** entirely: `decomposedRE`, `CheckDecomposed`, and `handleDecomposed` are deleted.

## Rationale

### Engine-owns-state

All GitHub mutations go through `GitHubClient` in `engine/spawn.go`. This makes them testable via `mockGitHubClient`, observable in engine logs, and subject to the same error-handling and retry logic as other engine actions. Claude's subprocess does nothing but declare intent.

### blockedBy over native sub-issues

GitHub now has a native `addSubIssue` API. We use the `addBlockedBy` GraphQL mutation (Go: `AddBlockedByIssue`) instead because:
- Fabrik's existing `checkDependencies` and `PushUnblockObserver` already read and act on `blockedBy` cross-repo, including Store-keyed lookup by `(depRepo, depNumber)`.
- `blockedBy` carries the parent-waits semantics we need: the parent is gated until every blocker closes.
- Native sub-issues are a UI affordance; `blockedBy` is a machine-readable dependency edge. We want the latter.

### fabrik:children-spawned idempotency guard

If `preImplement` is interrupted after creating child 1 of 3 and the user re-advances, re-running without a guard would create duplicates. The label prevents re-spawning without user intent. Manual removal of the label (and cleanup of orphaned children) is the explicit recovery path.

### Coordinator-only parents via FABRIK_NO_WORK_NEEDED composition

When a parent has no own implementation work, Implement runs after all children close, finds nothing to do, and emits `FABRIK_STAGE_COMPLETE` + `FABRIK_NO_WORK_NEEDED`. The existing `handleNoWorkNeeded` path moves the parent to Done without a PR. This subsumes the "parent → Done immediately" semantics of `FABRIK_DECOMPOSED` via composition rather than a dedicated code path.

### No depth limit

The ADR 017 depth-1 limit (skill-side `fabrik:sub-issue` check) is removed. A sub-issue is just a Fabrik issue; if its own Plan emits spawn blocks, the same `preImplement` mechanism applies. Depth limits are a prompt-quality concern, not an engine concern.

### Worktree boundary guardrail

The Implement skill (`fabrik-implement/SKILL.md`) now explicitly prohibits writes, PR creation, and branch creation outside the assigned worktree. When out-of-scope work is encountered, Implement emits `FABRIK_BLOCKED_ON_INPUT` rather than reaching outside. Hard tool-restriction enforcement is tracked separately in handarbeit/fabrik#761.

## Consequences

- `FABRIK_DECOMPOSED` is fully removed from engine and skills. Any Plan output that previously emitted this marker will now produce `FABRIK_STAGE_COMPLETE` instead; Implement fires normally on the parent.
- The Plan skill `SKILL.md` no longer documents `gh issue create` calls. It documents `FABRIK_SPAWN_CHILD_BEGIN/END` blocks and their exact format.
- Research now emits a mandatory `### Repositories` section listing all participating repos so Plan has an authoritative set of valid spawn targets.
- Partial spawn failures leave the parent in `fabrik:paused` with an error comment; v1 requires manual cleanup of orphaned children. Automatic rollback is out of scope.
- The `fabrik:sub-issue` label is retained as a human-visible filter applied engine-side by `preImplement`; it carries no engine semantics under this design.
- `preImplement` has a fourth outcome beyond spawn / no-op / fatal-error: a **deferred recovery retry**, returned as `errPreImplementDeferred`. It fires when `stage:Plan:complete` is present but no Plan comment is found in the item's cached snapshot — a stale deep-field read (#957) that would otherwise be indistinguishable from "Plan declared nothing to spawn" (#982). Recovery attempts a live, uncached re-read before concluding there is genuinely nothing to spawn; when that re-read itself fails, the parent is retried on a subsequent poll rather than paused, throttled by a per-item cooldown (mirroring the `dep-blocked` cooldown) to bound repeated live-read load during sustained failure windows.
