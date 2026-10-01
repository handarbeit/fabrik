# ADR 1933: One Per-Test Registry for the Live E2E Suite, with an Enforced Sim-Parity Field

**Date**: 2026-10-01
**Status**: Accepted
**Issue**: #1933 — Sim/live e2e parity manifest: map every live scenario to its sim twin or a live-only reason, enforced in go test

## Context

The sim bed (`tests/sim`) is the fast, $0 pre-gate for the live e2e suite (ADR-1454). The
pre-gate only buys confidence where the two suites cover the same behaviour, and nothing
recorded or enforced that: the one place it was written down, the prose "Coverage matrix"
in `tests/sim/README.md` (#1450), was per *file*, frozen, unchecked, and had drifted. A
live scenario with no sim twin (e.g. `TestCommentReentryShowsReworking`) can regress past
the pre-gate and surface only after a ~2h live leg.

Later issues in the e2e gate chain (#1972 ledger, #1975 packs, #1977 exclusivity) each need
per-live-test metadata. Three separate lists would drift apart.

## Decision

1. **One registry, keyed by live-test name**, at `tests/e2e/registry/registry.json`. Its
   first field is the sim-parity mapping: `parity` is `sim` (with a `sim` list of top-level
   `Test*` names in `tests/sim`), `live-only` (with a reason from a fixed vocabulary:
   `model-judgement`, `wire-format`, `review-bot`, `real-ci`, `app-auth`, `other`+note), or
   `gap` (no twin and no legitimate live-only reason — allowed, but counted). Later issues
   **add named fields to the same entries**; they do not create their own registries. The
   top level is `{"version": 1, "tests": [...]}` (never a bare map), sorted by name.

2. **JSON data file plus an untagged Go validator**, not a Go map with a `go run` dumper.
   `jq` reads it directly, so `scripts/e2e/run.sh` needs no Go in its path (and cannot
   collide with the PATH-shadowed fake `go` its shell tests use); the future Go gate runner
   can embed it. The Go package decodes with `DisallowUnknownFields`, so a typo in a field
   name fails the test. JSON has no comments; `note` carries the rationale.

3. **A subpackage `tests/e2e/registry`, outside the `e2e` build tag.** Every other file in
   `tests/e2e` is `//go:build e2e`; the completeness check must run in plain `go test ./...`
   on every PR (ADR-1857). Live and sim test names are therefore found by **parsing source**
   with `go/parser`, never by importing either package.

4. **Which tests are live is decided by rule:** a top-level `Test*` function in a
   `tests/e2e/*_test.go` file whose body — nested closures included — calls `LoadEnv`. A
   `LoadEnv` call anywhere else (reached through a helper, or at package level) is a hard
   error rather than a silent exclusion, so an unusual shape is caught instead of hiding a
   live test from the check.

5. **Enforced:** an unmapped live test, a stale entry, a dangling sim reference and a
   malformed entry (unknown parity or reason, empty/misplaced `sim` list, `other` without a
   note, duplicate, unsorted) each fail `go test ./...`. The check logic takes its inputs as
   parameters so each failure is proven non-vacuously with fixtures.

6. **Existence only.** The check does not verify that a mapped sim test asserts the same
   behaviour; a sim test gutted but keeping its name passes. Equivalence stays a review
   judgement. Subtests (`t.Run`) are not referenceable; an entry cites the parent test and
   says so in `note`. Unit tests elsewhere (e.g. `engine/stage_rework_test.go`) are not sim
   twins: such a test is a `gap` with the unit coverage named in `note`.

7. **Visibility:** `scripts/e2e/run.sh`'s `print_sim_parity_summary` prints
   `sim parity: N covered, M live-only, K gap` from the registry, from the dispatch guard
   *outside* `run_pregate` so it also prints when the pre-gate is skipped. It is
   informational: it always returns 0 and degrades to `unavailable` rather than print a wrong
   count, so it cannot change the pre-gate's ordering or pass criteria.

## Consequences

- `live-only` is **not** a way to retire a live test. ADR-1454 stands: every live scenario
  still runs live before every release. Use the narrowest reason; when doubtful, `gap`.
- The prose coverage matrix in `tests/sim/README.md` is marked historical and frozen; its
  fidelity prose is kept because source comments still reference it.
- Adding a live test now requires adding its registry entry (the build fails otherwise), and
  the sim twin is preferred in the same PR.
- Each `gap` is a candidate follow-up issue (blockedBy-chained, no epic); this ADR's PR lists
  them but does not file them.
- #1994's Go gate runner will later take over the summary line; the bash side stays one
  function over the JSON.
