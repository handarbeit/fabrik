# ADR 1811: Split api_error on api_error_status

## Status

Accepted. Narrows ADR-1458; does not overturn it. Builds on ADR-1183.

## Context

The CLI reports a session-limit hit two ways. `terminal_reason: "blocking_limit"` is the
structural signal `classifyUsageLimitExit` keys on (ADR-1183). The other is
`terminal_reason: "api_error"` with `api_error_status: 429` at 0 turns / $0.

ADR-1458 introduced `classifyAPIErrorExit` from eight exits that carried no `api_error_status`, and
treated every `api_error` as a per-invocation, self-resolving blip: no suspension, no label, exempt
from `max_retries`. Measured 2026-09-19 across three daemons, every recorded `api_error` exit
(532) was a session limit, with the result text confirming it in 100% of cases. Because the exit was
classed as transient, `activateClaudeSuspension` never fired and the engine re-dispatched every poll
for the whole limit window (139 invocations in 20 minutes on #1752; 93 in an hour on 86ed #574).
Community report #1566 is the report behind this.

## Decision

`classifyUsageLimitExit` gains a second structural trigger: `terminal_reason == "api_error"` and
`api_error_status == 429`, behind the same turns/cost exclusion gate. The classifier chain already
tries it before `classifyAPIErrorExit`, so 429 becomes a `claudeUsageLimitError` and every other
`api_error` falls through unchanged. No downstream code changes: the stage, comment, and merge-train
paths already key on `errors.As(&claudeUsageLimitError)`.

- **Structural only.** Only `terminal_reason`, `api_error_status`, and the usage counters are read.
  `result` is never matched (ADR-1183's incident: prose matching suspended dispatch for ~11 hours).
- **Tolerant decoding.** `apiErrorStatus` is an int-based type whose `UnmarshalJSON` never errors.
  `parseClaudeJSON` discards the whole result object on any unmarshal error, so a strict `int` would
  let one wrong-typed value erase every classification, `blocking_limit` included. Only a JSON
  integer counts; absent, `null`, string, float, or object read as 0 — "not a 429" (fail-safe:
  under-detecting costs retries, over-detecting suspends a healthy account).
- **Non-429 unchanged.** 500/502/529 and absent statuses stay `claudeAPIErrorExit` (ADR-1458).
- **`ResetTime` is not populated (issue R6 dropped).** It is not display-only in this codebase: it
  feeds `activateClaudeSuspension` → `computeUsageLimitResetDeadline`, so populating it from the
  result text would change the suspension from the fixed one-hour fallback to the parsed reset —
  out of scope — and would let prose reach a behavioural decision. A separate display-only field
  is a possible follow-up.

## Consequences

- A limit window now produces one `fabrik:claude-limit` label and an account-wide suspension instead
  of hundreds of invocations. Operators will stop seeing "transient api_error" log lines for 429s;
  this is called out in the release notes.
- With `ResetTime` empty the suspension is the fixed one hour, so a limit resetting hours later costs
  one turn-0 probe per hour until it lifts.
- A mid-session 429 after real work (turns and cost both non-zero) is deliberately not detected: the
  shared exclusion gate is retained.
- Merge-train conflict resolution now returns early on a 429 instead of ejecting the member as
  unresolvable.
