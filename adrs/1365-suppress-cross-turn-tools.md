# ADR 1365: Suppress Cross-Turn Resumption Tools via `--disallowedTools`

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1365 — `--allowedTools` cannot withhold `ScheduleWakeup`/`Workflow` from headless stage workers, causing stalls and, for `Workflow`, wasted spend

## Context

Fabrik's headless stage workers are offered the full Claude Code harness tool set, including
`ScheduleWakeup` and `Workflow` — both of which promise **cross-turn resumption**: a wakeup fired
after a delay, or a `<task-notification>` delivered after a background workflow completes. Neither
promise can be kept in a headless Fabrik stage. There is no interactive session listening for a
timer or a notification; the turn simply ends, the stage exits without `FABRIK_STAGE_COMPLETE`,
retries, and — because the reasoning that led there is deterministic — re-derives the identical
stall on the next attempt until the issue exhausts its retries and pauses.

Field evidence from a downstream repo (`verveguy/liminis-context-graph`, `.fabrik/logs/`) recorded
**75 real `ScheduleWakeup` invocations**, delays 120s–1500s, with confirmed casualties across
multiple issues. See #1345 for the full breakdown.

### `--allowedTools` was the obvious fix, and does not work

`defaultAllowedTools` (`engine/claude.go`) governs `--allowedTools`, and it seemed reasonable to
assume that simply never listing `ScheduleWakeup`/`Workflow` there would keep them out of a stage
worker's hands. Empirical testing against the live `claude -p` CLI, using Fabrik's exact flags
(`--permission-mode dontAsk`, `--allowedTools Read --allowedTools Grep`), disproved this:

```
Read Bash Edit Glob Grep ReportFindings ScheduleWakeup Skill ToolSearch Workflow Write Agent
SCHEDULEWAKEUP=YES
```

`Bash`, `Edit`, `Write`, and `Agent` were all offered and callable despite being absent from the
two-entry allowlist. **`--allowedTools` is a call-time permission filter, not an availability
filter** — it can only be checked against once a call is already attempted, and (see the related,
independently-discovered #1372) that check is not even reliably enforced under `dontAsk` mode.
Either way, the tool is still present in the schema offered to the model, and a stage worker not
constrained by an intact permission check can still invoke it.

### `--disallowedTools` does work

The same probe, with `--disallowedTools ScheduleWakeup` added, returned `SCHEDULEWAKEUP=NO` — the
tool was absent from `system.init.tools` entirely. This was independently re-verified during
Research for #1365 against `--dangerously-skip-permissions` as well (the `fabrik:unrestricted`
path), confirming the exclusion holds on both invocation paths. `--disallowedTools` is a
**construction-time exclusion**: the tool is removed from the schema handed to the model, so there
is no permission check to reason around or bypass — the model never sees it.

## Decision

Add a named, commented package-level var, `disallowedTools` (`engine/claude.go`, alongside
`defaultAllowedTools`), and emit `--disallowedTools <tool>` once per entry, unconditionally, in
`buildClaudeArgs` — placed immediately after the `unrestricted`/`dontAsk` branch and before any
other flag, so it is structurally impossible for a future edit inside that branch to accidentally
scope it to one path:

```go
var disallowedTools = []string{
    "ScheduleWakeup", // cross-turn resumption; no scheduler exists in a headless stage
    "Workflow",       // cross-turn resumption + background spend against session budget
}
```

Only `ScheduleWakeup` and `Workflow` are suppressed. `Agent` is explicitly excluded (see FR-3
below), and `Bash(run_in_background)` cannot be addressed by this mechanism at all (see FR-4).

### Why `Workflow` specifically, and why it's worse than `ScheduleWakeup`

`Workflow` returns immediately with a task ID and promises a `<task-notification>` on completion,
after potentially spawning dozens of subagents against the session's own token budget. Its failure
mode is therefore the stall *and* the spend — tokens consumed by subagents whose result is never
seen, on top of the same dead-end retry loop `ScheduleWakeup` produces.

### Why `Agent` is not suppressed (FR-3)

`Agent`/`Task` (the harness's user-facing name differs from `defaultAllowedTools`'s existing
`"Task"` entry — see Risks) completes synchronously within the parent turn. A subagent invocation
blocks until it returns; there is no cross-turn promise being made, so it presents none of the
failure mode this ADR addresses. Subagents are legitimately useful to stage workers and removing
them would be a regression, not a fix. If `Agent` is later found reachable in some background mode
from a stage, that is a distinct problem requiring separate investigation — not an extension of
this suppression list.

### Why `Bash(run_in_background)` is out of scope (FR-4)

`--disallowedTools` operates on whole tool names. `Bash` cannot be partially disallowed by argument
shape — suppressing it to block `run_in_background` would remove `Bash` entirely, which every stage
depends on for `git`, `gh`, build tooling, and more. This gap is tracked by a sibling stage-prompt
guidance issue and is deliberately not closed here; this ADR's mechanism structurally cannot reach
it.

### Relationship to #1372

#1372 independently found that `--allowedTools` under `dontAsk` does not reliably enforce *any*
restriction — a non-allowlisted `Bash` command can execute, and the worktree boundary
(`applyWorktreeBoundary`, ADR-049) can be breached. That finding is about the *allow*-list's
call-time enforcement gap. It does not undermine this ADR's fix: `--disallowedTools` sidesteps that
failure mode entirely because there is no call-time check to bypass in the first place — the tool
was never in the offered schema. The two issues address different mechanisms and neither issue's
resolution depends on the other's.

## Consequences

- Every headless Fabrik stage invocation, on both the `--permission-mode dontAsk` and
  `--dangerously-skip-permissions` (`fabrik:unrestricted`) paths, no longer offers `ScheduleWakeup`
  or `Workflow` to the model. The corresponding class of stall (and, for `Workflow`, wasted spend)
  is eliminated at the source rather than mitigated after the fact.
- `--allowedTools`/`stage.AllowedTools`/`applyWorktreeBoundary` are untouched — this is a purely
  additive, independent flag emission with no interaction with existing tool-restriction logic.
- A future contributor extending tool restrictions should reach for `--disallowedTools` (a named
  var, following this pattern) rather than assuming `--allowedTools` omission is sufficient — that
  assumption is exactly what made this bug invisible for as long as it was.
- `Bash(run_in_background)` remains a live gap, tracked separately. Do not treat this ADR as having
  closed it.

## Related

- #1345 — original diagnosis and the 75-invocation field evidence.
- #1372 — related but distinct `--allowedTools`/worktree-boundary enforcement gap under `dontAsk`;
  not fixed by, and does not undermine, this change.
- ADR-049 — worktree boundary enforcement via `--allowedTools` path-scoping; unaffected by this ADR.
