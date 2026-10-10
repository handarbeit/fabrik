# ADR 2065: Remember a repo that rejects catch-up pushes by policy

## Status

Accepted (#2065). Extends ADR-2044, resolving the follow-up its Consequences section names. #2044 is mentioned for context.

## Context

ADR-2044's singleton catch-up pushes a merge commit onto the member's own branch. A repo that forbids merge commits on PR branches (linear history, a ruleset, branch protection) rejects that push every time, and the rejection is an *uncharged fallback* to the trial. So on every poll, for every member, the engine prepared a worktree, merged, attempted a push and logged — with an outcome that never changes. `singleton_catch_up: off` stops it, but only if the operator knows to set it.

## Decision

1. **Classify by GitHub's markers, conservatively.** `PushCatchUp` returns a typed `*CatchUpPushError` carrying git's raw output (its `Error()` text is unchanged). `classifyCatchUpPushRejection` calls it policy only on a positive `GH006` or `GH013` marker. Vetoes win over a marker: lease failures (`stale info`, `fetch first`, `non-fast-forward`) and transport/credential markers (`fatal: unable to access`, `Could not resolve host`, `Connection reset`, `returned error: 5xx`, `Permission to … denied`). Free prose such as a bare `protected branch` never matches. Required signatures also match: that rejection is as permanent for the repo and the operator guidance is the same.
2. **A `GH013` that mentions secrets or push protection is ambiguous.** It depends on the content being pushed (for example a secret in one member's conflict-resolution edits), not on the repo, so it must not disable the catch-up for every member. This is a false negative by design.
3. **Per-repo, in-memory, 24h.** The memo is keyed by `owner/repo`, not the `(repo, base)` partition: the rules that matter apply to PR branches repo-wide, and a repo with several `base:` partitions should not probe once per partition. It expires after `catchUpPolicyMemoTTL` (24h, on the engine clock so tests and the sim can advance it) and is cleared by restart. Nothing is persisted and the repo's rules are not probed through the API (the same shape as ADR-2052's `trainInfraAbandonCooldown`).
4. **Consult before attempting.** While a memo is active, `trySingletonCatchUp` returns to the trial path right after the `singletonCatchUpEnabled()` and `baseSHA` checks — before `FetchCommitsBehind` — so there is no GitHub call, worktree, merge or push. `off` keeps its meaning (it is checked first).
5. **One operator line per memo.** Setting the memo logs once: the repo, GitHub's rejection reason (the `GH0xx` line plus `- …` rule bullets, with `remote:` prefixes stripped, control characters collapsed, capped at 300 runes — remote output is influenceable) and `singleton_catch_up: off`, `--singleton-catch-up`, `FABRIK_SINGLETON_CATCH_UP`. Skipped singletons log nothing. A rejection arriving during an active memo neither extends it nor logs (set-if-absent-or-expired and the log decision share one lock acquisition). After expiry a fresh rejection sets a new memo and logs again.
6. **The fallback does not change.** A policy rejection is still uncharged: no `recordTrial`, no ejection count, no pause, no attempt recorded.
7. **Other fallbacks are not memoised** (refused `PrepareCatchUp`, unexpected merge shape, CI that never starts). The default of `singleton_catch_up` is unchanged and there is no rebase variant.
8. **Test seam.** `catchUpPolicyMemoSkipDisabledForTest` (with `SetCatchUpPolicyMemoSkipDisabledForTest`) bypasses the consult so a test can show the skip assertions are not vacuous.

## Consequences

- A repo that forbids merge commits stops paying a merge and a push attempt per poll after the first rejection; singletons there cost the trial path they would have taken anyway.
- Fixtures under `engine/testdata/catchup_push_rejections/` are synthetic, built from GitHub's documented rejection format rather than recorded from a live repo. Only the markers are trusted, so drift in GitHub's prose shows up as a false negative, which keeps today's safe per-poll behaviour.
- **Residual risk:** a content-dependent `GH013` rule that does not mention secrets would be misread as repo policy and disable catch-up there for up to 24h. Expiry, a restart, or `off` bound it.
