# ADR 2331: Pruefer falls back to fetching the selected head SHA when `refs/pull/<N>/head` is missing

## Status

Accepted (#2331). Cites ADR 1113 (single-use depth-1 clone) and ADR 2192
decision 9 (`FETCH_HEAD` removal); amends neither.

## Context

`CloneForReview` fetched only `refs/pull/<N>/head`. GitHub does not always
create that ref (observed in the 0.0.86 release gate on
`handarbeit/fabrik-test-delta#12`: `refs/pull/12/merge` and the head branch
existed, `refs/pull/12/head` did not). Every attempt failed with
`couldn't find remote ref`, so the PR was never reviewed.

## Decisions

1. **The pull-ref fetch stays first and unchanged.** Same argv
   (`fetch --depth 1 -q <url> refs/pull/<N>/head`), same order, same error text.
   A test pins the git invocation sequence for a PR that has the ref.
2. **Fallback only on a missing ref.** If that fetch fails with git's
   `couldn't find remote ref` and the selected head SHA is a full SHA (40 or 64
   lowercase hex), the clone fetches that SHA at depth 1 from the **same URL**.
   Any other failure (auth, network, timeout, cancellation) fails as before.
   The trigger is a substring match on the error text, because `runGit` has no
   typed errors and git exits 128 for many causes. Under a non-English git
   locale the fallback simply does not trigger, which is today's behaviour.
   `LC_ALL=C` is not forced, as that would change the primary path's
   environment.
3. **Only the selected commit is reviewed.** `CloneFunc`/`CloneForReview` take
   the head SHA `ReviewPR` selected on (`pr.HeadSHA`). After either route, the
   clone runs `git rev-parse HEAD` and fails the attempt (cleaning up, returning
   no directory) if it differs. This verification also applies on the healthy
   pull-ref route: if the PR was pushed between selection and clone, the
   attempt now fails closed and the next poll re-selects the new head, where
   before the review was computed from newer code but submitted against the
   older SHA. `rev-parse` is the only additional git command on the healthy path.
4. **No SHA, no fallback, no verification.** `ReviewPR` can see an empty
   `HeadSHA`; failing every such clone would be a regression. This is the one
   way an unverified clone is still possible, and it is the pre-change behaviour.
5. **Fork PRs.** The fallback never contacts the fork: it fetches from the base
   repository URL the review already targets, so the token's read reach does
   not widen. For a fork PR whose pull ref is missing, the review succeeds only
   if the base repository serves the fork's head commit by SHA; otherwise the
   fallback fetch fails and the attempt fails as it does today.
6. **Logging.** Each attempt logs `[pr#N clone] route=pull ref` or
   `route=head SHA fallback` once.
7. **`FETCH_HEAD` is removed on both routes** after checkout (ADR 2192
   decision 9), and the token stays redacted from all error text, including the
   combined fallback error.

## Consequences

- `cloneRef` keeps its signature as a wrapper over `cloneRefAt` with no SHA, so
  its existing tests are unchanged.
- Local unit tests cannot prove GitHub's server-side reachability rule: a local
  repository serves any object it holds. That GitHub serves reachable commits
  by SHA is an assumption taken from the issue; only a live check confirms it.
- Selection and retry cadence are unchanged. A failed clone is an inconclusive
  outcome and is retried by the existing poll cadence.
