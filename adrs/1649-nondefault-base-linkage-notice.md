# ADR 1649: Non-Default-Base Linkage Notice

**Date**: 2026-08-24
**Status**: Accepted
**Issue**: #1649 — feat(engine): name the PR in a comment when base:<branch> suppresses GitHub's PR↔issue linkage

## Context

When a PR targets a **non-default** base branch, GitHub creates **no PR↔issue link at all** —
not merely no auto-close. Per [GitHub's docs](https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/linking-a-pull-request-to-an-issue),
closing keywords "are interpreted only when the pull request targets the repository's default
branch… otherwise these keywords are ignored, no links are created". So for a `base:<branch>`
item, the issue's Development panel shows no PR, and an operator scanning the board has no
visual cue that work exists — only a `cross-referenced` timeline event, easy to miss among stage
comments and label churn.

ADR-1096/ADR-1097 already cover the *closing* half of this same root cause: `closeIssueIfNonDefaultBase`
explicitly closes such an issue on a confirmed merge, with `fabrik:awaiting-close`/`settleNonDefaultBaseCloses`
durably retrying a failed close. Fabrik's own PR discovery is unaffected either way — `FetchLinkedPR`
resolves by the `fabrik/issue-N` head-branch convention, never GitHub's linkage graph. What was
missing is purely the human-visible half: nothing ever told the operator which PR belongs to a
non-default-base issue. Reported as the "adjacent, lower priority" section of #1646.

## Decision

Add a new settle scan, `settleNonDefaultBaseLinkageNotice` (`engine/nondefault_base_linkage_notice.go`),
wired into `poll()` immediately after `settleNonDefaultBaseCloses`. When an item's resolved base
(`base:<branch>` via `baseBranchForItem`) differs from the repository's actual default
(`wm.DefaultBaseBranch()`) and a PR has already been discovered for it (`item.LinkedPRNumber != 0`,
already populated on the board snapshot — no new fetch), it posts a one-time comment naming the PR
and explaining the GitHub linkage gap, then adds a new durable label,
`fabrik:nondefault-base-pr-noted`.

The base-vs-default detection is a near-verbatim reuse of `closeIssueIfNonDefaultBase`'s own
computation and its non-panicking `WorktreeManager` lookup pattern (skip-and-log if no
`WorktreeManager` is registered for the item's repo yet — self-heals on a later poll).

Unlike ADR-1097's `fabrik:awaiting-close`, this label is **not** an outstanding-action marker with
retry/escalation semantics. It is written unconditionally right after the comment-post attempt —
whether or not that attempt actually succeeded — mirroring `fabrik:api-key-helper-detected`'s
accepted "one-shot, no retry" shape rather than the heavier `recordSettleRetry`/`escalateSettle`
machinery ADR-1097 built for the close action.

## Rationale

### Why no retry/escalation machinery, unlike its ADR-1097 sibling?

`fabrik:awaiting-close`'s retry loop exists because a stalled explicit close has a real downstream
cost: dependent issues that unblock on close (not merge) stay blocked indefinitely until the close
eventually succeeds. A missed one-time informational comment carries no equivalent cost — nothing
gates on its delivery, no dependent issue waits on it, and the fact it reports (this PR has no
GitHub-native linkage) remains independently discoverable (the issue's own `base:<branch>` label, or
the `cross-referenced` timeline event GitHub still creates). Building `recordSettleRetry`/`escalateSettle`
support for this would be retry machinery in search of a problem it doesn't have — R2's "must
survive restarts and repeated polls without duplicating" is fully satisfied by the label's own
presence; it says nothing about guaranteeing eventual delivery, unlike ADR-1097's close retry, whose
entire purpose is exactly that guarantee.

### Why a settle scan over board.Items, rather than hooking the ≥3 PR-creation call sites directly?

A PR becomes discoverable for an item through at least three different code paths (Implement's
`ensureDraftPR` self-heal call, its main-completion-path call, and `prcreate.go`'s
`FABRIK_PR_CREATE` marker handling). Hooking all three would risk exactly the divergence ADR-1096
named as its own reason for centralizing the *close* logic into one shared helper instead of
scattering it across call sites. A settle scan keyed off `item.LinkedPRNumber` becoming non-zero on
the board snapshot fires identically regardless of which path created the PR, and follows the same
"dedicated settle scan sourced from raw `board.Items`, called unconditionally from `poll()`"
shape as every other member of this family (ADR-1270).

### Why a new label, not reusing `fabrik:awaiting-close`?

The two labels have incompatible lifecycles. `fabrik:awaiting-close` means "a close call is
outstanding and must eventually succeed"; once cleared, it's cleared for good and reapplying it
means a *new* close failed. This notice's label means "have we ever posted this" — a permanent,
one-way marker with nothing to retry toward. An item can legitimately need the notice without ever
needing `fabrik:awaiting-close` (e.g. its close succeeded on the first try, or the item isn't even at
Done yet when the PR is first discovered), so conflating the two would make one label answer two
unrelated questions.

### Why post the notice for a closed issue too?

`closeIssueIfNonDefaultBase` treats an already-closed issue as nothing-to-do because *closing* an
already-closed issue is a redundant, idempotent-by-necessity action. Posting an *informational*
comment is not the same kind of action — the fact remains true and useful regardless of how or when
the issue was closed (by the engine's own explicit close, by GitHub's ordinary mechanism on a
default-base issue that later got relabeled, or by a human). R1 does not exclude closed issues, so
this scan doesn't either.

### Why no extra API round-trip (R4)?

Every input this feature needs is already in hand at settle-scan time: `item.Labels` (for the
`base:<branch>` label and the idempotency check) and `item.LinkedPRNumber` (sourced from the
deep-fetch cache's `FetchLinkedPR`, which already runs for other reasons) both come straight off the
board snapshot passed into `poll()`. Base resolution itself
(`baseBranchForItem`/`wm.DefaultBaseBranch()`) is local git subprocess work, not a new GitHub API
call — identical to `closeIssueIfNonDefaultBase`'s existing cost profile.

## Consequences

**Positive:**
- Closes the human-visibility gap #1646 reported: an operator looking at a `base:<branch>` item's
  issue thread now sees which PR belongs to it, without having to go hunting for a
  `cross-referenced` timeline event.
- No change to `FetchLinkedPR`, PR discovery, or the `base:<branch>` resolution convention — this is
  purely an additive, informational side effect.
- Reuses `closeIssueIfNonDefaultBase`'s exact detection logic and non-panicking `WorktreeManager`
  lookup pattern, so there is no new architectural surface to reason about.

**Negative / Trade-offs:**
- A transient `AddComment` failure permanently forecloses the one-time notice for that item — there
  is no retry. Accepted: see Rationale above for why this gap is qualitatively different from, and
  lower-severity than, `fabrik:awaiting-close`'s durability requirement.
- Comment ordering is not guaranteed relative to other stage-completion comments — the notice fires
  as soon as `LinkedPRNumber` is first observed non-zero on a board snapshot, which may be before or
  after a stage's own completion comment posts. Cosmetic only; the notice's wording is accurate at
  any point after PR creation, draft or ready.

## Sibling Audit

Structurally the newest member of the ADR-1270 settle-scan family (`board.Items`-sourced,
unconditional, called from `poll()`), but deliberately the *lightest* — no retry counter, no
escalation path, no interaction with `transientLifecycleLabels` or dispatch-suppression machinery.
Nothing about merge-train basing (#1647/#1648) is touched or assumed; an item's `base:<branch>`
label and `LinkedPRNumber` are populated identically whether `merge_train` is on or off.

**References:** [ADR-1096: Explicit Close on Non-Default-Base Merge](1096-explicit-close-on-nondefault-base-merge.md),
[ADR-1097: Non-Default-Base Close Retry](1097-non-default-base-close-retry.md),
[ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md) (the settle-scan family
pattern), issue #1646 (the original community report), issue #1649 (this feature).
