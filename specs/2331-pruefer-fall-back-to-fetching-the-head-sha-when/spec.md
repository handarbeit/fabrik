# Feature Specification: Pruefer falls back to fetching the head SHA when refs/pull/<N>/head is missing

**Feature Branch**: `fabrik/issue-2331`
**Created**: 2026-10-16
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Pruefer clones a PR for review by fetching `refs/pull/<N>/head` only (`pruefer/worktree.go` `CloneForReview`). GitHub does not always create that ref. When it is missing, every review attempt fails and the PR is never reviewed.
>
> Seen 2026-10-15 in the 0.0.86 release gate: `handarbeit/fabrik-test-delta#12` (same-repo PR, head `f8fd7201`, branch `fabrik/issue-11`). `git ls-remote` showed `refs/pull/12/merge` and `refs/heads/fabrik/issue-11`, but no `refs/pull/12/head`. The dev Pruefer logged this 7 times between 08:05 and 08:15 UTC:
>
> ```
> [pr#12 warn] reviewing handarbeit/fabrik-test-delta#12: cloning PR head: fetching refs/pull/12/head: git fetch --depth 1 -q … refs/pull/12/head: exit status 128: fatal: couldn't find remote ref refs/pull/12/head
> ```
>
> No review was posted, so the live test `TestCrossRepoSpawn` failed on its "ready for review over 10m with zero reviews" check. On a production repo, a PR whose Fabrik stage declares `expected_reviewers: [handarbeit-pruefer]` would wait out the review timeout instead.
>
> ## Requirements
>
> 1. If fetching `refs/pull/<N>/head` fails with "couldn't find remote ref", fetch the PR's head commit **by SHA** (the head SHA Pruefer already selected on) at depth 1. GitHub serves reachable commits by SHA.
> 2. After either fetch, verify the checked-out `HEAD` equals the selected head SHA. If they differ, fail the attempt (never review a commit other than the one selected).
> 3. Log which route was used once per attempt (`pull ref` / `head SHA fallback`).
> 4. Any other fetch failure keeps today's behaviour.
> 5. Keep the #2192 rule: the credentialed URL never stays readable in the clone (`FETCH_HEAD` removed).
>
> ## Acceptance
>
> - A unit test against a local file-based repo with no `refs/pull/<N>/head` but the head commit reachable: the clone succeeds via the SHA fallback, and `HEAD` equals the SHA.
> - A test where the fallback fetches a different commit than the selected SHA: the attempt fails.
> - Existing `cloneRef` tests unchanged.
>
> ## Scope
>
> Pruefer clone only. No change to selection or retry cadence.

## Background

Pruefer reviews a PR by making a single-use, depth-1 clone of the base repository and fetching only `refs/pull/<N>/head`. GitHub does not always create that ref. When it is absent, the fetch fails with "couldn't find remote ref", the review attempt fails, and Pruefer retries without ever succeeding. The PR is never reviewed.

This happened in the 0.0.86 release gate on a same-repo PR (`handarbeit/fabrik-test-delta#12`). The remote had `refs/pull/12/merge` and the head branch but no `refs/pull/12/head`, and seven consecutive attempts failed identically. The live test `TestCrossRepoSpawn` then failed because no review appeared. In production, a Fabrik stage that declares `expected_reviewers: [handarbeit-pruefer]` would sit out the whole review timeout for a PR that Pruefer could have reviewed.

Pruefer already knows which head commit it selected the PR on. GitHub serves reachable commits by SHA, so that commit can be fetched directly when the PR ref is missing. The safety property that matters is that Pruefer never reviews a commit other than the one it selected, for example after a force-push or a close in the meantime.

Additional requirements from @arbeithand (2026-10-16), folded in below:
1. The primary path stays unchanged. The SHA fetch runs only after the PR-ref fetch has failed. Every review goes through `CloneForReview`, so the existing ref fetch must be byte-identical, and a test pins this.
2. Only the selected head is reviewed. The fallback fetches exactly the selected head SHA and checks that the checked-out commit equals it. Any mismatch or failed fetch fails the review exactly as today.
3. Say what happens for a fork PR whose `refs/pull/<N>/head` is missing. If the fallback cannot cover that case, record it in the ADR and keep today's failure. Do not widen what the token reads.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Review a PR whose pull ref is missing (Priority: P1)

An operator runs Pruefer against a repository. A same-repo PR has a head commit and branch on the remote, but GitHub never created `refs/pull/<N>/head`. Pruefer still reviews the PR at the head commit it selected, so Fabrik's review gate and any release-gate test see a review instead of waiting out the timeout.

**Why this priority**: This is the reported failure. Without it a PR can go unreviewed indefinitely, and the release gate fails.

**Independent Test**: Run the clone against a local file-based repository where the head commit is reachable but no `refs/pull/<N>/head` exists. The clone succeeds and the checked-out `HEAD` equals the selected SHA.

**Acceptance Scenarios**:

1. **Given** a repository with no `refs/pull/<N>/head` and a reachable selected head SHA, **When** the review clone runs, **Then** the clone succeeds via the SHA fallback and `HEAD` equals the selected SHA.
2. **Given** that clone, **When** the attempt finishes, **Then** one log line names the route used (`head SHA fallback`).

---

### User Story 2 - Never review a commit other than the selected one (Priority: P1)

A review was selected for head SHA `A`. Before the clone completes, the PR branch is force-pushed to `B`, or the selected commit is otherwise not what was fetched. Pruefer must not post a review of `B` as if it were `A`.

**Why this priority**: A review posted against the wrong commit is worse than no review. This is a hard safety boundary of the fix.

**Independent Test**: Make the fallback fetch yield a commit different from the selected SHA. The attempt fails and no clone is returned.

**Acceptance Scenarios**:

1. **Given** the fallback fetch resolves to a commit different from the selected SHA, **When** the head is verified, **Then** the attempt fails and the clone is cleaned up.
2. **Given** the selected SHA cannot be fetched at all, **When** the fallback runs, **Then** the attempt fails with an error that says the fallback was tried, exactly as a failed review attempt fails today.

---

### User Story 3 - Healthy PRs behave exactly as before (Priority: P1)

For every PR that has a `refs/pull/<N>/head`, and for every fetch failure other than a missing ref, nothing changes.

**Why this priority**: All reviews go through this clone. The change must add no new behaviour, risk or traffic on the normal path.

**Independent Test**: The existing `cloneRef` tests pass unchanged. A new test pins that the primary ref fetch is issued first, with the same arguments as today, and that the SHA fetch is issued only after it fails with a missing-ref error.

**Acceptance Scenarios**:

1. **Given** a PR whose pull ref exists, **When** the review clone runs, **Then** only the ref fetch is issued and the log line names `pull ref`.
2. **Given** the ref fetch fails for any reason other than "couldn't find remote ref" (auth, network, timeout), **When** the clone runs, **Then** the attempt fails as today and no SHA fetch is attempted.

---

### User Story 4 - Fork PRs keep today's failure unless the base repo serves the commit (Priority: P2)

For a fork PR whose pull ref is missing, the fallback must not widen what the token can read. It fetches only from the base repository the review already targets. If the base repository does not serve that commit, the attempt fails as it does today.

**Why this priority**: The commit of a fork PR may be unreachable from the base repository, and the installation token may be unable to read the fork. Falling back to the fork's URL would widen token reach, which is out of bounds.

**Independent Test**: With the fallback fetch failing (commit not served by the base repository), the attempt fails exactly as today, and no remote other than the base repository URL was contacted.

**Acceptance Scenarios**:

1. **Given** a fork PR with no pull ref and a head commit the base repository does not serve, **When** the fallback runs, **Then** the attempt fails as today.
2. **Given** any fallback attempt, **When** the remote URL is inspected, **Then** it is the same base-repository URL as the primary fetch.

---

### Edge Cases

- The selected head SHA is empty or malformed: the fallback is not attempted and the original missing-ref failure is returned.
- The ref fetch fails with "couldn't find remote ref" and the SHA fetch fails too: the returned error identifies both steps and does not contain the token.
- The branch moved on after selection: the SHA fetch still returns the selected commit, since the commit remains reachable, or it fails. Either way it is never a different commit.
- Cancellation or timeout during the SHA fetch: handled like any other fetch failure, with cleanup.
- A credentialed URL is used in the fallback fetch: `FETCH_HEAD` is removed after checkout on this route too (#2192), and the token is redacted from all error text.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The review clone MUST first fetch `refs/pull/<N>/head` at depth 1 exactly as it does today. This primary path (arguments, order, error text on non-missing-ref failures) MUST be unchanged, and a test pins it.
- **FR-002**: Only if the primary fetch fails with git's "couldn't find remote ref" condition for the pull ref, the clone MUST fetch the head commit by SHA, at depth 1, from the same remote URL. The SHA is the head SHA Pruefer selected the review on.
- **FR-003**: The fallback MUST NOT run for any other primary fetch failure (authentication, network, timeout, cancellation, or anything else), which MUST fail as today.
- **FR-004**: After either route has fetched and checked out, the clone MUST verify that the checked-out `HEAD` equals the selected head SHA. On any mismatch the attempt MUST fail, the clone MUST be cleaned up, and no clone directory may be returned.
- **FR-005**: If the SHA fetch fails, the attempt MUST fail with an error that identifies the fallback as the failing step and still carries enough information to see the original missing-ref failure.
- **FR-006**: If no selected head SHA is available (empty or not a full commit SHA), the fallback MUST NOT run and the original failure is returned.
- **FR-007**: Each clone attempt MUST log the route that produced the clone, once, as `pull ref` or `head SHA fallback`, through the existing per-PR logging.
- **FR-008**: On both routes, `FETCH_HEAD` MUST be removed from the clone after checkout so the credentialed URL is never readable from inside the review scope (#2192), and the token MUST remain redacted from all error text and logs.
- **FR-009**: The fallback MUST fetch only from the base-repository URL already used for the primary fetch. It MUST NOT contact the fork's repository or read anything beyond what the primary fetch's token scope already allowed.
- **FR-010**: For a fork PR whose pull ref is missing, the review MUST succeed only if the base repository serves the head commit by SHA; otherwise it MUST fail as today. This limitation MUST be recorded in the ADR for this change.
- **FR-011**: Existing `cloneRef` tests MUST pass unchanged. New tests MUST cover: SHA-fallback success against a local file-based repository with no pull ref; a fallback that yields a different commit failing the attempt; the primary path being issued first and the SHA fetch not being issued when the pull ref exists; and a non-missing-ref failure not triggering the fallback.
- **FR-012**: Selection and retry cadence MUST NOT change.

### Key Entities *(if applicable)*

- **Selected head SHA**: the PR head commit Pruefer chose to review. The clone must end up at exactly this commit.
- **Pull ref**: `refs/pull/<N>/head`, GitHub's synthetic ref for a PR head. It is the primary fetch target and is not always present.
- **Review route**: which fetch produced the clone, `pull ref` or `head SHA fallback`, as reported in the log.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A PR with a reachable head commit and no `refs/pull/<N>/head` is reviewed on the first attempt, where previously every attempt failed.
- **SC-002**: In 100% of clones, the checked-out `HEAD` equals the selected head SHA, or the attempt fails. No review is ever produced from a different commit.
- **SC-003**: For PRs that have a pull ref, the sequence and arguments of git commands issued are identical to those before this change, and the existing clone tests pass unmodified.
- **SC-004**: No fetch is made to any remote other than the base-repository URL, and no credentialed URL is readable in the clone on either route.
- **SC-005**: The log for every clone attempt names its route exactly once.

## Assumptions

- GitHub serves a reachable commit by SHA at depth 1 from the base repository for same-repo PRs, as stated in the issue. The unit tests use a local file-based repository standing in for this.
- Git signals a missing ref with the "couldn't find remote ref" message in the fetch error text; this is the condition that triggers the fallback, and the match is narrow.
- Reaching the selected head SHA needs it to be passed to the clone step. The current clone step receives only the PR number, so the planning stage must carry the SHA through. How is left to Plan.
- For fork PRs, GitHub may or may not serve the fork's head commit by SHA from the base repository. The spec makes no promise beyond "succeeds if served, otherwise fails as today."
- A failing review attempt continues to be retried by the existing cadence, which is unchanged.

## Out of Scope *(optional)*

- Fetching from a fork's own repository, or any widening of the token's read reach (FR-009, FR-010).
- Any change to PR selection, head-SHA selection, retry cadence or backoff.
- Diagnosing why GitHub omitted the pull ref.
- Changes to the Fabrik engine or to any non-Pruefer clone path.
- Caching or reusing clones across reviews.

## Source References *(optional)*

- Release gate 0.0.86 failure, 2026-10-15: `handarbeit/fabrik-test-delta#12`, live test `TestCrossRepoSpawn`.
- #2192: `FETCH_HEAD` removal so the credentialed URL is not readable in the review clone.
- `pruefer/worktree.go` (`CloneForReview`, `cloneRef`) and `pruefer/worktree_test.go`.
