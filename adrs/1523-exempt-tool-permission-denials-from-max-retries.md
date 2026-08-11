# ADR 1523: Exempt Tool-Permission Denials from `max_retries`

**Date**: 2026-08-11
**Status**: Accepted
**Issue**: #1523 — A blocked worker is not a failed stage

## Context

On 2026-08-09, a Claude Code profile misconfiguration made every mutating tool (`Edit`, `Write`, any
write-capable `Bash`) return a permission denial to Fabrik's stage workers, while read-only operations
kept working. Workers reported it verbatim: "Permission to use `<Tool>` has been denied because Claude
Code is running in don't ask mode." Four issues hit this in a ~30-minute window, and the engine responded
to the identical condition two incompatible ways, decided entirely by whether the worker happened to also
emit `FABRIK_BLOCKED_ON_INPUT`:

| issue | stage | engine response |
|---|---|---|
| #1453 | Validate | `FABRIK_BLOCKED_ON_INPUT` → `fabrik:awaiting-input`, paused cleanly |
| #1498 | Implement | `FABRIK_BLOCKED_ON_INPUT` → `fabrik:awaiting-input`, paused cleanly |
| #1456 | Implement | retried 3×, `stage:Implement:failed`, `fabrik:paused` |
| #1462 | Implement | retried 3×, `stage:Implement:failed`, `fabrik:paused` |

#1456 and #1462 each burned their entire `max_retries` budget on an environmental fault no retry could
ever fix, then marked themselves `stage:<name>:failed` — a durable claim that *the work* was wrong, when
the machine had simply been unable to write files. Recovery required manual label surgery, because the
retry counter lives in `itemstate` (in memory); clearing `fabrik:paused` alone does not reset it.

Fabrik already classifies exactly this shape of condition correctly, twice: `fabrik:claude-limit`
(ADR-1119, ADR-1183) and `fabrik:api-key-helper-detected` (ADR-1346, R13) both record `StageAttempted`
without `StageRetryIncremented`, apply neither `stage:<name>:failed` nor `fabrik:paused`, and post one
explanatory comment per episode. A blanket denial of every mutating tool belongs in that category: the
stage did not fail, it was never able to start doing the work it was asked to do.

## Decision

### Detection: a structural signal, not the two-decade-old prose-matching mistake

Research empirically captured the CLI's actual behavior against the installed version (`2.1.227`) using
Fabrik's own invocation flags (`--output-format stream-json --verbose --permission-mode dontAsk`, no
`--dangerously-skip-permissions`). A tool call denied via a `PreToolUse` hook produces this shape on the
terminal NDJSON `result` line:

```json
{
  "is_error": false,
  "subtype": "success",
  "terminal_reason": "completed",
  "permission_denials": [
    {"tool_name": "Write", "tool_use_id": "toolu_...", "tool_input": {...}}
  ],
  "result": "The write was denied — Claude Code is running in \"don't ask\" mode, ..."
}
```

`permission_denials` is a genuine structural array field on the same result object `claudeResponse`
already unmarshals — it was simply undeclared and silently dropped by `encoding/json` until this change.
Unlike `"blocking_limit"`/`"api_error"`, there is **no dedicated `terminal_reason` enum value** for a
permission denial — `is_error` is `false`, `subtype` is `"success"`, `terminal_reason` is `"completed"`,
identical to an ordinary successful run. The only structural signal is a non-empty `permission_denials`
array, so `classifyToolsDenied(resp claudeResponse) (toolNames []string, detected bool)` is:

```go
func classifyToolsDenied(resp claudeResponse) (toolNames []string, detected bool) {
    if len(resp.PermissionDenials) == 0 {
        return nil, false
    }
    // dedupe resp.PermissionDenials[].ToolName, preserving first-seen order
    ...
}
```

This evidence is first-hand and version-pinned, which is stronger footing than ADR-1183 had for
`"blocking_limit"` (sourced only from the SDK's published TypeScript types/changelog, "not from a
Fabrik-captured real occurrence" — ADR-1183's own "Unverified exact value" trade-off). A blanket
`--allowedTools`/`--disallowedTools` mismatch does **not** reproduce a denial in this CLI version: a tool
merely absent from `--allowedTools` is silently invoked anyway under `dontAsk` (the already-documented
#1372 caveat), and a tool named in `--disallowedTools` is removed from the tool list entirely — the model
self-reports it unavailable, with no `permission_denials` entry. Only an explicit `PreToolUse` hook (or,
by strong inference, an equivalent `permissions` "ask" rule with no interactive prompt available) produces
a genuine runtime denial. This means the real incident's misconfiguration most plausibly involved a hook
or an org/user-level `permissions` "ask"-tier rule, not a simple `--allowedTools` omission — worth noting
here, though root-causing the incident itself is out of scope (see Explicitly Out of Scope).

**Gated on `!completed`, in the clean-exit path only.** `interpretClaudeResult` (`engine/claude.go`)
consults `classifyToolsDenied` only when `runErr == nil` (a clean process exit) and
`!stageCompleteRE.MatchString(text)`. A denial the model worked around and still completed the stage is
ordinary success — every incident report and this ADR's own reproduction match this shape exactly. Gating
on `!completed` also means the classifier touches only the existing clean-exit return, not any other
branch of `interpretClaudeResult`. Detection is deliberately scoped to the clean-exit path only, per the
evidence above — a diagnostic-only log line records a `permission_denials` array seen on a non-clean
exit (a shape not yet observed) for future evidence, never as a classification, mirroring the
unmatched-`terminal_reason` diagnostic ADR-1183 established.

### Handling: continue-processing, not the short-circuit shape R2's wording echoes

R2's language ("record `StageAttempted` but not `StageRetryIncremented`... do not apply
`stage:<name>:failed`... do not apply `fabrik:paused` as the first response") reads almost verbatim like
the usage-limit/`api_error`/`apiKeyHelper` "did-not-run" family, all three of which short-circuit
`finalizeStageOutcome` with an early return — no `commitWIP`, no push, no `markCommentsSeenByStage`,
because in every case the invocation aborted at or near zero real work.

A tool-permission denial is structurally different: the CLI exits cleanly, and the denial can occur
*after* real, already-committed-worthy work (the incident's own worker reports show investigative and
editing work happening before the denial). Short-circuiting here would silently discard that work purely
because a *later* tool call in the same invocation was denied — a data-loss risk none of the three
existing precedents carry. `*claudeToolsDeniedError` therefore follows `claudeTurnLimitError`/
`claudeResumeFailureError`'s **continue-processing** shape instead: `toolsDenied :=
errors.As(err, &toolsDeniedErr)` is computed alongside the pre-existing `turnLimited`/`resumeFailed`
checks in `finalizeStageOutcome`, and `commitWIP`, the branch push, `markCommentsSeenByStage`, and
`InvocationRecorded` all still run exactly as they do for any other incomplete run. Only
`StageRetryIncremented` and the `stage:<name>:failed`/`fabrik:paused` application in the final escalation
branch are diverted.

This resolves the one open question Research flagged (short-circuit vs. continue-processing) in favor of
data preservation, at the cost of R2's wording being satisfied in spirit (the retry exemption, the label
avoidance) rather than by literal reuse of the short-circuit handlers' code shape.

### R6 for free: no explicit marker-suppression code

Because detection is expressed as a non-nil `error` returned from `interpretClaudeResult` — not a
separately-threaded boolean — `blockedOnInput := err == nil && CheckBlockedOnInput(output)`
(`engine/item.go`) is structurally unreachable once a denial is classified. Whether the worker's output
also contains `FABRIK_BLOCKED_ON_INPUT` cannot change the outcome, because the code path that would act on
that marker is gated on `err == nil`, and `err` is never nil for a detected denial. No ordering code, no
"ignore the marker if X" conditional exists anywhere — the asymmetry that let #1453/#1498 recover cleanly
while #1456/#1462 burned their retry budget on the identical condition is closed by construction, not by
a rule someone has to remember to apply consistently.

### A distinct, independently-bounded counter — not `MaxRetries`

`ToolsDeniedRetries`/`MaxToolsDeniedRetries` mirrors `SliceRetries`/`MaxSliceRetries`'s (#1199) shape
exactly: a separate `map[string]int` in `itemstate.StageState`, its own mutation
(`ToolsDeniedRetryIncremented`), its own pause path (`pauseForToolsDeniedLimit`, modeled directly on
`pauseForSliceLimit`), and — critically — **no `stage:<name>:failed` even at the bound**. The condition
is never treated as a stage failure, because it never was one; the bound exists solely so an unwatched,
never-fixed permission misconfiguration cannot retry forever with nobody watching (R5).

`MaxToolsDeniedRetries` defaults to **3**: lower than `MaxSliceRetries` (10 — a turn-cap preemption is
routine and self-resolving by construction, a large job legitimately needing several slices), since a
permission misconfiguration does not resolve itself the way slicing does — no amount of retrying fixes a
broken permission profile. Higher than `MaxResumeFailures` (2 — a resume failure is provably identical
every time, so a second attempt tolerates a transient hiccup and a third is pure waste), since the
explanatory comment already reaches the operator on the very first detection (R4); the extra cycles before
`pauseForToolsDeniedLimit` fires guard only against a single spurious/flaky denial, never against
expecting a retry to fix anything on its own.

### Label: `fabrik:tools-denied`, self-resolving per-worktree

R3 asked for a label "in the shape of `fabrik:tools-denied`," applied on detection and cleared on the
next invocation not classified this way, "mirroring `fabrik:api-key-helper-detected`'s per-worktree
self-resolution rather than `fabrik:claude-limit`'s account-wide settle sweep." This is a straightforward
choice, not a close call: a permission misconfiguration is inherently local to one worktree's environment
(the specific `.claude/settings.json`, hook, or org/user-level rule affecting that invocation), unlike a
genuine Claude account usage limit, which is account-wide by definition. No settle sweep is added; the
label clears the same way `fabrik:api-key-helper-detected` does — on the issue's own next invocation that
is not itself classified this way.

**The clear is gated on `!toolsDenied`, unlike its two siblings.** `fabrik:claude-limit` and
`fabrik:api-key-helper-detected` are cleared unconditionally at the same site in `finalizeStageOutcome`,
because their own detection short-circuits — the code path that would clear the label is simply never
reached in the same invocation that sets it. `fabrik:tools-denied` does *not* short-circuit, so the
generic "clear the label whenever we get past the did-not-run block" idiom would immediately erase the
label the escalation branch is about to apply, in the very same invocation. The clear condition is
therefore `!toolsDenied && hasLabel(...)` — the one piece of code in this change that cannot simply copy
its two siblings' shape verbatim, because the shape they copy from (short-circuit) is not the shape this
condition uses.

## Rationale

### Why not fold `toolsDenied` into the existing `turnLimited`/`resumeFailed` handling instead of a new branch?

`turnLimited` and `resumeFailed` are each already exempted from `StageRetryIncremented`, so a
minimal-diff instinct is to simply add `toolsDenied` as a third condition to the existing `else if
resumeFailed` branch. Rejected: `resumeFailed` deliberately applies **no label and no comment** (a
self-healing condition — see `resumeFailErr`'s doc comment), while R3/R4 explicitly want both a label and
a once-per-episode comment for tools-denied. Folding the two together would either force a label/comment
onto every resume failure (contradicting #1414's design) or require a conditional inside that branch to
suppress them for one sub-case — reproducing exactly the "a runtime conditional someone has to remember"
coupling risk ADR-1458 rejected for a structurally similar question. A distinct branch keeps each
condition's blast radius exactly as narrow as it needs to be, verified by the compiler via the Go type
`errors.As` dispatches on, not by a flag threaded through a shared branch.

### Why not reuse `api_error`'s "fifth, more minimal shape" (no label, no comment)?

ADR-1458 explicitly chose no label/no comment for `api_error` because that condition is transient,
per-invocation, and self-resolving by construction — the *next* attempt succeeding is the entire point,
and a label would be indistinguishable from ADR-1183's orphaned-durable-state problem. A tool-permission
denial is different in the dimension that matters: it does **not** self-resolve on its own. The underlying
cause (a hook, an "ask" rule) persists across every retry until a human changes the permission
configuration outside Fabrik's control — the four workers in the original incident diagnosed this
correctly in prose every single time, and R4 exists specifically so that diagnosis reaches the operator
once, not silently on every attempt with no durable trace. `api_error`'s minimal shape is the wrong
sibling to copy here; `fabrik:claude-limit`/`fabrik:api-key-helper-detected`'s fuller shape (label +
comment + clear-on-resolution) is the correct one, precisely because both of those conditions also require
a human or an external system to change something before the next attempt can succeed.

### Why is the counter independent of `SliceRetries`/`Attempts`, even though a stage could in principle hit multiple bounds?

`ToolsDeniedRetries`, `SliceRetries`, and `Attempts` are all counters on the same `(issue, stage)` key,
tracking three conditions that are mutually exclusive per-invocation (a single invocation is classified as
at most one of turn-capped, tools-denied, or a genuine failure) but not mutually exclusive across
invocations — a stage could hit a turn cap on one attempt and a tools-denied exit on the next. No special
interaction is needed: whichever counter reaches its own threshold first escalates via its own pause path,
and `StageRetryCleared` (fired by a manual unpause via `clearFailedStage`) resets all three together, so
there is no double-bookkeeping and no conflicting state between them.

## Consequences

**Positive:**
- A tool-permission denial no longer consumes `max_retries` budget, and no longer applies
  `stage:<name>:failed` — an operator reading the board can now distinguish "the change is wrong" from
  "the machine could not write files."
- The exact incident this issue is named for cannot recur: whether a worker emits `FABRIK_BLOCKED_ON_INPUT`
  can no longer change the outcome, because the engine's own structural classification governs it, not a
  worker-side courtesy.
- No committed work is discarded by a late-invocation denial, unlike a naive short-circuit
  implementation would have caused.
- `MaxToolsDeniedRetries` bounds the condition independently, so a persistently broken permission
  configuration eventually surfaces for human review instead of retrying forever unwatched.

**Negative / Trade-offs:**
- Detection is scoped to the clean-exit path only, per the evidence. If a future CLI version reports
  `permission_denials` on a non-zero-exit result too, that shape goes undetected until the diagnostic log
  line surfaces it in practice — a silent false negative (falls back to today's pre-fix, misclassified
  behavior for that shape specifically), never a new false-positive class. Mirrors ADR-1183's own accepted
  trade-off for `"blocking_limit"`.
- The `permission_denials` schema is not officially documented by Anthropic beyond this ADR's own
  empirical capture. If a future CLI version renames or restructures the field, the same silent-false-
  negative failure mode applies.
- `MaxToolsDeniedRetries`'s default of 3 is a judgment call with no prior incident data on how many
  retries is "enough to rule out a flaky denial" — easy to revisit via `--max-tools-denied-retries`/
  `FABRIK_MAX_TOOLS_DENIED_RETRIES` without a code change, but not empirically derived.

## Explicitly Out of Scope

Diagnosing or fixing the 2026-08-09 profile misconfiguration itself — resolved, and not an engine concern.
Any change to `--permission-mode`, `--allowedTools`, or `--setting-sources` (that Fabrik never passes
`--setting-sources` while Pruefer does, deliberately, is worth its own investigation — not this issue).
Changing `max_retries` defaults or the retry machinery generally.

**References:** ADR-1119 (the `StageAttempted`-without-`StageRetryIncremented` precedent this mirrors),
ADR-1183 (structural-only classification discipline, and the origin of the diagnostic-log-for-unobserved-
shapes idiom this reuses), ADR-1346 R13 (`fabrik:api-key-helper-detected`, the closer sibling for label
clearing behavior), ADR-1458 (`api_error`'s minimal shape — the sibling deliberately *not* copied here,
and the precedent for "a distinct sentinel type, not a reused one, to avoid coupling unrelated
behaviors"), ADR-1199/#1199 (`SliceRetries`/`MaxSliceRetries`, the structural precedent for an
independently-bounded, non-failure counter), ADR-1414 (`ResumeFailureError`'s continue-processing,
non-short-circuiting shape, mirrored here for the same "real work may precede the failure" reason).
