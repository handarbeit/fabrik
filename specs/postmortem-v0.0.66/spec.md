# Feature Specification: Post-Mortem and Prevention for v0.0.66 Broken GraphQL Mutation

**Feature Branch**: `fix/v0.0.66-postmortem`
**Created**: 2026-05-25
**Status**: Draft
**Input**: User description: "Fabrik v0.0.66 shipped a nonexistent GraphQL mutation (`addBlockedByIssue` instead of `addBlockedBy`) that silently no-op'd a production feature. PR #798 fixed the call site. Produce a post-mortem that explains how the bug survived every pipeline stage and adopts at least one concrete systemic change to prevent recurrence. Also audit every GraphQL mutation currently in the codebase against the live GitHub schema."

## Background and Motivation *(non-mandatory context)*

The cross-repo sub-issue decomposition feature (issue #762, merged in #780) shipped in
v0.0.66 with a broken `AddBlockedByIssue` implementation in `github/project.go`. The code
called a GraphQL mutation that does not exist:

| What we shipped | What actually exists |
|---|---|
| `addBlockedByIssue(input: {issueId, blockedById})` | `addBlockedBy(input: {issueId, blockingIssueId})` |

The bug was caught only when a real user triggered the code path in production. The API
returned an error, parent issues never linked to spawned children, and the decomposition
feature silently no-op'd. PR #798 fixed the call site.

This post-mortem is about the **process failure**: how did a never-working API call pass
unit tests, Implement, Review, and Validate? The original spec
(`specs/sub-issue-decomposition/spec.md`) had the correct mutation name in four places.
The implementation diverged and no stage caught it.

Compounding the bug, the broken code carried a comment claiming verification against the
live schema — but the verification was evidently performed against the *read* field
(`Issue.blockedBy`), not the write mutation. False confidence is worse than no
confidence: it short-circuits review.

GitHub's API has an asymmetric naming convention here that contributed to the slip:
`Issue.blockedBy` is the read field but `addBlockedBy` (not `addBlockedByIssue`) is the
write mutation. A reasonable inference from the read-side name produces the wrong
write-side name.

The remaining work is concentrated in four places: a written post-mortem document, a
query-shape regression test, a CI workflow that finally enforces test gates on every PR,
and a Review-skill update directing the agent to perform an explicit spec→implementation
diff of external API names.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — A maintainer reads the post-mortem and understands every stage's blind spot (Priority: P1)

The post-mortem must answer six investigation questions in plain language and call out
which structural property of the pipeline allowed the bug at each stage.

**Why this priority**: Without a written record, the same root cause recurs in six months
and the project re-learns the same lesson. The document is the deliverable that
distinguishes "we patched a bug" from "we made the class of bug harder."

**Independent Test**: A maintainer who did not work on v0.0.66 reads
`docs/postmortems/v0.0.66-broken-graphql-mutation.md` and can answer, without reading the
code, what specifically each stage would have had to do differently to catch the bug.

**Acceptance Scenarios**:

1. **Given** the post-mortem document, **When** the reader reaches the end of the
   "Investigation" section, **Then** they can name the structural reason unit tests did
   not catch the typo (mocks accept any GraphQL document; tests assert on `variables`
   only, never on the `query` string).
2. **Given** the document, **When** the reader reaches the "Why didn't Implement notice?"
   section, **Then** they understand that `go build` + `go test` + `go vet` cannot
   validate GraphQL strings against a remote schema and that this is not a regression in
   Implement's expected bar.
3. **Given** the document, **When** the reader reaches the "Why didn't Review catch the
   divergence?" section, **Then** they see a concrete prompt-level change to the Review
   skill that would have caught the bug, and that change is committed in the same
   change set.

### User Story 2 — A query-shape regression test would have caught the bug at `go test` time (Priority: P1)

A new unit test in `github/mutations_test.go` must assert that the HTTP request body
sent to GitHub contains the correct mutation name and input field names. Variable
assertions alone are insufficient because they pass on a syntactically valid Go string
that references a nonexistent mutation.

**Why this priority**: This is the cheapest possible prevention. No new credentials, no
schema introspection, no CI changes — just an additional `strings.Contains` check inside
a test that already exists. The same pattern can guard every other mutation in the
codebase.

**Independent Test**: Revert the call-site fix on a scratch branch (re-introducing
`addBlockedByIssue` and `blockedById`) and run `go test ./github/`. The new test fails
with a clear message naming the divergence.

**Acceptance Scenarios**:

1. **Given** the new test, **When** the implementation uses `addBlockedBy` and
   `blockingIssueId`, **Then** the test passes.
2. **Given** the new test, **When** the implementation accidentally regresses to any
   nonexistent name, **Then** the test fails with a message that names the expected and
   actual mutation/field, not just a generic assertion failure.
3. **Given** future maintainers add new mutations, **Then** the same query-shape pattern
   is the standard template for testing them.

### User Story 3 — CI runs `go vet` and `go test -race` on every PR targeting main (Priority: P1)

The `wait_for_ci: true` gate in `validate.yaml` already exists. Until this issue, no CI
workflow produced check runs that the gate could observe, so the gate never fired. This
issue activates the gate by adding the workflow.

**Why this priority**: Without CI, a query-shape regression test (User Story 2) can be
committed and later removed without anyone noticing. CI makes the prevention durable.

**Independent Test**: Open any PR with a deliberate test regression. Branch protection
blocks merge until the new `ci.yml` workflow's check is green.

**Acceptance Scenarios**:

1. **Given** a PR targeting `main`, **When** the PR is opened or updated, **Then**
   `.github/workflows/ci.yml` runs `go vet ./...` and `go test -race -timeout 5m ./...`.
2. **Given** the CI run, **When** any test fails or `go vet` emits a diagnostic, **Then**
   the workflow exits non-zero and branch protection blocks the merge.
3. **Given** the `wait_for_ci: true` setting in `validate.yaml`, **When** a Fabrik issue
   reaches Validate, **Then** the engine sees check runs from the new CI workflow and
   gates on them.

### User Story 4 — Review skill explicitly diffs spec API names against implementation (Priority: P2)

When a `specs/` document exists for the feature under review, the `fabrik-review` skill
prompt must direct the Review agent to perform a character-by-character comparison of
external API names (GraphQL mutation names, REST endpoint paths, input field names,
variable names) between the spec and the implementation.

**Why this priority**: Mocked tests cannot catch a mutation name that doesn't exist on
the live schema. A human reviewer reading the spec alongside the diff *would* catch it,
but only if the reviewer is explicitly directed to perform that comparison. P2 because
it's a process change rather than a test, and is reinforced by User Story 2's regression
test anyway.

**Independent Test**: Compose a synthetic PR with a spec that says `addBlockedBy` and
an implementation that uses `addBlockedByIssue`. Run the Review stage. The agent's
output explicitly flags the mutation-name divergence.

**Acceptance Scenarios**:

1. **Given** a `specs/<feature>/spec.md` exists and a PR implements that feature,
   **When** the Review stage runs, **Then** the agent's checklist includes "compare
   external API names between spec and code: GraphQL mutation names, REST paths, input
   field names."
2. **Given** the spec and implementation disagree on a mutation or field name, **When**
   Review runs, **Then** the divergence is named explicitly in the Review output.

### User Story 5 — Mutation audit confirms no other broken-but-uncalled mutations exist (Priority: P2)

Every currently-defined GraphQL mutation in `github/*.go` must be verified against the
live GitHub schema. The audit results must appear in the post-mortem with the
verification command and the live-schema output, so the audit is reproducible.

**Why this priority**: The `addBlockedByIssue` bug went undetected because no one called
the function until a real user triggered the path. Other never-called or rarely-called
mutations could have the same defect.

**Independent Test**: Run the verification command in the post-mortem. The output names
exactly the six expected mutations, with no missing names and no additional broken ones.

**Acceptance Scenarios**:

1. **Given** the audit section of the post-mortem, **When** a future maintainer adds a
   new mutation, **Then** the audit can be re-run by copy-pasting the command and adding
   the new mutation name to the grep filter.
2. **Given** the audit results, **When** the post-mortem is committed, **Then** all six
   currently-defined mutations are listed with their source file and verification
   status.

### Edge Cases

- **Tests under `t.Parallel`**: the new test captures request fields into shared
  variables and asserts on them after the call. The captures must be guarded with a mutex
  so the race detector does not flag the test (and so the test is correct).
- **CI workflow flakiness**: if a pre-existing test is flaky under `-race` in the CI
  environment, the workflow must either skip the specific test in CI or be marked
  best-effort for that test alone. Do not let one flake block all subsequent merges.
- **Asymmetric naming**: when documenting any future GraphQL operation in ADRs or specs,
  always distinguish between the Go function name and the GraphQL mutation name. ADR 048
  needed this fix retroactively.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A post-mortem document MUST exist at
  `docs/postmortems/v0.0.66-broken-graphql-mutation.md`, answering the six investigation
  questions in plain language and committing concrete fixes in the same change set.
- **FR-002**: A regression test `TestAddBlockedByIssue_QueryShape` MUST exist in
  `github/mutations_test.go`, asserting that the HTTP request body contains the correct
  mutation name (`addBlockedBy`) and input field (`blockingIssueId`), AND that the
  nonexistent names (`addBlockedByIssue`, `blockedById`) are absent.
- **FR-003**: `.github/workflows/ci.yml` MUST run `go vet ./...` and
  `go test -race -timeout 5m ./...` on every pull request targeting `main`.
- **FR-004**: The `plugin/fabrik-workflows/skills/fabrik-review/SKILL.md` MUST include an
  explicit checklist item under "Correctness" directing the Review agent to compare
  external API names between any matching `specs/` document and the implementation.
- **FR-005**: All six currently-defined GraphQL mutations MUST be re-verified against
  the live GitHub schema, with the verification command and output captured in the
  post-mortem.
- **FR-006**: `adrs/048-spawn-child-engine-side.md` MUST clearly distinguish between
  the Go function name (`AddBlockedByIssue`) and the GraphQL mutation name
  (`addBlockedBy`) wherever both appear.

### Non-Functional Requirements

- **NFR-001**: The new test must not introduce a race-detector failure when run under
  `go test -race`.
- **NFR-002**: The CI workflow must not exceed 5 minutes wall-clock under normal
  conditions to avoid slowing down the merge pipeline.

## Key Entities

- **Post-mortem document** — single Markdown file under `docs/postmortems/`. Structured
  prose; no engine code references it. Permanent record.
- **Query-shape test** — single Go test function. Same package as the other mutation
  tests. Uses the existing `httptest.NewServer` pattern.
- **CI workflow** — single YAML file under `.github/workflows/`. Triggered on
  `pull_request` targeting `main`.
- **Review skill prompt** — single Markdown file. Read by the Fabrik engine when the
  Review stage runs.

## Out of Scope

- The bug fix itself, which was already shipped as PR #798.
- The worktree-manager startup warming bug (#797 / independent root cause).
- A live-schema CI validation step (see Recommended Follow-Ups in the post-mortem). This
  requires a CI secret PAT and should be tracked as a separate issue.
- A stricter `// Verified against live schema YYYY-MM-DD` comment convention (see
  Recommended Follow-Ups in the post-mortem). Worth doing but a separate change.

## Acceptance Criteria

- [ ] `docs/postmortems/v0.0.66-broken-graphql-mutation.md` exists and answers the six
      investigation questions
- [ ] `TestAddBlockedByIssue_QueryShape` exists and passes
- [ ] `TestAddBlockedByIssue_QueryShape` fails when the implementation uses the
      pre-#798 nonexistent names (verifiable by reverting the call-site fix locally)
- [ ] `.github/workflows/ci.yml` runs on every PR and is registered as a required
      status check on `main`
- [ ] `fabrik-review` SKILL.md has the spec→implementation API-name diff checklist item
- [ ] All six mutations in `github/*.go` are listed with verification status in the
      post-mortem
- [ ] ADR 048 uses `AddBlockedByIssue` for the Go function and `addBlockedBy` for the
      GraphQL mutation, with no remaining ambiguity

## Assumptions

- The Fabrik release pipeline (`scripts/cut-release.sh`, `.github/workflows/release.yml`)
  is unaffected by adding a new `ci.yml` workflow.
- Branch protection on `main` can be updated to require the new CI check (manual repo
  setting, post-merge).
- `httptest.NewServer` and `json.NewDecoder` remain available in the Go stdlib for the
  test infrastructure pattern used here.
