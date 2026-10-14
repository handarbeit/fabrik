# Feature Specification: Skip the Go test suites when a PR changes no Go build input

**Feature Branch**: `fabrik/issue-2254`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description (verbatim original request):

> ## Problem
>
> CI's `Test and vet` and `Test in a worker-shaped environment` jobs run the whole Go module, including `go test -race ./...` with the sim package, on every PR. A PR that changes only docs or shell scripts waits about 20 minutes for results its diff cannot affect. That wait now applies to most release-prep and gate-maintenance PRs. For example, #2253 changed only `cut-release.sh` and some docs, plus one Go comment.
>
> [ADR 1857](adrs/1857-no-per-pr-test-selection.md) chose to have no per-PR test selection. Its concern is selecting a subset of *Go* tests from the import graph, which can miss a dependency. Skipping the Go suites when the diff touches no Go build input is a different question with a sound answer: a change that cannot alter `go build` or `go test` output cannot change their result.
>
> Testing efficiency is a priority for 0.0.85: the release gate has grown to many hours, and every saved CI cycle shortens the merge train and the release-prep loop.
>
> ## Requirements
>
> - **R1 — classify the diff.** Run a first, cheap step in each Go test job: `git diff --name-only <merge-base>...HEAD` (the base the PR or trial targets). If every changed path is outside the Go build inputs, the job skips its Go steps and **still exits success**.
>   - The skip is a step-level early exit, not a workflow `paths:` filter. A filtered workflow leaves required checks pending forever, which would block branch protection and the merge train.
> - **R2 — the list of paths that allow a skip is narrow and explicit. Fail safe.**
>   - Start from paths that are never compiled or embedded: `docs/**`, `adrs/**`, `specs/**`, `release-notes/**`, `notes/**`, top-level `*.md`, `.claude/**`, and `scripts/**` except anything a Go test reads.
>   - `plugin/**` and `stages/**` are **never** skippable: they are embedded with `//go:embed` and tested.
>   - Go test fixtures under `testdata/` are never skippable either.
>   - An unreadable diff, an unknown base, or any path not on the list means run everything.
> - **R3 — keep the checks for what did change.** Steps that test the changed non-Go files still run when those files change:
>   - `scripts/cut_release_gate_test.sh` when `scripts/cut-release.sh` or its libs change;
>   - `scripts/lib/killgroup_test.sh` when `scripts/lib/` changes;
>   - `scripts/check-adr-references.sh` when `adrs/` or docs change;
>   - the `docs-drift` workflow, unchanged.
> - **R4 — merge-train compatibility.** A trial or integration PR runs the full suite unless every member's diff qualifies. A singleton catch-up merge-commit head is classified by its diff to the base. Fast-path eligibility (green and complete own CI) must keep working with a skipped job, and the skip must report success at the real head SHA.
> - **R5 — observable.** A skipped job prints the classified file list and the reason, and its job summary says "Go tests skipped: no Go build inputs changed".
> - **R6 — amend ADR 1857** with this carve-out and its fail-safe rule. Update CLAUDE.md's layer table text ("CI's layer-1 `go test` step runs the whole module on every PR").
>
> ## Out of scope
>
> - Selecting Go tests by import graph (still rejected by ADR 1857).
> - Sharding the sim package.
> - The live e2e gate (tracked separately).
>
> ## Acceptance
>
> - A docs-only PR's `Test and vet` and worker-shaped jobs finish in under 2 minutes, green.
> - A PR touching any `.go` file, `go.mod`/`go.sum`, `plugin/**`, `stages/**` or `testdata/` runs the full suite.
> - A `scripts/cut-release.sh`-only PR runs `cut_release_gate_test.sh` and skips the Go suite.
> - A test of the classifier, written as a script test or a Go test with fixture path lists, covers both the allowed paths and the never-skip paths, and fails if the never-skip guard is neutralised.
> - A merge-train singleton fast path lands a docs-only member on the skipped job's success.

## Background

CI runs the whole Go module on every PR, with no `paths:` filter, in two jobs: `Test and vet` (gofmt, vet, hygiene lint, `go test -race ./...`, an e2e compile check, plus the shell tests for `killgroup.sh`, `cut-release.sh` and the ADR reference check) and `Test in a worker-shaped environment`. A PR that changes only non-code files waits roughly 20 minutes for results its diff cannot influence. Most release-prep and gate-maintenance PRs are of that kind (#2253 is the example). The merge train waits on the same checks, so the cost compounds across the train.

ADR 1857 decided against per-PR test selection. That decision concerns choosing a *subset of Go tests* from the import graph, which can miss a dependency. It does not address the narrower question of whether the Go suites need to run at all when no input to `go build` or `go test` changed. The answer to that is sound by construction, provided the list of "inputs" is conservative.

**Finding that shapes the skip list.** Some Go tests read files that sit outside Go source. Examples are `docs/USER_GUIDE.md`, `docs/state-machine.md`, `adrs/1966-b-fabrik-mcp-channel-events.md`, `CLAUDE.md`, `.claude-plugin/marketplace.json` and the `specs/` tree. Several others use `../` paths. A docs or ADR change can therefore change a Go test result, so `docs/**` and `adrs/**` are not skippable as blanket globs. The principle in the issue (`scripts/**` "except anything a Go test reads") applies equally to every candidate directory. The skip list must carve out the specific files Go tests read, and a guard must keep that carve-out from going stale. The enumeration of files Go tests actually read is Research's job; this spec sets the rule.

The workflow must keep producing a real, successful check at the real head SHA. The merge train's singleton fast path, catch-up classification and `classifyLandingCI` all depend on "green and complete own CI". A path-filtered workflow would leave required checks pending forever, so the skip has to be an early exit inside the job.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A non-code PR gets fast, green CI (Priority: P1)

A contributor or Fabrik opens a PR whose diff touches only paths that cannot affect any Go build or test input: for example release notes, ADRs or specs no Go test reads, or `.claude/**`.

**Why this priority**: This is the whole point of the issue. Most release-prep PRs are this shape, and each one currently costs ~20 minutes.

**Independent Test**: Run the classifier against a path list of only skippable paths. It must report "skip". Then open a docs-only PR and check that both Go test jobs finish green in under 2 minutes with the skip message in the job summary.

**Acceptance Scenarios**:

1. **Given** a PR changing only `release-notes/v0.0.85.md`, **When** CI runs, **Then** `Test and vet` and `Test in a worker-shaped environment` both complete successfully without running gofmt, vet, `go test`, the e2e compile check or the worker-shaped suite.
2. **Given** the same PR, **When** the job finishes, **Then** its log lists every classified path with the reason for the decision, and its job summary says "Go tests skipped: no Go build inputs changed".

---

### User Story 2 - Any Go build input runs everything (Priority: P1)

A PR changes at least one path that is, or might be, a Go build or test input.

**Why this priority**: The skip is only acceptable if it can never hide a failure. Fail-safe behaviour is a hard requirement, not an optimisation.

**Independent Test**: Feed the classifier one path at a time from the never-skip set, and each ambiguous or unknown path. Every case must yield "run everything".

**Acceptance Scenarios**:

1. **Given** a PR touching any `*.go` file, `go.mod`, `go.sum`, `plugin/**`, `stages/**` or any `testdata/` directory, **When** CI runs, **Then** the full suite runs exactly as today.
2. **Given** a PR touching a path that is on no list (a new top-level directory, a workflow file, `.claude-plugin/marketplace.json`), **When** CI runs, **Then** the full suite runs.
3. **Given** the diff cannot be read or the base cannot be resolved, **When** the classifier runs, **Then** the full suite runs.
4. **Given** a PR mixing one skippable file with one Go-input file, **When** CI runs, **Then** the full suite runs.

---

### User Story 3 - Non-Go checks still run for the files that changed (Priority: P2)

A PR changes only `scripts/cut-release.sh` (or its libs), `scripts/lib/`, `adrs/` or docs. The Go suites are skipped, but the shell tests and checks that cover those files still run.

**Why this priority**: Skipping the Go suites must not drop the only tests covering the changed files.

**Independent Test**: For each changed-file group, classify and check that the matching check is selected and the Go suite is not.

**Acceptance Scenarios**:

1. **Given** a PR changing only `scripts/cut-release.sh`, **When** CI runs, **Then** `cut_release_gate_test.sh` runs and the Go suite is skipped.
2. **Given** a PR changing only files under `scripts/lib/`, **When** CI runs, **Then** `killgroup_test.sh` runs.
3. **Given** a PR changing `adrs/` or docs, **When** CI runs, **Then** `check-adr-references.sh` runs, and the `docs-drift` workflow behaves as it does today.

---

### User Story 4 - The merge train works with a skipped job (Priority: P2)

A merge-train member or trial qualifies for the skip.

**Why this priority**: Required checks and "green and complete own CI" must still resolve, or the speed-up would stall the train it is meant to speed up.

**Independent Test**: In the sim, land a member whose CI reports success from a skipped-style job through the singleton fast path.

**Acceptance Scenarios**:

1. **Given** a docs-only member whose CI jobs ran the skip path, **When** the singleton fast path evaluates it, **Then** the member's own CI is treated as green and complete and the member lands.
2. **Given** a trial or integration PR whose members include one that needs Go tests, **When** CI runs, **Then** the full suite runs, because the trial's diff includes a Go build input.
3. **Given** a singleton catch-up merge commit, **When** the classifier runs, **Then** it classifies the diff against the base, not against the previous head.
4. **Given** a skipped run, **When** it reports, **Then** the check conclusion is `success` on the real head SHA, under the same required-check names as a full run.

---

### User Story 5 - The classifier cannot silently rot (Priority: P1)

A maintainer later adds a new Go test that reads a doc, or edits the skip list.

**Why this priority**: The skip list is a safety property. A stale list gives false green CI.

**Independent Test**: Neutralise the never-skip guard in the classifier; the classifier test must fail. Add a fixture path that a Go test reads to the skippable list; the guard test must fail.

**Acceptance Scenarios**:

1. **Given** the classifier test with fixture path lists, **When** the never-skip guard is disabled, **Then** the test fails.
2. **Given** a Go test that reads a non-Go file, **When** that file is on the skippable list, **Then** a test fails and names the file.

---

### Edge Cases

- Renamed or deleted files: both the old and new path must be classified (a rename out of `plugin/` is still a change to a build input).
- A PR whose merge base cannot be computed (shallow checkout, force-pushed base): classify as unknown and run everything.
- An empty diff: not treated as docs-only. Behaviour must be defined and fail-safe (run everything).
- Symlinks and paths with spaces or unusual characters: parsed safely (NUL-delimited or equivalent), never mis-split.
- Case differences and path normalisation (`./docs/x`, `docs/../plugin/x`): normalised before matching, or treated as unknown.
- A new `.go` file hidden under a skippable-looking directory (for example `docs/x/gen.go`): the `.go`, `go.mod` and `testdata/` rules take precedence over any directory rule.
- Top-level `*.md` that Go tests read (`CLAUDE.md`): not skippable even though it matches the glob.
- A PR that edits the CI workflow, the classifier or its test: never skippable, so the change to the gate is itself tested.
- Cancelled or superseded runs (the existing concurrency group): unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Each Go test job (`Test and vet`, `Test in a worker-shaped environment`) MUST begin with a cheap classification step that lists the changed paths between the merge base with the PR/trial's target base and the head.
- **FR-002**: If and only if every changed path is on the explicit skippable list and none is on the never-skip list, the job MUST skip its Go steps (gofmt, vet, hygiene lint, `go test`, e2e compile check, worker-shaped suite) and MUST still finish with a successful conclusion.
- **FR-003**: The skip MUST be implemented as a step-level early exit inside the job. The workflow MUST NOT use a workflow-level `paths:`/`paths-ignore:` filter, and the job names (the required-check names) MUST NOT change.
- **FR-004**: The skippable list MUST be narrow and explicit, and MUST be built only from paths that are never compiled, embedded or read by a Go test. The candidates are `docs/**`, `adrs/**`, `specs/**`, `release-notes/**`, `notes/**`, top-level `*.md`, `.claude/**` and `scripts/**`, each minus any specific file a Go test reads.
- **FR-005**: The never-skip list MUST include `*.go`, `go.mod`, `go.sum`, `plugin/**`, `stages/**`, any `testdata/` path, every file a Go test reads (for example the docs, ADR and `CLAUDE.md` files read by `internal/channelevents`, `plugin` and similar tests), `.claude-plugin/**`, `.github/workflows/**` and the classifier script and its test. Never-skip rules MUST win over skippable rules.
- **FR-006**: The classifier MUST fail safe: an unreadable diff, an unresolvable base, an empty result, a path it cannot parse or normalise, or any path on neither list MUST result in running everything.
- **FR-007**: When a changed file group has its own non-Go check, that check MUST still run in a skipped job: `cut_release_gate_test.sh` when `scripts/cut-release.sh` or its libs change; `killgroup_test.sh` when `scripts/lib/` changes; `check-adr-references.sh` when `adrs/` or docs change.
- **FR-008**: The `docs-drift` workflow MUST be unchanged.
- **FR-009**: For merge-train trial and integration PRs, the classifier MUST be run on the actual diff of that PR to its base. The full suite runs unless that whole diff qualifies. A singleton catch-up merge-commit head MUST be classified by its diff to the base.
- **FR-010**: A skipped job MUST report a `success` conclusion on the real head SHA under the same check names as a full run, so that `classifyLandingCI`, `ciSuiteHold` and the singleton fast path treat it as green and complete.
- **FR-011**: A skipped job MUST print every classified path with the reason for the decision, and write a job summary stating "Go tests skipped: no Go build inputs changed". A non-skipped job MUST print why it is running everything (first offending path, or the fail-safe reason).
- **FR-012**: The classifier MUST be covered by an automated test, as a script test or a Go test with fixture path lists. The test MUST cover allowed paths, never-skip paths, mixed lists, unknown paths and the fail-safe cases, and MUST fail if the never-skip guard is neutralised.
- **FR-013**: A guard test MUST fail if a file read by any Go test appears on the skippable list, so that adding a new reading test cannot silently make CI skip it.
- **FR-014**: ADR 1857 MUST be amended with this carve-out and its fail-safe rule, and CLAUDE.md's test-layer text ("CI's layer-1 `go test` step runs the whole module on every PR") MUST be updated to match. Since canonical doc pages may change, `docs/llms-full.txt` MUST be regenerated if any bundled page is edited.

### Key Entities *(if applicable)*

- **Skippable list**: The explicit, reviewed set of path patterns whose changes provably cannot affect `go build`/`go test` results.
- **Never-skip list**: Path patterns that always force the full suite and take precedence over the skippable list.
- **Classification result**: `skip` or `run-everything`, plus the per-path reasons and the set of non-Go checks to run.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A PR whose diff is entirely skippable finishes both Go test jobs green in under 2 minutes of wall time, versus about 20 minutes today.
- **SC-002**: A PR touching any `.go` file, `go.mod`/`go.sum`, `plugin/**`, `stages/**` or `testdata/` runs the full suite, with no change in what is run compared to today.
- **SC-003**: A `scripts/cut-release.sh`-only PR runs `cut_release_gate_test.sh` and does not run the Go suite.
- **SC-004**: The classifier test passes on both allowed and never-skip fixture lists, and fails when the never-skip guard is removed.
- **SC-005**: A docs-only merge-train singleton member lands through the fast path on the skipped job's success, and a trial including any Go-input member still runs the full suite.
- **SC-006**: No required-check name changes, and no PR is left with a pending check as a result of the skip.

## Assumptions

- The skippable list is defined by what is *not read by any Go test*, so the carve-outs within `docs/`, `adrs/`, `scripts/` and top-level `*.md` come from an enumeration of the Go tests' file reads. Research produces that enumeration, and FR-013's guard keeps it current. As a consequence, a PR editing `docs/state-machine.md`, `docs/USER_GUIDE.md`, `CLAUDE.md` or another file a Go test reads will run the full suite. That is correct, and a deliberate narrowing of the issue's blanket `docs/**`.
- The "under 2 minutes" acceptance applies to PRs whose diff is entirely skippable under that narrowed list (for example release notes, specs, most ADRs, `notes/`).
- The diff base is the PR's target base (or, for trial and integration PRs, the base the trial targets). The classifier needs enough git history for a merge-base calculation, and falls back to running everything when it cannot get it.
- Other workflows (`docs-drift`, `release`, `route-community-issues`) are not changed by this issue.
- The `.github/workflows/**` and `.claude-plugin/**` paths are treated as never-skip. This is low-impact and reversible, and consistent with the fail-safe rule.
- Where the classifier lives (script or Go) is a design choice for Plan, constrained only by FR-012 and by being cheap to run as the first step of a job.

## Out of Scope *(optional)*

- Selecting Go tests by import graph (still rejected by ADR 1857).
- Sharding the sim package.
- The live e2e gate (tracked separately).
- Changing which tests run when Go inputs *do* change.
- Changing the `docs-drift`, release or community-routing workflows.

## Source References *(optional)*

- `adrs/1857-no-per-pr-test-selection.md`: the decision this issue carves an exception into.
- `.github/workflows/ci.yml`: the `test` and `worker-shaped` jobs.
- `scripts/cut_release_gate_test.sh`, `scripts/lib/killgroup_test.sh`, `scripts/check-adr-references.sh`: the non-Go checks that must keep running for their files.
- Go tests that read non-Go files (examples, not exhaustive): `internal/channelevents/docs_test.go`, `plugin/labels_drift_test.go`, `plugin/rebase_reset_regression_test.go`, `plugin/pm_plugin_test.go`, `engine/spec_persist_test.go`.
