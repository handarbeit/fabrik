# ADR 017: FABRIK_DECOMPOSED Marker and Decomposition State Machine

**Date**: 2026-04-07  
**Status**: Accepted

## Context

Issue #39 adds the ability for the Plan stage to autonomously split an issue into sub-issues when the work is too large for a single Implement cycle. When the Plan stage decomposes an issue, the parent must be moved to **Done** and must not flow through Implement, Review, or Validate. The sub-issues start in the **Research** column and flow through the full pipeline independently.

Two approaches were considered for how Plan signals decomposition and how the parent advances:

**Option A — Engine marker (`FABRIK_DECOMPOSED`)**: Plan outputs a marker on a line by itself, identical in style to `FABRIK_STAGE_COMPLETE`. The engine detects the marker and owns the state transition: adds the completion label, then calls `UpdateProjectItemStatus` with the Done column option ID.

**Option B — Direct board mutation**: Plan calls `gh project item-edit` to move itself to Done on the board, then outputs `FABRIK_STAGE_COMPLETE` so the engine adds the completion label. The engine sees a completed stage and advances normally — but the item is already in Done, so `advanceToNextStage` hits no further stages (Done has no next stage).

## Decision

Use **Option A**: the `FABRIK_DECOMPOSED` marker. The engine owns all state transitions. Plan declares intent ("I decomposed this"); the engine decides what that means structurally.

A new `handleDecomposed(board, item, stage)` function is added to `engine/stages.go`. It:
1. Adds the `stage:<Name>:complete` label (same as `handleStageComplete`)
2. Looks up `e.statusField.Options["Done"]` with an ok-check
3. Calls `UpdateProjectItemStatus` with the Done option

The detection chain is identical to `FABRIK_BLOCKED_ON_INPUT`: a module-level `decomposedRE` regex and a `CheckDecomposed(output string) bool` function in `engine/claude.go`, checked in `engine/item.go` after `InvokeClaude` returns. `FABRIK_DECOMPOSED` is a fourth terminal outcome alongside `completed`, `blockedOnInput`, and the retry/fail path.

## Rationale

### Marker approach vs. direct board mutation

- **Engine owns state, not Claude**: Fabrik's design principle is that the engine manages all state transitions on the GitHub Project board. Claude (via Claude Code) is a worker that signals outcomes; the engine decides what those outcomes mean. If Plan calls `gh project item-edit` directly, it bypasses this principle and creates a parallel state management path that's harder to test and reason about.

- **Testability**: `FABRIK_DECOMPOSED` can be injected via the `mockClaudeInvoker` exactly like any other marker. Testing Option B would require mocking `gh` CLI calls from inside Claude's session.

- **Auditability**: All board state changes go through `UpdateProjectItemStatus` in `engine/stages.go`, which is the single place the engine expresses board intent. Option B would have some board changes happen inside Claude's subprocess with no engine-level visibility.

### Why Done (not a new "Decomposed" column)

A new "Decomposed" board column would require every user to update their GitHub Project board before upgrading Fabrik. The engine's startup board validation would reject configs missing the column. The `fabrik:sub-issue` labels on children and the "Decomposed into #X, #Y" text in the parent body provide sufficient distinguishing context without a new column.

### Mutual exclusivity

`FABRIK_DECOMPOSED`, `FABRIK_STAGE_COMPLETE`, and `FABRIK_BLOCKED_ON_INPUT` are three mutually exclusive terminal outcomes. The engine checks them in priority order: `completed` (from `runClaude` itself), then `decomposed`, then `blockedOnInput`. Outputting more than one is undefined behavior and will result in whichever branch is checked first winning; the skill prompt must prevent this.

### Depth limit (max depth = 1)

Recursive decomposition (a sub-issue's Plan stage splitting into sub-sub-issues) is prevented by a skill-side check: if `fabrik:sub-issue` appears in the issue's labels, Plan skips decomposition and produces a normal plan. The engine does not enforce this gate — it trusts the skill prompt. This is a deliberate choice: the skill prompt is the appropriate place for this policy because it involves reading and interpreting a label, which is context available in the prompt but not in the engine's completion state machine.

### Completion label on decomposition

`handleDecomposed` adds `stage:Plan:complete` even though Plan did not produce a "plan" in the normal sense. This is intentional: the label prevents the engine from re-running Plan on the next poll after a crash or restart. Without it, a restart after `FABRIK_DECOMPOSED` was output but before `UpdateProjectItemStatus` fired would re-run Plan, potentially creating duplicate sub-issues. The skill's idempotency check (scanning for existing sub-issues before creating new ones) is the second line of defense.

## Consequences

- Any stage can technically output `FABRIK_DECOMPOSED` and have the engine move it to Done. By convention, only the Plan stage should do this. The skill prompt is the enforcement point.
- Sub-issues created by Plan inherit `fabrik:sub-issue` to prevent recursive splitting. The dependency gate infrastructure (ADR 016) naturally sequences them if `gh issue link --type blocks` creates the native GitHub `blockedBy` relationship.
- The `.fabrik-context/project.md` file (added in this same feature) provides Plan with the owner, repo, and project number needed to run `gh project item-add`. This is consistent with ADR 011 (context files as the delivery mechanism for runtime data).
