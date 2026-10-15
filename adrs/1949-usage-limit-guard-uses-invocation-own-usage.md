# ADR 1949: The usage-limit "real work" guard uses this invocation's own usage

## Status

Accepted. Amends the gate wording of ADR-1811 and ADR-1458; ADR-1183 and ADR-1815 unchanged.

## Context

`classifyUsageLimitExit` and `classifyAPIErrorExit` decline when `usage.TurnsUsed > 0 && usage.CostUSD > 0`, on the premise that a did-not-run limit exit bills nothing. On `--resume` the CLI reports the resumed session's cumulative figures, so in the 0.0.83 gate (alpha#6829) two resumed Validate invocations rejected at the weekly limit read as `1 turn, $0.6891`, were not classified, and fell through to `classifyResumeFailure`. The session was abandoned after two exits and the account-wide suspension started only on the third, fresh, invocation.

## Decision

Both classifiers keep their signatures and their guard, but `interpretClaudeResult` feeds them `invocationGuardUsage(usage, rawOutput, resumed)`:

- fresh invocation: usage unchanged (cumulative equals own);
- resumed with no `type:"user"` line in this invocation's own stream: turns and cost zeroed, so the guard cannot fire;
- resumed with at least one `type:"user"` line: unchanged, so the guard declines as before.

The `user`-line definition is the one `turnCountingWriter` already uses for a turn. Only line types are read, never content (ADR-1183). Reported usage is left untouched for logs and accounting.

## Alternatives rejected

- **Delta against pre-invocation totals.** Needs new plumbing through `InvokeOptions`/`runClaude`, depends on in-memory `itemstate`, and is wrong after a restart.
- **A rejected `rate_limit_event` as decisive.** A limit hit mid-work also emits one, so it would classify real-work exits as did-not-run, breaking ADR-1811's retained gate. It stays an input only to the reset deadline (ADR-1815).
- **Counting assistant lines.** The CLI can emit a synthetic assistant error message on the 429 path.
- **Applying the count to fresh invocations.** Their usage is already their own; leaving that path untouched keeps behaviour byte-identical.

## Consequences

- A resumed usage-limit or `api_error` exit is classified on first occurrence; the suspension, did-not-run refunds (#1812) and `fabrik:claude-limit` apply, and the resume counter and session pointer are untouched (R2).
- A mid-session 429 after real work is still not detected (ADR-1811, ADR-1815 unchanged).
- Inverse gap, accepted: a resumed invocation that streams assistant output and then takes a 429 before any tool result has no `type:"user"` line, so its (partially billed) cost is zeroed for the guard and it is classified as a did-not-run usage limit, with the suspension and the #1812 refund. Partial work is therefore refunded rather than charged. This errs toward the account-wide suspension, which is the correct response to a real 429, and a retry costs only an attempt. Counting `assistant` lines to close it is rejected because the CLI can emit a synthetic assistant error message on the 429 path, which would make the common did-not-run exit look like work and reintroduce this defect. Whether the CLI emits a 429 after real assistant output with no following `user` line is unverified; if it does, a targeted non-synthetic-assistant check belongs in `invocationGuardUsage`.
- A genuinely poisoned session that returns a 429-shaped result no longer counts toward abandonment; the usage-limit hold handles it.
- The stream shape of the real 429 exit (no `user` line) is taken from the issue's description; if the CLI ever emits one, `isUserTurnLine` use in `invocationGuardUsage` is the single place to tighten.
- Follow-up, not changed here: `InvocationRecorded` also records cumulative usage as per-invocation (`item.go`, `comments.go`); that is an accounting defect, not a classification one.
- Test seam: `invocationOwnUsageDisabledForTest`.
