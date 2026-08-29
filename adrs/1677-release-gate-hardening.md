# ADR 1677: Release-Gate Hardening — Bed Reset, Unified Parallelism Cap, Pre-Gate Dedup Fix

**Date**: 2026-08-29
**Status**: Accepted
**Issue**: #1677 — four release-gate defects found cutting v0.0.81 — bed accumulation,
uncapped `-race`, unreachable parallelism band, dead pre-gate guard

## Context

Cutting v0.0.81 took ten attempts. None of the failures were defects in the code being
released — every one was the release harness itself. Four distinct, independent root causes:

1. **Bed accumulation.** `cut-release.sh`'s invocation of `scripts/e2e/run.sh` never passed
   `--clean`, so the shared test bed's board/label state (and local session/log trees)
   accrued release over release, until `TestSwitchTrainMode`'s fixed 30s lock-release wait
   was no longer enough margin — killing two of the ten attempts outright, with a bare
   `"did not release lock within 30s"` giving no clue why.
2. **Uncapped repo-wide `-race`.** #1624 capped `scripts/sim/run.sh`'s own `-parallel`
   (`SIM_PARALLEL`, default `min(8, host cores)`) specifically to avoid a TSan fork/exec
   SIGSEGV on high-core-count hosts, but both `cut-release.sh` step 4 and CI's `go test -race
   ./...` run the exact same git-forking `tests/sim` package as part of `./...`, uncapped —
   reproducing the identical crash via a different entry point.
3. **The viable parallelism band was narrower than #1624 assumed.** Measured on the 28-core
   host that produced both #1624 and #1677:

   | `-parallel` | outcome |
   |---|---|
   | 28 (default, uncapped) | TSan fork/exec SIGSEGV, frequent |
   | 8 (#1624's own default) | ~130s, SIGSEGV roughly **1 run in 5** — not reliable for a release gate |
   | 4 | ~661s, zero SIGSEGVs across multiple consecutive runs — but over `go test`'s undocumented 10-minute default `-timeout`, which nobody had ever made explicit |

   This data cost a full release cycle (ten attempts) to obtain, which is why it's recorded
   here in full rather than summarized — re-deriving it requires the same multi-run,
   high-core-count validation methodology described in Consequences below.
4. **The pre-gate SHA dedup guard (#1624's R5) never actually engaged.** `run_pregate`'s
   `FABRIK_PREGATE_VERIFIED_SHA` check requires both a HEAD match and a clean working tree.
   `cut-release.sh` step 4's "Record embedded plugin hash" step writes
   `plugin/known_embedded_versions.go` on disk, uncommitted, between the SHA being captured
   (end of step 3) and step 5's dirty-tree check — so on essentially every real release the
   tree was dirty by the time the guard ran, and the sim + wire-contract pre-gate paid for
   itself twice on every one of the ten attempts, doubling the odds of losing an attempt to
   the very SIGSEGV described above.

## Decision

**1. `cut-release.sh` step 5 now passes `--clean` unconditionally.** `scripts/e2e/reset.sh`
already existed and was already destructive by design (closes open PRs/issues, deletes
`fabrik/*` branches, drains the board) — this was a missing call site, not new capability.
The trade-off (a release cut and concurrent manual e2e testing on the shared bed can no
longer safely overlap) is accepted and documented in the step's own header comment; it is
the correct trade given the alternative is a release-gate failure mode that recurs "every
few releases by construction."

**2. `tests/e2e/lifecycle.go`'s `StopFabrikTestBed`/`StartFabrikTestBed` get a shared,
generous 90s timeout (up from a hardcoded 30s/40s) with periodic progress logging and
failure diagnostics.** Rejected deriving the timeout from the engine's own
`--drain-deadline` config — more precise, but couples the test harness to engine internals
for a marginal gain. 90s matches the convention `scripts/e2e/run.sh`'s own
`preflight_bed_start` already uses for bed startup. On timeout, the failure message now
includes `bedDiagnostics`: process liveness plus a tail of the bed's own `fabrik.log` — the
exact janitor/scan activity the original incident's evidence block showed at the moment of
failure, previously discarded entirely by the bare `"did not release lock within 30s"`
message.

**3. One shared parallelism-cap helper (`scripts/lib/parallel.sh`'s `default_race_parallel`),
ceiling lowered from `min(8, cores)` to `min(4, cores)`, used by all three invocations that
exercise the same git-forking `tests/sim` package under `-race`: `scripts/sim/run.sh`
(`SIM_PARALLEL`'s default), `cut-release.sh` step 4's repo-wide `go test -race ./...`, and
CI's equivalent step.** Treating the three independently was rejected — they exercise
overlapping concurrency, so a coherent single cap is required or one of the three caps
becomes pointless (confirmed during Research: capping only `scripts/sim/run.sh` left the
other two free to reproduce the exact SIGSEGV the cap exists to prevent, which is exactly
what happened during the v0.0.81 cut). Lowering the ceiling to 4 (rather than merely
plumbing 8 through to all three sites) follows directly from the measured ~20%-per-run
SIGSEGV rate at 8 — not reliable enough for a release gate. CI's 2–4-core runners are
unaffected in practice (the cap is a no-op there); this reverses #1624's own explicit
decision to scope the cap away from a bare `go test -race ./...` invocation, because that
reasoning held for CI specifically, not for a high-core-count release-cutting workstation
running the identical command locally via `cut-release.sh`.

**4. `scripts/sim/run.sh` and `cut-release.sh` step 4 both add an explicit `-timeout 20m`
(`SIM_TIMEOUT`, overridable).** This is what makes the lower, reliable `-parallel 4` value
actually viable rather than merely "less likely to SIGSEGV but liable to time out instead" —
the previous absence of an explicit `-timeout` meant `-parallel 4`'s ~661s runtime was
silently racing an undocumented 10-minute Go default. Rejected the alternative mechanism
(excluding git-forking scenarios from `-race` entirely) as more invasive — per-scenario
build-tag/skip machinery — for a problem the lower cap + explicit timeout combination already
resolves per the issue's own measured data (zero SIGSEGVs at `-parallel 4` across multiple
runs).

**5. `run_pregate`'s dirty-tree check now accepts a caller-declared allowlist
(`FABRIK_PREGATE_ALLOWED_DIRTY_REGEX`), generated by a single `allowed_dirty_regex()`
function in `cut-release.sh` shared with step 1's own preflight dirty-tree filter, instead of
hardcoding the one known offending filename inside `run_pregate` itself.** Hardcoding
`plugin/known_embedded_versions.go` directly in `run.sh` was rejected: a future change adding
a second such self-write pattern to `cut-release.sh` (as `plugin/*/.claude-plugin/plugin.json`
already is, for step 6b) would silently defeat the guard again in the same way, and step 1's
preflight already maintains exactly this list — two independently-maintained "files this
script expects to leave dirty" lists is the drift risk this closes, not just the one instance
found during Research. Anything **not** matching the caller-declared allowlist still counts
as dirty and still falls through to the full pre-gate, exactly as before — this does **not**
reopen the TOCTOU gap the clean-tree check exists to close (see #1624's R5): only
already-known, already-declared self-writes are ever disregarded, never a blanket "ignore all
dirty files" loosening. A standalone `scripts/e2e/run.sh` invocation never sets
`FABRIK_PREGATE_ALLOWED_DIRTY_REGEX`, so its own dirty-tree check is unchanged and exactly as
strict as before this existed.

## Consequences

- A release cut is now always preceded by a full bed reset — a stale bed from a prior release
  (or from interrupted manual testing) cannot fail the mode-switch lock-timeout check, closing
  the failure mode that killed two of v0.0.81's ten attempts. The cost is that a release cut
  and concurrent manual e2e testing on the shared bed are now mutually exclusive by
  construction, not just by convention.
- The repo-wide `go test -race ./...` gate (`cut-release.sh` step 4 and CI) is parallelism-capped
  identically to `scripts/sim/run.sh`, closing the gap that let the exact SIGSEGV #1624 was
  meant to prevent recur via an uncapped entry point.
- The sim suite's viable configuration (`-parallel 4`, `-timeout 20m`) is validated the same
  way the original incident was diagnosed — multiple consecutive runs on a high-core-count
  host, not a single clean CI pass, since CI's 2–4-core runners cannot reproduce the
  fork/exec contention this addresses. This is a process obligation for future changes to
  this area, not something CI enforces automatically.
- The pre-gate SHA dedup guard (#1624's R5) now actually engages on a real release cut: the
  sim + wire-contract pre-gate runs exactly once per release instead of twice, removing the
  doubled odds of losing an attempt to a flake that affected all ten v0.0.81 attempts.
- `scripts/lib/parallel.sh` and `allowed_dirty_regex()` are now the two canonical,
  single-source-of-truth mechanisms for "parallelism cap" and "expected self-write allowlist"
  respectively — future call sites should extend these rather than reintroducing a third,
  independently-tuned copy of either.

See also: adrs/1624-sim-suite-concurrency-and-pregate-dedup.md (the direct predecessor this
issue extends and partially reverses — specifically its CI-scoping decision for the
parallelism cap), adrs/1454-sim-pre-gate-not-replacement.md (establishes the sim suite as a
pre-gate that never substitutes for the live e2e suite — unaffected by this issue).
