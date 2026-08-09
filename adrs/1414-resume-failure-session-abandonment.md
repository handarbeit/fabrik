# ADR 1414: Consecutive Resume-Failure Session Abandonment

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1414 — A failing `--resume` session is retried indefinitely with no reset path

## Context

When a `--resume` invocation fails, Fabrik retries by resuming **the same session** — including when
the session itself is the cause of the failure. A session that has grown too large to resume (or is
otherwise structurally broken) can never recover on its own: every retry re-triggers the identical
condition and, per Claude Code's own transcript handling, appends to the transcript that is already the
problem. The only existing session-pointer reset in the whole engine was a literal string match on
`"No conversation found with session ID"` (`engine/claude.go`) — a narrow, specific staleness signal
that does not fire for a merely-too-large or otherwise-broken session. Everything else fell through to
a generic error and re-saved the identical session id for the next attempt.

Reported live (#1383/#1386): an Implement stage failed at 85 turns with a ~3.0 MB transcript
(≈755k tokens); every subsequent retry died instantly at 1 turn / $0.0000. The reporter's manual
workaround — move the 36-byte `.session` pointer file aside so the next run starts cold — worked
precisely because the session-resolution code already treats an absent pointer as a cold start. This
issue generalizes that manual workaround into an automatic, bounded mechanism.

**The session pointer is shared by two invocation paths, not one.** `InvokeClaude` (the primary stage
run) and `InvokeClaudeForComments` (comment review) compute the identical session file path for a given
(issue, stage) — one `.session` file, two entry points. The stage path gates resume on
`!lastAttempt.IsZero()`; the comment path resumed unconditionally. A transcript poisoned by a long
stage run therefore also poisoned every subsequent comment-review invocation on that stage, and vice
versa — this is a per-(issue, stage) session problem, not a stage-path-only problem, and the fix's
counter must be shared across both paths or an alternating failure sequence would never reach threshold.

## Decision

### A durable, file-based sidecar counter — a deliberate divergence from every sibling counter's storage

Every existing bounded per-issue counter (`SliceRetries`, `ReviewCycles`, `RebaseCycles`,
`MaxCiFixCycles`, etc.) lives in `itemstate.Store`, which is confirmed **entirely in-memory**
(`itemstate.NewStore(nil)` is called fresh at every engine startup; no disk writes exist anywhere in
that package). This is fine for those counters — none of them are spec'd to survive a restart, and the
merge-train trial counter's restart-reset is explicitly documented elsewhere as acceptable — but this
issue has an explicit, spec-mandated restart-durability requirement: a restart must not silently reset
the loop and let the indefinite-retry bug resume.

The only mechanism already in this codebase durable across a restart for exactly this shape of
per-(issue, stage) state is a plain file colocated with the session pointer itself, written the same way
`saveSessionIDDirect` already writes the session id (`os.MkdirAll(0700)` + `os.WriteFile(0600)`). The
counter is therefore stored as `<Stage>.session.resumefails`, a plain-text integer, a sibling of
`<Stage>.session`. This is naturally shared across both invocation paths (and merge-train's conflict
resolution, which reuses `InvokeForComments`) because all three compute the identical `sessFilePath`
stem — the counter is keyed to the session-pointer identity, not the invocation path, exactly as the
issue's own Risks section required Research to state explicitly.

This is the one place this feature's shape diverges from its sibling counters: the `Config`/
`resolveInt`/flag+env tier (`MaxResumeFailures`, `--max-resume-failures`, `FABRIK_MAX_RESUME_FAILURES`,
default 2) is otherwise modeled directly on `MaxSliceRetries`/`MaxRebaseCycles`.

### `claudeTurnLimitError`-shaped, not `claudeUsageLimitError`-shaped

The existing "did-not-run" family (`claudeUsageLimitError`, `claudeAPIErrorExit`,
`apiKeyHelperDetectedError`) all represent invocations guaranteed to be near-zero-turn/cost, and their
handlers early-return in `finalizeStageOutcome` before `commitWIP`/push ever run. A resume failure has
no such guarantee — real work may have happened before the session broke mid-invocation. Mirroring the
did-not-run handlers exactly would silently drop uncommitted progress, contradicting this issue's own
"committed work is unaffected" framing (which only promises *committed* work is safe — an uncommitted
diff at the moment of failure is a different question the did-not-run family never has to answer,
because it is definitionally not reachable there). The correct precedent is instead
`claudeTurnLimitError`'s non-short-circuiting shape (ADR-1199): `commitWIP`, the branch push,
`markCommentsSeenByStage`, and `InvocationRecorded` all continue to run unconditionally for a resume
failure exactly as they do for any other incomplete run.

`claudeResumeFailureError{Cause, SessionID, ConsecutiveFailures, Threshold, Abandoned}` is a new sentinel
type, returned by `interpretClaudeResult` only from the generic-failure fallthrough — the point reached
once stale-session, turn-cap, usage-limit, and `api_error` have all already had their chance to classify
the exit and declined.

### Per-outcome classification, centralized inside `interpretClaudeResult`

| Outcome | Sidecar effect |
|---|---|
| Success (`runErr == nil`) | Reset to 0 |
| `FABRIK_STAGE_COMPLETE` found despite `runErr != nil` | Reset to 0 |
| Turn-cap exit (`*claudeTurnLimitError`) | Reset to 0 — real turns/cost is the strongest possible evidence the session is healthy |
| Usage-limit / `api_error` exit | Untouched — the stage never ran, says nothing about session health |
| Engine-shutdown exit (`ctx.Err() != nil && !wasTimedOut`) | Untouched |
| Generic failure, `resumeSessionID == ""` (a cold start itself failed) | Reset to 0; plain, unwrapped error returned — normal `MaxRetries` applies |
| Generic failure, `resumeSessionID != ""` | Increment; at or past threshold, abandon (see below). **Every** occurrence of this row returns `*claudeResumeFailureError`, not only the one that crosses the threshold |

Centralizing this inside `interpretClaudeResult` — the single funnel `InvokeClaude`,
`InvokeClaudeForComments`, and (transitively, via `resolveConflictWithClaude`) `merge_train.go`'s
conflict-resolution invocation already converge on — gets all three call sites the fix for free, the
same "centralize once, every caller inherits it" property ADR-1206 established for wall-time scaling.

### No explicit "force cold start" boolean

The issue's own text anticipated an explicit override ("it must also honor the cold-start override once
the threshold is hit"), but none was built: `resolveResumeSessionID` already treats an absent session
file as a cold start identically for both callers (logs a warning, returns `""`). Removing the file at
threshold *is* the cold-start signal, mechanically, for whichever invocation path runs next — guaranteed
sequential, never concurrent, by the existing per-issue single-worker lock. An explicit boolean would
have threaded one more field through both invocation paths to express a fact the file's own presence or
absence already expresses; file-removal-as-signal is provably equivalent and touches less code.

### Every resume failure up to threshold is exempted from `MaxRetries` — not only the abandoning one

`finalizeStageOutcome` (`engine/item.go`) computes `resumeFailed := errors.As(err, &resumeFailErr)`
alongside the pre-existing `turnLimited` check, and adds it as a third branch in the final escalation
block: `resumeFailed` skips `StageRetryIncremented` entirely, with no label and no comment applied.
`StageAttempted` still fires unconditionally for any invocation that ran (the existing `claudeRan` gate),
so the normal dispatch cooldown applies and the mechanism cannot hot-loop.

This exemption is not scoped to only the abandoning failure. If it were, a below-threshold resume
failure would still burn `MaxRetries` budget, and with `MaxResumeFailures` at or above `MaxRetries` the
issue could pause having never once reached the cold-start attempt this mechanism exists to produce —
reproducing, one layer up, the exact indefinite-retry bug this issue is about. The mechanism's entire
purpose is to *guarantee* a cold start; it cannot be starved by the very failures it is counting.

### `engine/comments.go` needs no exemption from the comment-processing circuit breaker

Unlike `MaxRetries` (a genuine failure escalation) and the account-wide usage-limit suspension (which
*is* excluded from the breaker, since it says nothing about a specific issue's comment thread), a resume
failure is specific to this issue's own session. It therefore counts toward `checkCommentBreaker`'s
"no forward progress" accounting exactly like `*claudeAPIErrorExit` already does (ADR-1458) — the
breaker is this path's only bound at all, and exempting a resume failure from it would leave the
comment-triggered dispatch path with no bound whatsoever if a session kept failing to resume.

### No issue comment, no durable label

This engine's existing convention is to comment when a human must act and log when the engine
self-heals. `fabrik:claude-limit` posts once per episode because the operator can act (wait, or clear
the suspension) and the engine cannot proceed unassisted. Discarding a poisoned session is the opposite
— a self-healing action whose entire purpose is that the *next* attempt succeeds without intervention.
The abandonment is logged (`claudeLog`, tag `resume`: session id, stage, consecutive-failure count,
threshold, and the last error) and nothing else. If the guaranteed cold-start attempt also fails, that
outcome carries no `--resume` session ID and is therefore a genuine, unexempted failure — it flows into
the normal `MaxRetries` → `stage:<name>:failed` → `fabrik:paused` path, which already comments. A
durable label was considered and rejected: the condition is transient by construction and clears within
one invocation, and a label here would be indistinguishable from the orphaned-durable-state leak #1135
and ADR-1183 exist to clean up.

### `#1199`'s mechanism does not fit — stated explicitly, as the issue requested

The issue asked to coordinate with #1199 (turn-cap preemption vs. `max_retries`) and reuse whatever
"attempt that should not be charged" mechanism it lands on. #1199's actual mechanism, `SliceRetries` in
`itemstate.StageState`, does not fit here for two independent reasons:

1. **Storage.** `itemstate` is confirmed non-durable; this issue's counter must survive a restart.
2. **Dimension.** `SliceRetries` counts how many time-slices a genuinely-progressing job has used — a
   question about a job's size relative to its turn budget. This issue's counter answers a completely
   different question: is this specific session's *content* still resumable. Conflating the two would
   let an unrelated legitimate multi-slice job's count bleed into session-health accounting, or vice
   versa.

The *shape* actually reused is the narrower `StageAttempted`-without-`StageRetryIncremented` exemption
pattern ADR-1119 and ADR-1458 established, combined with `claudeTurnLimitError`'s non-short-circuiting
handler treatment (ADR-1199) — not `SliceRetries` itself.

## Rationale

### Why a lower default (2) than every sibling counter (3–5)?

Every sibling counter's retries do something different on each attempt — a rebase re-runs against a
possibly-moved base, a review cycle addresses newly posted feedback. A resume retry is provably
identical: the same session id is re-resumed, re-appending to a transcript that is already the cause of
the failure. A second attempt is worth having, because a genuinely transient failure (network, a killed
subprocess) should not discard usable session context on the first blip. A third attempt is pure waste.
Discarding the session is also cheap and non-destructive — the work lives in the worktree and in git;
only conversation context is lost.

### Why consecutive, not windowed?

`MaxCommentCyclesPerWindow`'s time window exists because comment arrival is externally driven and
unbounded in time. Resume failures are engine-driven and strictly sequential — a windowed reset would
only add a way for the count to silently reset mid-episode, defeating the mechanism's purpose.

## Consequences

**Positive:**
- A session that has grown too large (or is otherwise structurally broken) to resume now recovers
  automatically within `MaxResumeFailures` attempts, without an operator having to manually move the
  `.session` file aside.
- The mechanism is shared across both invocation paths and merge-train's conflict resolution by
  construction (all three converge on `interpretClaudeResult`), so a session poisoned by one path is
  correctly abandoned for the other too — no separate fix was needed for the comment-review path beyond
  threading the config value through.
- The guaranteed cold-start attempt cannot be starved by `MaxRetries`, closing the exact starvation gap
  a naive "just charge it to max_retries" implementation would have reproduced one layer up.

**Negative / Trade-offs:**
- **`api_error` classification (ADR-1458) may intercept the exact failure signature this issue
  targets.** ADR-1458's own text notes no real captured sample of an `api_error` exit's `CostUSD`/
  `NumTurns` exists in this repository; if a future oversized/poisoned-session failure happens to
  present as `terminal_reason == "api_error"` with near-zero turns/cost (structurally plausible for a
  context-length-exceeded rejection), the already-shipped `classifyAPIErrorExit` check — which runs
  earlier in `interpretClaudeResult` than this mechanism — would exempt it before this issue's counter
  ever sees it, reproducing an unbounded retry loop via a different, newer code path. Out of this
  issue's scope to close (a separate, already-decided mechanism); named here as a residual risk.
- **Orphaned sidecar file after age-based session pruning.** The session janitor's exact `.session`
  suffix match never touches the `.resumefails` sidecar, so a nonzero leftover count can survive its
  parent session file's age-based prune, and the janitor's empty-dir cleanup then never fires (the
  directory is not actually empty). Low severity — a few bytes — and explicitly out of this issue's
  scope (janitor changes, ADR-1136).
- **`MaxRetries`'s effective bound is diluted for a genuinely-broken, non-session-related stage.**
  Because every resume failure up to `MaxResumeFailures` is exempted from `StageRetryIncremented`, a
  stage that fails on every invocation for an unrelated reason only has its cold-started attempts
  (roughly 1 in `MaxResumeFailures + 1`) counted toward `MaxRetries`, stretching the effective pause
  threshold by roughly that factor. With the shipped defaults (`MaxResumeFailures = 2`,
  `MaxRetries = 3`) this is a modest ~3x dilution, not an unbounded ceiling — a direct, accepted
  consequence of the "must not be starved" requirement, consistent with the same trade-off ADR-1119 and
  ADR-1458 already made for usage-limit and `api_error` exits respectively.
- Discarding conversational context on a large, legitimately-in-progress job may cause a stage to redo
  some work. Bounded by the small default threshold (2) to a last resort, not a routine occurrence.

## Explicitly Out of Scope

A pre-resume transcript-size guard (would require reading Claude Code's own on-disk transcript layout,
the exact coupling ADR-1136 rejected). Session retention/janitor age-based pruning policy (ADR-1136),
beyond noting the sidecar-orphaning residual risk above. Claude Code's own error text and
context-window/transcript behavior — upstream, not Fabrik's to fix. The comment-processing circuit
breaker's own scoping (#1413) — related containment, not superseded or duplicated by this issue's cure.

**References:** ADR-1119 (the `StageAttempted`-without-`StageRetryIncremented` exemption this mirrors),
ADR-1199 (confirmed not to fit — different storage and dimension, reasons stated above), ADR-1206 (the
"centralize once, every caller inherits it" precedent this issue's `interpretClaudeResult`-centered
design reuses), ADR-1458 (the `api_error` exemption whose comment-breaker-counts-it precedent this issue
follows for the comment path, and whose classification-ordering interaction is this issue's principal
residual risk), ADR-1136 (session janitor; the liveness-check coupling this issue's design stays clear
of), ADR-1183 (structural-only classification discipline, preserved here).
