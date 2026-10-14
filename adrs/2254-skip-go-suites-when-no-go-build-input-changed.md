# ADR 2254: Skip the Go Test Suites When a PR Changes No Go Build Input

**Date**: 2026-10-14
**Status**: Accepted
**Issue**: #2254 — ci: skip the Go test suites when a PR changes no Go build input
**Amends**: ADR 1857
**Rollout**: the classifier, rules and tests land first; the `ci.yml` gating is held in `notes/2254-ci-workflow.patch` because the GitHub App cannot push `.github/workflows/`. Until that patch is applied (and the file deleted) CI still runs the whole module on every PR and this ADR describes the intended behavior.

## Context

CI's `Test and vet` and `Test in a worker-shaped environment` jobs run the whole module,
including `go test -race ./...` with the sim package, on every PR. A PR that changes only
release notes, specs or a shell script waits about 20 minutes for results its diff cannot
affect. ADR 1857 rejected *per-PR test selection*: choosing a subset of Go tests from the
import graph, which can miss a dependency (embedded files, testdata, files a test reads).
It did not address a narrower question: whether the Go suites need to run at all when no
input to `go build` or `go test` changed. That answer is sound by construction, provided
the list of inputs is conservative.

## Decision

**A classifier runs as the first step of both Go test jobs.** `scripts/ci/classify-diff.sh`
lists the paths changed by the pull request and, if every one is on an explicit skippable
list and none is on a never-skip list, the job's remaining Go steps are skipped by their
`if:` conditions. The job still ends `success` under its own name on the real head SHA.

- **A step-level early exit, never a workflow `paths:` filter.** A filtered workflow does
  not run, so its required checks stay pending and block branch protection and the merge
  train. The job names (the required-check names) are unchanged. `classifyLandingCI`,
  `ciSuiteHold` and the singleton fast path already treat `success` as green and complete,
  so the engine needs no change (`tests/sim/mergetrain_singleton_fastpath_test.go` pins it).
- **Per-job classification, no shared `classify` job.** A `needs:` dependent that is
  skipped reports `skipped`, not `success`, and a separate job adds runner start-up time to
  every PR.
- **Bash, with a Go test around it.** Bash needs no toolchain, so it runs before
  `setup-go` and keeps the skipped path to seconds. `tests/ciskip` runs it on fixture path
  lists in plain `go test ./...`.

### The rules

The rules are one data file, `scripts/ci/go-skip-paths.txt`, read by the script and by the
guard tests, so they cannot drift. Precedence, per path:

1. An unparseable path (empty, absolute, backslash, an empty, `.` or `..` segment) runs
   everything.
2. A `never` rule runs everything. It wins over every skip rule.
3. A `skip` rule makes the path skippable.
4. A path on neither list runs everything.

The result is *skip* only if every changed path is skippable and the list is non-empty.

- **Never skippable:** `*.go`, `go.mod`, `go.sum`, `go.work*`, any `testdata/`, `plugin/**`,
  `stages/**` (both `//go:embed`ed), `tests/**`, `tools/**`, `.claude-plugin/**`,
  `.github/**`, `scripts/ci/**`, `scripts/lib/parallel.sh` (the CI steps execute these
  inside the Go jobs, so a change to the gate is itself tested), and the non-Go files Go
  tests read: `CLAUDE.md`, `docs/state-machine.md`, `docs/USER_GUIDE.md` and
  `adrs/1966-b-*.md`.
- **Skippable:** `docs/**`, `adrs/**`, `specs/**`, `release-notes/**`, `notes/**`,
  `.claude/**`, the rest of `scripts/**`, and top-level `*.md`.

This is deliberately narrower than the issue's blanket `docs/**` and `adrs/**`. Go tests
read a handful of files in those trees (`internal/channelevents/docs_test.go`,
`plugin/labels_drift_test.go`, `plugin/rebase_reset_regression_test.go`), so a blanket glob
would let CI skip the test that guards the file just edited.

### Fail safe

An unreadable diff, a `HEAD` that is not a two-parent merge commit, an unavailable base
parent, an empty diff, an unloadable rule table, a path on no list and an unparseable path
all run everything. The script exits 0 on every one of them; a missing output also runs
everything, because every Go step is gated on `skip != 'true'` and every shell check on
`!= 'false'`.

### Diff basis

Both jobs check out with `fetch-depth: 2`. For `pull_request`, `actions/checkout` gives the
synthetic merge commit `refs/pull/N/merge`: parent 1 is the live base tip, parent 2 the PR
head. The classifier reads `git diff --name-only --no-renames -z HEAD^1 HEAD` — exactly what
the merge would change. `--no-renames` reports both sides of a rename (a rename out of
`plugin/` is still a change to a build input) and `-z` handles any file name.

Merge-train trial PRs, integration PRs and singleton catch-up merge-commit heads are
ordinary `pull_request` events against `main`, so the same diff classifies each of them by
its diff to the base. A trial containing any Go-input member diffs to include that file and
runs the full suite. No train-specific code exists.

### Non-Go checks still run

A skipped job still runs the shell check for what changed: `cut_release_gate_test.sh` when
`scripts/cut-release.sh`, its test or `scripts/lib/` changed, `killgroup_test.sh` when
`scripts/lib/` changed, `check-adr-references.sh` when `adrs/`, `docs/`, `CLAUDE.md` or the
checker changed. A full run executes all three, as before. `docs-drift` is untouched.

### Observability

The classifier logs every changed path with the rule that classified it, and the reason for
the decision. A skip writes "Go tests skipped: no Go build inputs changed" to the job
summary.

### The guard against a stale list

The skip list is a safety property. `tests/ciskip/readers_test.go` has two layers: a
registry of the non-Go files Go tests read today, each asserted to run everything, and a
static scan of every `*_test.go` for string literals that climb out of the test's directory
(`../docs/x.md`, alone or as leading `filepath.Join` arguments). A file such a literal
resolves to that the rules call skippable fails the test and names both. `classify_test.go`
covers allowed, never-skip, mixed, unknown, unparseable and empty lists and real merge
commits; it also proves the guard is not vacuous: `CLASSIFY_DISABLE_NEVER_SKIP=1` makes the
classifier ignore every `never` rule, and the never-skip paths that sit under a skippable
directory then classify as skip.

**Known blind spot:** a path computed at run time (a variable, a constant in another file,
a directory walk) is invisible to the scan. The registry is the mitigation; a new reader
that the scan cannot see must be added to it by hand.

## Consequences

- A PR whose whole diff is skippable (release notes, specs, most ADRs, `notes/`, `.claude/**`,
  script-only changes) finishes both Go jobs in seconds instead of ~20 minutes.
- The benefit is smaller than the issue implies. CLAUDE.md asks for doc updates in most
  PRs, and those usually touch `docs/state-machine.md` or `docs/USER_GUIDE.md`, which run
  everything. ADR 1857's replay found 72% of PRs narrow when docs are treated as inert and
  57% when strict; the real share is lower still because the files above are not inert.
- This PR itself runs the full suite: it touches `.github/**`, `scripts/ci/**`, `tests/**`
  and `CLAUDE.md`.
- The release gate and the live e2e suite are unchanged and nothing here narrows them
  (ADR 1454). Per-PR *test selection* stays rejected (ADR 1857).
