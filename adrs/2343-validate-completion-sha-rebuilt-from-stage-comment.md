# ADR 2343: The Validate completion SHA is rebuilt from the Validate stage comment after a restart

## Status

Accepted. Extends ADR 2158 (its catch-up exemption now holds across a restart).

## Context

`ItemState.ValidateCompletedSHA` is written when Validate completes and compared by
`settleSHAInvalidationScan` (`docs/state-machine.md` §2.16) against the linked PR's live head: a push after
Validate completed must strip `stage:Validate:complete` so Validate re-runs (#1225). The value lives only in
the in-memory Store. After a restart (SIGHUP, self-upgrade, crash) it is empty, and the scan skips an empty
value (FR-5). An item waiting at `stage:Validate:complete` therefore stopped being protected: a push after the
restart never invalidated Validate, and under yolo the PR landed on gates that do not re-run Validate.
Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md` §1.3).

A second ordering fact matters: in `poll()` the catch-up loop (`runCatchUpPhase2`, which reaches the landing
decision) runs **before** `settleSHAInvalidationScan`. Restoring the value only inside the scan would still let
a yolo item land on the first post-restart poll.

## Decision

1. **Rebuild, do not persist (R2).** The SHA is recovered from durable GitHub evidence Fabrik already writes:
   the `commit:` field of the engine's own `🏭 **Fabrik — stage: Validate**` comment. `captureGitMeta` reads
   the worktree HEAD after the run ends, so the value can never be newer than the head Validate ran on. No
   new file, no new write site, no stale-file hazard when someone removes a label while the daemon is down.
   Rejected: a persisted sidecar (four write sites plus clear-on-invalidate, wrong after out-of-band label
   changes) and a persisted-first hybrid (two sources for no gain).
2. **Only the stage comment is read.** `(comment review)` and `(review feedback addressed)` comments carry
   different headers and never match, so a later reinvoke comment cannot pull the SHA forward past a push that
   was never validated. The last matching comment wins. Only comments authored by `selfLogin()` count.
3. **Abbreviated SHA, no git.** The header holds 8 hex characters. If the live head starts with it, the head
   is unchanged since Validate ran and the full live head is recorded; otherwise the abbreviated commit is
   recorded as-is. It differs from the live head, so every existing `!=` comparison reads "head moved" and the
   #2158 exemptions (landed, catch-up in flight, pure catch-up) decide. The stored value may therefore be a
   short SHA and must only ever be compared for (in)equality with a full head, never used as a git revision.
   Rejected: expanding with `git rev-parse` (fails on a force-pushed-away object or ambiguity, needs a
   repository in a hot path) and lengthening the header (changes `formatMetaLine` and every consumer,
   including Pruefer's copy).
4. **Hydrate before the catch-up loop.** `hydrateValidateCompletionSHAs` (`engine/validate_sha_rebuild.go`)
   runs immediately before the catch-up loop in `poll()`, fills the SHA of every open, unpaused,
   `stage:Validate:complete` item at Validate whose Store value is empty, then runs the scan's own check
   (`checkValidateSHAInvalidation`, extracted from the scan so both apply one rule) and drops any item it
   invalidated from the loop (its label copy is stale). The scan stays as the steady-state backstop and keeps
   its FR-5 skip. The hydrate skips landed items, items with a worker in flight and items mid catch-up push;
   it waits (no action) while the live head is unknown, using the poll-observed PR head when the
   engine-recorded head is still empty right after a restart.
5. **R3: no recoverable SHA means re-run Validate.** If the item has `stage:Validate:complete`, a head is
   known and no SHA can be recovered (no own Validate comment, not authored by Fabrik, malformed or `unknown`
   commit), the engine calls
   `handleValidateSHAInvalidation` with one `[validate-sha]` log line. Holding the landing was rejected: it
   adds a stuck state only `fabrik:revalidate` could clear. Re-running reuses a tested path and heals itself,
   because the re-run records a fresh SHA. The cost is one extra Validate for an item whose evidence is gone.
6. **R4.** A pure engine catch-up head after a restart is re-recorded rather than invalidated: the rebuilt
   (older) SHA differs from the catch-up head, `headIsPureCatchUp` reads the durable self-authored marker, and
   the head is re-recorded exactly as ADR 2158 describes. Only the in-flight window state is memory-only, and
   the hydrate defers while it is set.

## Consequences

- A push after a restart invalidates Validate before the first post-restart landing decision.
- Known gap: the `wait_for_ci` writer records the head at CI-clear, which can be newer than the Validate
  comment's commit when the engine itself pushed in between. After a restart that head reads as "moved" and
  the scan or the pure-catch-up exemption decides. This errs toward re-validating, never toward landing
  unverified.
- Trust limit: under PAT auth `selfLogin()` is the operator's login, so the operator could forge the header.
  This is the same limit as the catch-up marker.
- Deploy: the first start after this ships rebuilds for every Validate-complete item; mismatches re-validate.
- Seam: `Engine.SetValidateSHARebuildDisabledForTest` (per Engine) restores the old no-op; the sim twins
  `tests/sim/validate_sha_restart_test.go` use it to prove they are not vacuous.
