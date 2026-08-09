# ADR 1458: Transient `api_error` Exit Exemption from `max_retries`

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1458 — A transient `api_error` exit is charged against `max_retries` and pauses the issue, though the stage never ran

## Context

A Claude invocation that exits with `terminal_reason="api_error"` never ran the stage — the CLI's own
result object records 0-1 turns and `$0.0000` cost — yet before this change it was charged against
`max_retries` exactly like a genuine stage failure, and three of them paused the issue. Observed live on
2026-08-08, 21:19–21:32 UTC: eight such exits across two issues (#1408, #1208), every one at 1 turn and
`$0.0000`. #1408 was paused with `stage:Validate:failed` on a PR that was `MERGEABLE`/`CLEAN` with all
six checks green, and stayed paused for roughly an hour with a dependent chain blocked behind it. No
Claude invocation ran anywhere on the board for ~50 minutes, because both active lane heads were paused.
Nothing was wrong with either issue's work — the transient was Anthropic-side.

`engine/claude.go`'s structural classification (ADR-1183) recognized exactly one `terminal_reason`
value, `"blocking_limit"` (a genuine account usage limit). Everything else, including `api_error`, fell
through a diagnostic-only log line to the generic error path, reaching `StageRetryIncremented` and, at
`MaxRetries`, `stage:<name>:failed` + `fabrik:paused`.

This issue is explicitly modeled on ADR-1119's precedent (`StageAttempted`-without-
`StageRetryIncremented`), which #1119 established for a genuine usage-limit exit and which
`apiKeyHelperDetectedError`/`handleAPIKeyHelperDetected` (R13) later reused for a second, unrelated
condition. The ask was not to loosen classification generally — ADR-1183's structural-only discipline
(never output prose) had to be preserved — but to add one further narrow structural signal from the
CLI's own result object.

## Decision

### A new, distinct sentinel type — not a reuse of `claudeUsageLimitError`

The naive reading of the issue ("add one further structural `terminal_reason` match alongside the
existing check") would add `"api_error"` as a second value inside `classifyUsageLimitExit` and route it
through the existing `claudeUsageLimitError` type. This was rejected: `claudeUsageLimitError` is not an
inert marker. By Go *type*, via `errors.As`, it is the trigger for two additional behaviors bundled at
three call sites:

1. Account-wide suspension activation (`activateClaudeSuspension`, in both
   `runInvocationWithExtension` and `runCommentExtensionLoop`).
2. Exclusion from the comment-processing circuit breaker's count (`processComments`).

Both are correct only for a genuine account-wide usage limit. An `api_error` is per-invocation and
transient — it says nothing about the account as a whole. Reusing the type would have silently
suspended Claude dispatch account-wide on every transient API blip (directly regressing the ADR-1183
incident this repository already paid for once, in spirit if not in mechanism) and would have exempted
the comment-triggered dispatch path from its only existing bound (the circuit breaker), converting a
bounded 3-strike pause into a potentially unbounded fast retry loop — exactly the failure mode
`api_error`'s own observed shape (#1208: five exits in 31 seconds) demonstrates is reachable.

`claudeAPIErrorExit` is therefore a new sentinel type, structurally parallel to the existing
`claudeTurnLimitError`/`apiKeyHelperDetectedError` precedents:

```go
const apiErrorTerminalReason = "api_error"

type claudeAPIErrorExit struct {
    TerminalReason string
    NumTurns       int
    CostUSD        float64
}
```

`classifyAPIErrorExit(resp claudeResponse, usage TokenUsage)` mirrors `classifyUsageLimitExit` exactly:
structural-only (`resp.TerminalReason == "api_error"`, never output prose), reusing the same conjunctive
`usage.TurnsUsed > 0 && usage.CostUSD > 0` exclusion. `interpretClaudeResult` (`engine/claude.go`) checks
this immediately after the `classifyUsageLimitExit` branch and before the pre-existing diagnostic-only
unmatched-`terminal_reason` log line, so an `api_error` exit that fails the guard (real turns and real
cost) still falls through to that log line unchanged — the same behavior it had before this issue.

Being a distinct type means the two `errors.As(&claudeUsageLimitError{})` checks in `item.go`/
`comments.go` simply never match a `*claudeAPIErrorExit` — zero changes needed at either call site. This
is a structural guarantee, not a conditional added at each site that could be forgotten in a future
edit.

### Handling: a fifth, more minimal "did not run" shape

`finalizeStageOutcome()` (`engine/item.go`) gains one more `errors.As` branch, alongside the existing
`claudeUsageLimitError`/`apiKeyHelperDetectedError` branches, routing to `handleAPIErrorExit`. It applies
`itemstate.StageAttempted` (so the normal dispatch cooldown applies) but never `StageRetryIncremented` —
the same split every prior "did not run" handler uses. Unlike `handleUsageLimitExit` and
`handleAPIKeyHelperDetected`, it applies **no label and posts no comment** — R4's explicit requirement,
and a shape none of the four existing "did not run"/boundary handlers matched exactly. The condition
self-resolves on the next invocation; a label here would be indistinguishable from the orphaned-durable-
state leak ADR-1183's own sweep exists to clean up, and a comment for a self-healing, per-invocation
transient is noise (the same "comment when a human must act, log when the engine self-heals" rule #1414
established for a sibling condition).

### `engine/comments.go` is deliberately untouched

The comment-triggered dispatch path has no `LastAttemptAt`-based cooldown at all — that mechanism only
exists in `itemNeedsWork`'s stage-dispatch fallthrough, which the comment path bypasses entirely (a new
comment dispatches unconditionally once detected). Its only existing bound is the comment-processing
circuit breaker (`checkCommentBreaker`, #1089, default 10 invocations/30 min). Because
`claudeAPIErrorExit` is a distinct type from `claudeUsageLimitError`, it automatically falls through to
the same `checkCommentBreaker(item, "")` call every other unclassified error already reaches — no code
change was needed, and none was made. This is also the answer to R6 for the comment-triggered path:
unlike a genuine usage-limit hit (deliberately exempt from the breaker, since it says nothing about a
specific issue's comment thread), an `api_error` should count toward "no forward progress" accounting —
counting it is a feature, not an oversight, cheap insurance against an unusually long-lived transient. It
is the direct explanation for #1208's 31-second/5-invocation burst: comment-triggered redispatch, not
the 5-minute-cadence stage-dispatch cooldown that bounds #1408's pattern.

## Rationale

### Why reuse `classifyUsageLimitExit`'s exact exclusion guard despite unverified `CostUSD`?

R5 asked whether the observed 1-turn/`$0.0000` `api_error` exits' `CostUSD` is exactly zero or a small
value the `%.4f` log format rounds down — this could not be empirically verified: no raw NDJSON/result-
object sample of a real `api_error` exit exists anywhere in this repository (`grep -rl "api_error"`
across all `.go` files returned nothing prior to this change). The guard is reused unchanged rather than
dropped, because it is directionally safe regardless of the true value: if a future `api_error` exit
follows genuine billed work (many turns, real cost), the guard excludes it and it falls through
unchanged to the pre-existing generic-failure path — no regression, no silent over-classification of a
real failure. If cost is genuinely zero, as the observed shape strongly suggests, the guard passes and
the fix activates. Dropping the guard entirely was considered and rejected: it would remove the belt-
and-suspenders protection that is the direct lesson of this codebase's own #1184/#1183 history, for no
evidenced benefit. The pre-existing diagnostic-only log line remains reachable for an `api_error` that
fails the guard, so a future occurrence stays observable and the guard can be revisited with real
captured data.

### Why not a config flag or a shared "did-not-run" umbrella type instead of a new struct?

A single umbrella type covering usage-limit, apiKeyHelper, and api_error conditions was considered and
rejected: the three conditions differ in exactly the two behaviors (suspension, breaker exemption) this
decision hinges on keeping *out* of the `api_error` path. A shared type would need a discriminant field
and conditional logic at every call site that currently just checks a type — reintroducing the coupling
risk this ADR's whole design avoids. Small, distinct sentinel types matching the existing
`claudeTurnLimitError`/`apiKeyHelperDetectedError` precedent keep each condition's blast radius exactly
as narrow as it needs to be, verified by the compiler (a call site either handles the type or it falls
through) rather than by a runtime conditional someone has to remember to add.

## Consequences

**Positive:**
- A transient `api_error` exit no longer consumes `max_retries` budget; a genuinely transient failure
  that happens to follow one still has the full budget available (mirrors ADR-1119's regression guard).
- No account-wide suspension is ever triggered by a per-invocation transient — avoiding a new false-
  positive class in the same family ADR-1183 already paid to eliminate once.
- The comment-triggered dispatch path gains a correct, non-invasive bound (the existing circuit breaker)
  without inventing a new mechanism.

**Negative / Trade-offs:**
- No new label means no durable operator-visible signal that an issue has recently hit `api_error`
  exits — by design (R4), but it does mean a human reviewing an issue's label history sees no trace of
  a self-healed episode, only the log line.
- The exclusion guard's `CostUSD > 0` behavior remains unverified against a real captured sample; if a
  future occurrence shows nonzero cost, the guard silently stops matching and this fix becomes inert for
  that shape until revisited (mitigated by the diagnostic log line staying reachable).
- Like ADR-1119's usage-limit exemption, there is no ceiling on retries for this condition — an
  unusually persistent or misdetected `api_error` condition on the stage-dispatch path retries every
  cooldown window indefinitely rather than ever escalating for human review. Bounded on the comment-
  dispatch path only by the existing circuit breaker.

## Explicitly Out of Scope

Other unmatched `terminal_reason` values (e.g. `rapid_refill_breaker`) — added only when observed in the
wild with evidence, per the same discipline that kept this list at one entry (`blocking_limit`) until
this issue. Retry/backoff policy for genuine stage failures. The account-wide suspension mechanism
itself (ADR-1120), untouched by this change.

**References:** ADR-1119 (the `StageAttempted`-without-`StageRetryIncremented` precedent this mirrors),
ADR-1183 (why classification is structural-only), ADR-1120 (the account-wide suspension this
deliberately does not touch), ADR-1199 (confirmed unrelated — a different budget, `SliceRetryIncremented`,
for turn-cap preemption where the stage *did* run).
