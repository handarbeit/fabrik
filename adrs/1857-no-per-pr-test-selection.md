# ADR 1857: CI Runs the Whole Module on Every PR — No Per-PR Test Selection, No `simgh` Sharding

**Date**: 2026-09-25
**Status**: Accepted
**Issue**: #1857 — ci: select tests from the dependency graph instead of running the whole module on every PR

## Context

`.github/workflows/ci.yml` runs `go test -race … ./...` over every package on every pull
request, with no `paths:` filter. #1857 asked whether to derive a per-PR package set from
the reverse-dependency closure of the changed files (computed from Go's import graph, not
from hand-maintained globs), failing closed on any ambiguity. Its R6 made that build
conditional on a cost comparison first: would sharding/parallelising `tests/sim/simgh`,
said to be ~60% of the suite, deliver most of the win with none of the selection risk? And
the issue said explicitly that "don't do the clever thing" is a legitimate outcome.

The measurements below contradict two of the issue's premises, and the remaining benefit
of selection is small next to its failure mode. **Neither selection nor sharding is built.**

This ADR is a **dated snapshot** of the evidence behind that decision (2026-09-25). It is
not a statement of current runtimes — `tests/sim/README.md` deliberately records none,
because such figures go stale. Re-measure before relying on any number here.

## Evidence

### Measured CI cost (R6)

Fourteen successful `pull_request` runs of `ci.yml` on `ubuntu-latest` (public-repo
runners, 4 vCPU), created 2026-09-24 to 2026-09-25. Run IDs: 35973725281, 36089958446,
36123689876, 36127125280, 36127908988, 36189977287, 36190707909, 36190869159, 36192428947,
36195329261, 36195663810, 36196124595, 36199752612, 36201098977. Per-package times come
from the `ok <pkg> <time>` lines of the `go test` step.

| Package | min | median | max |
|---|---|---|---|
| `tests/sim` | 120.1s | 125.8s | 130.1s |
| `engine` | 79.1s | 93.3s | 98.3s |
| `github` | 22.6s | 24.8s | 25.1s |
| `tests/sim/simgh` (10 of 14 runs; the other 4 were `(cached)`) | 13.7s | 18.6s | 20.4s |
| `pruefer` | 4.6s | 4.7s | 5.0s |

Everything else is 1–3s. The `go test` step's wall clock (step start to `tests/sim`
finishing, i.e. including compilation) was 145–200s.

**Premise correction 1 — `simgh` is not ~60% of the suite.** It is ~19s on CI when it
runs at all, and `(cached)` on 4 of 14 runs. The suite's critical path is `tests/sim`
(~126s, already parallel under the `-parallel` cap of ADR 1677) and `engine` (~93s,
fully serial). The ~189s figure in the issue does not reproduce on CI; local runs on a
loaded, many-core machine showed `simgh` at ~96–104s, which is likely where it came from.

### `simgh` sharding (R6)

Sharding `simgh` can save at most its own ~19s off a 145–200s step, and only if the
package's tests can safely share a process:

- It has 173 tests and zero `t.Parallel()`. `git_timeout_test.go` and `state_test.go`
  use `t.Setenv`, which forbids `t.Parallel()` in those tests.
- Its cost is spread out, git-bound and outlier-free: 17 tests over 1s account for about
  half of it, and the slowest single test is ~9s locally.
- The concurrency-hammer tests (`concurrency_test.go`, `instrumented_concurrency_test.go`)
  assert on wall-clock and interleaving, and may not survive being run under extra load.
- R6 requires any adopted sharding to preserve coverage exactly, under `-race`, with
  nothing skipped. Proving that for a ≤19s (~10%) saving is not worth the risk.

Rejected: adding `t.Parallel()` to `simgh`, and splitting it into matrix shards.

### Selection's saving over recent history (R6/R5)

Method (reproducible, throwaway — not shipped): take the 120 most recent first-parent
merge commits on `main`; for each, list `git diff --name-only <merge>^1 <merge>`; build the
import graph with
`go list -e -test -f '{{.ImportPath}}|{{.Imports}} {{.TestImports}} {{.XTestImports}}' ./...`
(`-test` is required, otherwise test-only edges such as the root package's `cmd` import
are missed); map each changed file to its nearest owning package; take the reverse-
dependency closure. Files under `tests/e2e` (build-tagged, absent from a plain
`go list ./...`), `scripts/`, `.github/`, `.fabrik/`, `go.mod`/`go.sum` and any other
unowned path fail closed to the full suite. Two policies were compared:

| Policy for `adrs/**`, `docs/**`, `specs/**`, root `*.md` | Full suite (fail closed) | Narrowed | No Go tests | Narrowed PRs that avoid both `engine` and `tests/sim` | Avg. packages selected (of 31), narrowed PRs |
|---|---|---|---|---|---|
| Treated as inert (allowlist; `docs/state-machine.md` and `CLAUDE.md` mapped to `plugin`, which reads them) | 29 (24%) | 86 (72%) | 5 | 5 (~4% of PRs) | 10.8 |
| Strict — any unowned non-Go file fails closed | 52 (43%) | 68 (57%) | 0 | 6 (~5% of PRs) | 8.3 |

The 5–6 PRs that avoid `engine` and `tests/sim` are the only ones that skip the ~125s
and ~93s packages. Most narrowed PRs still select both, because `engine` is the most
frequently changed package and `github`, `stages`, `plugin` and `internal/*` all feed it.
An earlier hand analysis during Research found 10 of 120; either way it is a small
minority — on the order of 1 PR in 12–24 saves real time.

**Premise correction 2 — reverse-dependency count of `github/`.** The issue states 15.
Computed today with `go list -deps` per package it is 14, and 14 again with `-test -deps`
(and with `-tags e2e`). The exact number is immaterial here; it is why AC2 says
"computed, not hardcoded".

**What the replay cannot show.** R5 asks that the selection be a *superset of the packages
whose tests actually exercise the change*. The import graph cannot establish that: tests
exercise files, embedded assets and testdata through paths `go list -deps` never sees.
Known examples today: `plugin/labels_drift_test.go` reads `../docs/state-machine.md`,
`plugin/rebase_reset_regression_test.go` reads `../CLAUDE.md`, and
`tools/list-marketplace-plugins/main_test.go` reads `../../.claude-plugin/marketplace.json`.
The only way to check the superset property directly is to run the full suite for every
sampled PR and compare — an expensive check that would itself be the thing selection is
meant to avoid.

## Decision

1. **Do not build the selector.** The saving materially reaches a small minority of PRs,
   while the failure mode is silent coverage loss in a repository that merges its own
   work.
2. **Do not shard or parallelise `simgh`.** At most ~19s of saving, against real
   `t.Setenv`/timing constraints and the requirement to preserve coverage exactly.
3. **Keep `ci.yml`'s job structure and `go test` step unchanged**, so the `Test and vet`
   check reports on every PR exactly as before (R7/AC8).
4. **Pin R3 with a test.** `scripts/release_gate_full_suite_test.sh` (wired into `ci.yml`)
   asserts that `scripts/cut-release.sh` and `scripts/e2e/run.sh` still run their full
   suites and reference no selection mechanism. Nothing is narrowed today, but the guard
   means a future selector cannot leak into the release gate unnoticed (AC5). This follows
   the reasoning of ADR 1454: a cheap layer never replaces the full one at the release
   gate.

Under AC7, this ADR satisfies AC1–4 and AC6 (no selector ships, so there is nothing to
exercise them against); AC5 is covered by the guard test; AC8 and AC9 hold because the
workflow's job and test step are untouched.

## Why not selection — the case in full

- **The only version that pays off relies on a hand-maintained list.** With a strict
  "unowned file ⇒ full suite" rule, 43% of PRs fall back to the full suite, largely
  because CLAUDE.md requires docs and ADR edits in most PRs. Selection saves anything
  only once `docs/**`, `adrs/**` etc. are allowed to be inert — and then the three known
  cross-package `_test.go` readers must be registered by hand too. That is precisely the
  silently rotting list the issue itself warns against.
- **`tests/e2e` is a blind spot of the default graph.** It carries the `e2e` build tag and
  does not appear in a plain `go list ./...`; a change there must be mapped explicitly or
  fail closed.
- **The diff basis needs care.** `actions/checkout` defaults to depth 1. A correct basis
  needs `fetch-depth: 2` and `HEAD^1` of the synthetic merge commit, with a fail-closed
  fallback for merge-train integration PRs or any missing parent.
- **Asymmetric risk.** A wrong skip lets a defect reach `main`; the gain is a couple of
  minutes on a small share of PRs.

## Preferred alternative, not built: a `main`-scoped Go test cache

Go's own build/test cache already does what selection would do, per package and against
the true dependency closure (including embedded files and files a test opened) — CI logs
show `ok … (cached)` for unchanged packages. Its weakness is where the cache comes from:
`ci.yml` triggers only on `pull_request`, so `actions/setup-go`'s cache is scoped to
`refs/pull/N/merge`, and no default-branch cache exists for a PR's first run to restore.
Producing and keying a `main`-scoped cache (a `push`-to-`main` job, or a run-id key with
`restore-keys`) would give correct-by-construction reuse with no silent-coverage-loss
mode. It is a workflow and caching change, outside this issue's scope (build caching is
listed out of scope). **If CI latency becomes a problem, file it as its own issue.**

## Revisit if

- `engine` or `tests/sim` runtime grows toward the per-package `-timeout 5m`.
  (Noted, out of scope: locally under heavy load `engine` reached ~290s, and one
  `tests/sim` run hit a 10-minute timeout in
  `TestMergeTrainEjection_NoQueuedMemberWithoutComment`, passing on rerun — both looked
  load-induced and did not reproduce on CI.)
- #1439 moves Pruefer-only code out of `github/`, shrinking its blast radius so more PRs
  narrow meaningfully.
- The shape of PR traffic changes so that a large share of PRs touch only leaf packages.
- The `main`-scoped cache above is tried and proves insufficient.

If selection is re-planned, the Research findings behind this decision are the starting
point: `go list -e -test` with `TestImports`/`XTestImports`; `fetch-depth: 2` with `HEAD^1`
as the diff basis; fail closed for `tests/e2e` and every unowned path; an explicit registry
of cross-package `_test.go` readers, backed by a guard test that fails when a test reads a
`../` path not in the registry.

## Consequences

- CI stays whole-module; every PR pays the ~145–200s `go test` step (a cold run can be
  slower than cached reruns).
- ADR 1454's R7 ("CI placement") stays accurate: layers 1–3 share one `go test -race ./...`
  on every PR. It carries a pointer to this ADR.
- The release gate (`scripts/cut-release.sh`, `scripts/e2e/run.sh`'s `run_pregate`) is
  unchanged and guarded against future narrowing.
