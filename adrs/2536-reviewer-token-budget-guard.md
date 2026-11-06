# ADR 2536: Reviewer-token budget guard in the release gate

## Status

Accepted (issue #2536). Builds on issue #1684 (competing consumers), 1676 (bounded probes), 1972 (missing, not failed), 1976/2263 (identity scheduling).

## Context

On the 0.0.91 gate, `FABRIK_REVIEWER_TOKEN`'s REST `core` budget was 0/5000 when two legs started. Every test touching it failed at its first call, the failed leg stopped the remaining four cells, and coverage stood at 65 of 366 after ~1.5 hours of live spend. The gate guarded only the engine token. The reviewer belongs to a personal account other local sessions share.

## Decisions

1. **A separate, read-only facility, not an `identitySet` member.** The set drives exclusive/shared acquisition and the budget floor; the reviewer has no engine or harness role. Folding it in would be a large semantic change for a guard.
2. **Two buckets, two sources.** REST `core` from `GET /rate_limit` (`IdentityOps.CoreBudget`; free, and a live gauge — the "dead gauge" note in `budget.go` is about GraphQL). GraphQL from the inline `rateLimit` query as for other identities. A GraphQL read refused *because* the bucket is exhausted (rate-limit wording or `RATE_LIMITED`) is 0 remaining; any other failure is `unavailable`.
3. **Preflight is the first live preflight**, after the free pre-gate (ADR 1454) and before `bed-identity`/`board-lag-probe`. Exit 7, banner names login, remaining/limit, reset, minimum and its env override.
4. **Login is best effort.** An exhausted token cannot answer `GET /user`, so the refusal falls back to naming the bed and variable.
5. **Cell start check in `Gate.runLeg`**, shared by both schedulers, before `RunLeg` notes an invocation, restarts the bed or records anything. Hold when the reset is inside `E2E_REVIEWER_HOLD_MAX` (default 10 min), otherwise decline. A missing or past reset is not recovering. A GraphQL read refused because the bucket is exhausted carries no reset time, so an exhausted GraphQL bucket is declined rather than held; the `resources.graphql` gauge of `GET /rate_limit` is not used to fill it in, because `budget.go` documents that gauge as unreliable for these tokens. The cost is resilience only: `--resume` recovers the cell after the hourly reset. Applies to every cell (no registry field says which tests use the reviewer; over-guarding is the safe failure).
6. **Decline reuses exit 3 through a distinct type**, `*ReviewerBudgetError` wrapping `ExitError`. It is the existing "budget exhausted" outcome, but `multiSched.finish` matches it first so the engine identity is not marked exhausted; it stops new cells and records the cell as not started. Nothing ran, so nothing is voided and pairs stay missing for `--resume`.
7. **Multi-bed hold happens after identity acquisition**: the scheduler does no I/O under its lock. The hold is bounded and logged, and delays only cells wanting the same engine identity.
8. **Competing consumers use the same `.env` proxy as issue #1684**: local `fabrik` processes (not any bed's engine) whose cwd `.env` has the reviewer token. No pat-leg downgrade. Live process environments are platform-dependent (SIP) and processes elsewhere are invisible; the budget checks cover them.
9. **Thresholds are absolute counts**, defaults 500 REST core and 100 GraphQL, `0` disables a bucket. They are provisional; the new per-leg reviewer lines are the data to tune from.
10. **Unreadable means unavailable**: every read goes through the bounded `gh` path; a failure warns and never blocks. Error text is redacted of the token.

## Consequences

An exhausted reviewer account now fails fast at no live cost, or pauses between cells, instead of burning a gate run. Exit 3 now has a second meaning (reviewer budget before a cell started); the message and `scripts/cut-release.sh` say so. The engine is untouched.
