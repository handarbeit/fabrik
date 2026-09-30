# ADR 1962: Fabrik does not depend on GitHub's closing-keyword linkage

## Status

Accepted (2026-09-30).

## Context

Fabrik put a PR's `Closes #N` keyword to two uses that depended on GitHub
acting on it:

1. **The review gate's broken-linkage check** (`handleBrokenReviewLinkage`)
   read `LinkedPRNumber` from `closedByPullRequestsReferences`. On the default
   base, a zero value paused the item as "broken PR↔issue linkage" without
   checking the PR itself. The `base:<branch>` path already confirmed against
   the PR body (#1050), because that field is structurally empty there.
2. **Landing on the default base** relied on GitHub's auto-close to close the
   issue when the PR merged. Only a `base:<branch>` landing closed it
   explicitly (ADR-1096/ADR-1097).

On 2026-09-30, from about 05:40Z, GitHub stopped doing both, with no declared
incident:

- It stopped creating closing-keyword links for new PRs. That covered 34 of 34
  e2e harness PRs, and controlled probes across two orgs, two accounts,
  `Closes` and `Fixes`, and the keyword set at creation or added by edit. It
  also covered a new personal repo, `verveguy/linkage-probe-20260930`.
- It dropped the auto-close even for PRs it had linked:
  `verveguy/liminis-context-graph` PR #633 listed #629 in
  `closingIssuesReferences`, merged, and `ClosedEvent.closer` stayed null.

Items were falsely paused, and landed items reached Done with their issues
still open.

## Decision

Fabrik treats the PR body as the ground truth and closes landed issues itself.
GitHub's linkage and auto-close are no longer relied on.

- **R1, broken-linkage check.** `handleBrokenReviewLinkage` parses the PR body
  with `FetchPRClosingIssues` on every path before pausing. It pauses only when
  the body has no closing keyword for the issue. A read error never pauses.
- **R2, default-base close backstop.** At every Done transition credited to a
  merged PR on the default base, `closeIssueIfNonDefaultBase` calls
  `guardDefaultBaseAutoClose`. That function live-reads the issue. If it is
  still open, or the read fails, the function records `fabrik:awaiting-close`.
  `settleNonDefaultBaseCloses` then closes the issue on a later poll, with
  `state_reason: completed` and a "🏭 **Fabrik — closed after merge**" comment
  naming the PR, using the existing ADR-1097 retry and escalation path.
  - **Why not close immediately.** The singleton fast path reaches the guard
    within the auto-close's own 1–2 s latency. Closing there would race GitHub
    and misreport a miss. Deferring the close to a later poll lets a working
    auto-close land first and clear the marker silently. The settle scan
    decides on a **live** `FetchIssue`, not the board snapshot, which can lag
    that close by a poll. `CloseIssue` succeeds on an already-closed issue, so
    a stale snapshot would otherwise re-close it and post a false miss. An
    unreadable state waits for the next poll. The live read runs on every
    base: a `base:` label only changes the wording of the comments.
  - **Where the issue set comes from.** It comes from Fabrik's own knowledge
    (the landed item), never `closingIssuesReferences`. When the auto-close
    fails, that field is empty too.

## Consequences

- Fabrik's landing and review-gate behaviour survive GitHub's closing-keyword
  machinery being slow, stale or absent. The comment makes each miss visible
  to the user, and the `pr-terminal` log line
  `GitHub auto-close did not fire for #N` makes the miss rate visible to the
  operator.
- When GitHub's auto-close has not fired within the poll interval, the issue
  can close one poll later than it used to.
- The cost is one extra REST read per default-base landing, plus, on a miss,
  one label write and one label removal.
- `fabrik:awaiting-close` is no longer non-default-base only. Its name and
  the `nonDefaultBase*` identifiers are kept to avoid churn. Its docs now
  cover both cases.
