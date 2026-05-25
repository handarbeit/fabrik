# Post-Mortem: v0.0.66 Broken GraphQL Mutation

**Feature Branch**: `fix/v0.0.66-postmortem`
**Created**: 2026-05-25
**Status**: Draft
**Source**: Ported verbatim from issue #799 body (retroactively, per the spec discipline introduced after the issue was filed).

---

## Problem

The cross-repo sub-issue decomposition feature (#762 / #780) shipped in v0.0.66 with a broken `AddBlockedByIssue` implementation in `github/project.go`. The code called a GraphQL mutation that does not exist in GitHub's API:

| What we shipped | What actually exists |
|---|---|
| `addBlockedByIssue(input: {issueId, blockedById})` | `addBlockedBy(input: {issueId, blockingIssueId})` |

The bug was caught only when a real user triggered the code path in production. PR #798 fixed the call site. This issue is the post-mortem: how did a never-working API call survive Implement, Review, and Validate? What systemic changes prevent this class of bug from recurring?

The broken code carried a comment claiming empirical verification against the live schema — but the verification was evidently performed against the *read* field (`Issue.blockedBy`), not the write mutation. The original spec (`specs/sub-issue-decomposition/spec.md`) had the correct mutation name; the divergence was introduced in implementation and was never caught by review.

## Summary

Produce a written post-mortem committed to `docs/postmortems/v0.0.66-broken-graphql-mutation.md`, covering why each pipeline stage failed to catch the broken mutation, adopting at least one concrete systemic change to prevent recurrence, and auditing all currently-defined GraphQL mutations in `github/*.go` against the live schema to confirm no other broken-but-uncalled mutations exist.

## Requirements

- **Post-mortem document** committed to `docs/postmortems/v0.0.66-broken-graphql-mutation.md`, answering the six investigation questions in Investigation Scope below.
- **Mutation audit**: every GraphQL mutation in `github/*.go` verified against the live GitHub schema. The six known mutations are:
  1. `addProjectV2ItemById` (project.go)
  2. `addBlockedBy` (project.go, fixed in PR #798)
  3. `updateProjectV2ItemFieldValue` (status.go)
  4. `archiveProjectV2Item` (status.go)
  5. `markPullRequestReadyForReview` (prs.go)
  6. `resolveReviewThread` (comments.go)
- **At least one concrete systemic change** adopted and committed (the mutation audit itself is the minimum; a CI schema-validation step is the strongest candidate — see systemic options below).
- **Test coverage for `AddBlockedByIssue`**: the function currently has no unit test in `github/mutations_test.go`; a test must be added.

## Investigation Scope

The post-mortem must cover these six questions:

1. **Why did unit tests not catch this?** The hypothesis is that tests mock the GitHub client and assert on call shape, not on whether the GraphQL document is valid against the real schema. Confirm whether a test for `AddBlockedByIssue` existed; if not, why not.

2. **Why didn't Implement notice?** Determine whether the Implement-stage Claude session was ever expected to run mutations against the real API, or whether "compiles + unit tests pass" was (and should be) the full bar.

3. **Why didn't Review catch the spec/implementation divergence?** The spec had the correct mutation name; the implementation had the wrong name. Was Review prompted to diff spec against implementation? Should it be?

4. **Why didn't Validate catch it?** Validate ran tests (which passed, because mocks) and rebased. Same structural blind spot as question 1.

5. **What systemic change(s) would prevent this?** Evaluate the options and adopt at least one:
   - **Schema validation in CI**: introspect GitHub's GraphQL schema and validate every mutation/field in `github/*.go` exists. Requires a GitHub PAT stored as a CI secret — feasibility and token requirements should be assessed.
   - **Integration tests against a real test repo**: a dedicated test org/repo for mutation smoke tests. Slow; catches what mocks miss.
   - **Review-stage spec→implementation diff**: if a spec exists, Review should explicitly compare mutation/function names between spec and code.
   - **Verified-API-call comment convention**: require either "verified live YYYY-MM-DD" or "untested — see issue #N" on every GraphQL document string.

6. **Audit: are there other broken-but-never-called mutations?** Run the audit from the mutation list above and document results in the post-mortem.

## Scope

**In scope:**
- `docs/postmortems/v0.0.66-broken-graphql-mutation.md` committed to the repository
- Unit test for `AddBlockedByIssue`
- Mutation audit (all 6 mutations above)
- At least one systemic change: either a CI schema-validation step, a code convention, or a Review-skill prompt update

**Out of scope:**
- The bug fix itself (PR #798)
- The worktree-manager startup warming bug (#797, independent root cause)
- Refactoring the full `github/` test suite for all mock-based tests

## Naming Convention

Post-mortem files live in `docs/postmortems/` and follow this naming scheme:
- Version-tied incidents: `v<version>-<short-slug>.md` (e.g., `v0.0.66-broken-graphql-mutation.md`)
- Non-version-tied incidents: `<YYYY-MM-DD>-<short-slug>.md`

## Definition of Done

Validate should confirm:
- `docs/postmortems/v0.0.66-broken-graphql-mutation.md` exists and the filename contains both the version and a descriptive slug.
- The document contains sections addressing each of the six investigation questions (why tests didn't catch it, why Implement didn't notice, why Review didn't catch the divergence, why Validate didn't catch it, systemic prevention, mutation audit results).
- A unit test for `AddBlockedByIssue` exists in `github/mutations_test.go` and passes.
- At least one concrete systemic change is adopted and committed (documented in the post-mortem).
- All 6 GraphQL mutations are confirmed to exist against the live schema (documented in the post-mortem).

## Prior Art / Context

- `specs/sub-issue-decomposition/spec.md` — contains the correct mutation name; confirms the bug was introduced at implementation time, not specification time.
- `github/mutations_test.go` — currently has no test for `AddBlockedByIssue`, confirming the hypothesis that the function was never unit-tested against a mock.
- GitHub's schema asymmetry (read field `Issue.blockedBy`, write mutation `addBlockedBy`) is now documented in a code comment at `github/project.go:1485–1488`, verified via schema introspection on 2026-05-24.
- GitHub's GraphQL introspection endpoint (`api.github.com/graphql` with `__schema` query) is accessible with any valid PAT and returns the full schema. Schema-validation tooling (e.g., `graphql-inspector`, custom `go generate` step) could be added to CI but requires a token stored as a CI secret.

## Risks / Dependencies

- CI schema validation requires a GitHub PAT stored as a secret. If the project's CI doesn't currently have a suitable token, this is a prerequisite for the schema-validation approach.
- The post-mortem's answer to "why didn't Review catch the divergence?" may call for a change to the `fabrik-review` skill prompt. That change should be made in this issue if adopted, or explicitly filed as a follow-up if deferred.

## Related

- PR #798 (handarbeit/fabrik) — the fix
- #797 — sibling pre-Implement spawn bug, independent root cause
- #762, #780 — the spawn / decomposition feature this regressed in
