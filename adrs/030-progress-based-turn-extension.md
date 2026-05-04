# ADR 030: Progress-Based Turn Extension

**Status**: Accepted  
**Date**: 2026-04-24

## Context

Some issues legitimately require more turns than the stage YAML `max_turns` default: large implementations, complex reviews with many reviewer threads to resolve, or multi-faceted validation tasks. The previous behavior was unconditional: once `max_turns` was hit, the stage failed and entered the cooldown/retry cycle regardless of whether the stage was actually stuck or just doing legitimate work.

The symptom-level fix — bumping `max_turns` globally in the stage YAML — blunts the budget signal for all issues. A per-issue label (`fabrik:extend-turns`) was also considered as the primary mechanism, but a label that requires human intervention for every long-running stage is error-prone and adds operational friction.

The root gap is that the engine had no way to distinguish "legitimately busy" from "genuinely stuck." Observable GitHub and git signals exist for most stages that would allow the engine to make this determination automatically.

## Decision

Add an **in-dispatch extension loop** to `processItem` that wraps the single `e.claude.Invoke()` call. When Claude exits due to `max_turns`, the engine:

1. Checks whether observable progress has been made since the baseline snapshot taken before the first invocation.
2. If progress is detected → extend by one additional `stage.MaxTurns` quantum (re-invoke with `--resume`).
3. If no progress → treat as turn-limit failure (same behavior as before).
4. Hard cap: 3× `stage.MaxTurns` total across all invocations.

Progress signals are per-stage and observable without parsing Claude's output stream:

| Stage | Signal |
|-------|--------|
| Implement | New git commit on `fabrik/issue-N` (HEAD SHA changed) |
| Review | New git commit OR resolved reviewer thread count increased |
| Validate | Total comment count on issue or linked PR increased |
| All others | No signal — always fail on turn-limit |

A `fabrik:extend-turns` label is retained as a **manual safety valve**: when present, the first invocation gets 2× the normal budget without requiring a progress check. Subsequent extensions beyond 2× still require progress. ~~The label is auto-removed on successful completion.~~ See Amendment below.

## Why In-Dispatch (Not a Catch-Up Loop)

Three existing re-invocation patterns (review reinvoke, CI-fix reinvoke, rebase reinvoke — ADRs 026–028) all operate across poll cycles via the catch-up loop. The extension loop is architecturally different:

- **Output must be accumulated before posting.** Each `--resume` invocation returns only its delta output. Posting after each invocation would produce multiple fragmented stage comments, confusing the issue thread. The extension loop must accumulate all output in memory before posting a single `🏭 Fabrik — stage: <Name>` comment.
- **WIP commit and push must be deferred.** Committing a WIP in the middle of a multi-invocation stage would create misleading commits and corrupt the progress baseline (since new commits would look like progress on the next extension check). The loop defers `commitWIP` and `PushBranch` to after all invocations complete.
- **No poll-cycle gap.** The catch-up loop approach would introduce a cooldown period between extensions, increasing latency and complexity. An in-dispatch loop extends within the same goroutine, with no gap.
- **No goroutine dispatch.** The catch-up loop spawns async goroutines. Extensions do not require goroutine dispatch — they are sequential within `processItem`.

These constraints make the in-dispatch loop the right primitive for turn extension.

## Why In-Memory Baseline (Not Persisted)

The baseline (git HEAD SHA, comment count, resolved thread count) is captured in a local struct within `processItem` and is lost on engine restart. Alternatives considered:

- **`.fabrik-context/` file**: Survives restarts but adds file I/O, introduces a write-before-invoke ordering constraint, and would need cleanup logic. The restart-gap risk (a fresh baseline after restart grants extensions that wouldn't have been granted with full history) is rare and bounded: the engine would simply grant an extension if any progress exists since the fresh baseline.
- **Issue body annotation**: Too visible and noisy for users; creates merge conflicts.
- **Engine state file**: Adds persistence infrastructure for a single struct.

In-memory is simpler and the restart-gap risk is acceptable.

## Why Implement Uses Git Commits (Not Checkbox Counting)

The spec mentioned checkbox counting (`- [x]`) as a progress signal for the Implement stage. Research found that `stage-Plan.md` (the canonical plan document) is written once by `writeContextFiles` before the Implement stage starts and does not change during the Implement run. Checked boxes accumulate in the file only when the engine re-writes context files for a new invocation, not mid-run.

Git commits are the right signal for Implement: they reflect actual durable work done in the worktree.

## Why `fabrik:extend-turns` Is Retained

Automatic progress detection may fail in edge cases:
- The Review stage may have resolved threads before the engine re-fetches (race).
- The Validate stage may have comments that are not visible in the FetchItemDetails response for timing reasons.
- Progress detection for Review/Validate has a one-GraphQL-call overhead per turn-limit hit, which may have transient failures.

The `fabrik:extend-turns` label provides a bypass that does not depend on progress detection. It is a safety valve, not the default path.

## Consequences

- Stages with measurable progress no longer fail spuriously at `max_turns`.
- The hard cap (3×) bounds runaway extension.
- Per-stage comments are clean: one comment per stage invocation (from all extensions combined).
- The `fabrik:extend-turns` label is seeded by `SeedLabels` at engine startup.
- Review and Validate progress detection costs one GraphQL call per turn-limit hit — bounded and acceptable.
- Plan, Research, and Specify always fail on turn-limit (no observable progress signal within a dispatch).
- ADRs 026–028 (reinvoke patterns) are unaffected; extension is orthogonal to those patterns.

## Amendment — 2026-05-04 (issue #530)

**`fabrik:extend-turns` lifecycle changed from single-use to persistent.**

The original decision described the label as auto-removed on successful stage completion so that "the next stage gets a normal budget." In practice this required the operator to time the label precisely — applying it too early meant an unneeded stage consumed it, while applying it too late meant the offending stage had already overrun. This produced repeated operational dances (pause → re-apply → unpause) across issues #516–#519.

**New behavior:** The label persists across all intermediate stages and is removed only when the Done stage's `CleanupWorktree` branch runs (or when the operator removes it manually). Every stage invocation that runs while the label is present receives the 2× budget pre-grant. The 2×→3× progressive extension within a stage is unchanged. No-op when `max_turns == 0` remains unchanged.

The removal site moved from `processItem`'s `completed` block to the `CleanupWorktree` branch in `engine/item.go`. The as-built docs (`docs/state-machine.md`, `docs/stage-lifecycle.md`, `CLAUDE.md`, `docs/USER_GUIDE.md`) reflect the new semantics.
