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

Only `ScheduleWakeup` and `Workflow` were suppressed at the time this ADR was accepted; `Monitor`
and `CronCreate` were added by the #1558 amendment below. `Agent` is explicitly excluded (see FR-3
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

## Amendment (2026-08-12, #1558): `Monitor`, `CronCreate`, and a stated membership test

`disallowedTools` was accepted above as two hand-enumerated entries under a category described only
in prose ("harness tools that must never reach a headless Fabrik stage worker"). #1558 found a third
member — `Monitor` — reaching a real production stage worker and being denied, causing both a lost
invocation (26 turns, $5.95) and a spurious `max_retries` exemption (see #1556: a denial that isn't
causally related to the stage's own lack of progress still triggers the exemption `classifyToolsDenied`
grants). Per this repo's own "enumerate a category by hand, miss a member" failure shape (also seen at
#1539), the fix adopted here is not just adding `Monitor`, but stating a membership test precise
enough that a future contributor can classify a new tool without re-deriving the rationale, and
sweeping the current tool catalogue against it once.

### R2: the membership test

**A tool belongs on `disallowedTools` iff its own contract requires delivery to a future turn,
session, or external callback that a single headless invocation cannot provide** — no interactive
session persists past the current invocation to receive that delivery, so the promise can never be
kept and the turn either stalls (nothing delivers the result) or is denied outright.

This is a **contract test, not a reachability test**: whether a tool happens to be visible in any
particular probe's tool catalogue (see the reachability caveat below) is irrelevant to whether it
belongs on the list. It is also a test of the tool's *own* delivery obligation, not of whether some
*other* party's future action is undeliverable — see the `SendMessage` rejection below for the
distinction this draws.

### R3: sweep result

Every tool discoverable in the harness catalogue available to a Research/Plan/Implement stage
session was classified against the R2 test:

**Added:**

| Tool | Contract | Why it fails the R2 test |
|---|---|---|
| `Monitor` | Watches a condition over time and reports back "on its own schedule" — explicitly not a same-turn reply. | No future turn exists in a headless stage to receive that report. Production-confirmed reachable and denied (#1450 log). |
| `CronCreate` | Enqueues a prompt for a future time; fires "only while the REPL is idle." | Structurally identical to `ScheduleWakeup`, recurring instead of one-shot — a headless stage is never later idle to receive the fire. |

**Considered and rejected** (so a future reader can tell "deliberately excluded" from "never
examined"):

| Tool | Contract | Why it passes (does *not* belong on the list) |
|---|---|---|
| `CronDelete`, `CronList` | Synchronous same-turn CRUD against the cron job store. | No delivery promise of their own; inert without `CronCreate`. |
| `SendMessage` | The send call itself completes synchronously/fire-and-forget within the turn. | The undeliverable half — an incoming reply — is a promise made by the *recipient's future turn*, not by this tool's own contract. Also has legitimate one-way uses (notify, no reply expected) the "don't remove on suspicion" constraint protects. |
| `TaskOutput` | Synchronous, wall-clock-bounded poll (`block`/`timeout`) of a background task. | Does not require future delivery — this is the in-turn polling pattern Fabrik's own stage skills already prescribe for handling backgrounded work. |
| `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`, `TaskStop` | Task-list tracking and synchronous background-task management. | No cross-turn delivery contract. (`TodoWrite`/`Task` availability questions are #1340, unrelated to this test.) |
| `EnterWorktree`, `ExitWorktree` | Synchronous session/directory management. | Resolves within the same turn. |
| `NotebookEdit`, `WebFetch`, `WebSearch` | Synchronous request/response. | Resolves within the same turn. |
| `DesignSync`, `PushNotification`, `RemoteTrigger` | Synchronous request/response against external (claude.ai) services. | None promise delivery back to *this* invocation in a future turn; `RemoteTrigger`'s `run` action fires an entirely separate remote session, not a continuation of the caller's own turn. |
| `ListAgents`, `ReportFindings`, `ToolSearch` | Synchronous, same-turn list/report/lookup calls. | Return immediately; no delivery promise. (`ListAgents` is newly catalogued since this ADR's original acceptance — noted here as examined-and-rejected, not an oversight.) |
| `mcp__claude_ai_*` connector tools (e.g. Google Calendar) | Per-account MCP connector tools tied to an interactive claude.ai session/surface. | Not applicable to this catalogue — no MCP server is configured for Fabrik's headless workers, so these are never offered to a stage invocation in the first place. |
| `Agent`, `Bash(run_in_background)` | — | Already addressed above (FR-3, FR-4); re-confirmed, not re-litigated by this sweep. |

### R4: empirical basis, extended

The original empirical claim (Context section above, CLI 2.1.227/2.1.228: a tool named in
`--disallowedTools` is absent from `system.init.tools` and yields `permission_denials: []` on a
non-completing run) was re-verified during #1558 on CLI **2.1.229**, using the same probe
methodology (`claude -p --permission-mode dontAsk --allowedTools Read --allowedTools Grep`, prompted
to enumerate its own tool schema): adding `--disallowedTools ScheduleWakeup --disallowedTools
Workflow` removed both from the enumerated list and produced `permission_denials: []`, on both the
`dontAsk` and `--dangerously-skip-permissions` paths. This is now confirmed across three CLI versions
(2.1.227, 2.1.228, 2.1.229).

**Monitored assumption, not a proven invariant**: this entire mechanism depends on the external CLI
continuing to respond to `--disallowedTools` with schema omission rather than a runtime denial. It
cannot be unit-tested (external binary behavior). If a future CLI version reports `--disallowedTools`
blocks as `permission_denials` entries instead of omitting the tool, this fix silently stops working
for the "no denial reaches the model" half of its guarantee — the tool would still be unavailable,
but `classifyToolsDenied`'s exemption could fire again. #1556 (narrowing that exemption's causality)
remains on the roadmap for exactly this reason; it is complementary to this ADR, not superseded by it.

**Reachability/catalogue-composition caveat**: the #1558 re-probe also found the tool catalogue
visible to a probe session is *not* determined by CLI version alone — `Monitor` did not appear in the
2.1.229 probe run from a developer machine at all, despite being confirmed reachable from a real
Fabrik production stage worker in the same timeframe (the #1450 log). Catalogue composition appears
to depend on account/entitlement or invocation-surface factors beyond CLI version. This means the R3
sweep's "considered and rejected" table above is exhaustive against the *documented* tool contracts
available to a Research/Plan/Implement session, not empirically proven exhaustive against every
possible production invocation surface. `Monitor`'s own inclusion rests on the #1450 production log,
not on any probe reproducing its reachability.

## Related

- #1345 — original diagnosis and the 75-invocation field evidence.
- #1372 — related but distinct `--allowedTools`/worktree-boundary enforcement gap under `dontAsk`;
  not fixed by, and does not undermine, this change.
- ADR-049 — worktree boundary enforcement via `--allowedTools` path-scoping; unaffected by this ADR.
- #1558 — added `Monitor`/`CronCreate`, stated the R2 membership test, and swept the tool catalogue
  against it (see Amendment above).
- #1556 — narrows the tools-denied `max_retries` exemption's causality; complementary to, not
  superseded by, this amendment.
- #1523 — the tools-denied exemption mechanism whose spurious firing on `Monitor` this amendment
  removes at the source.
