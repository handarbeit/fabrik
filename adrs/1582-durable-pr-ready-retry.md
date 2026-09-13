# ADR 1582: Durable Mark-PR-Ready Retry

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1582 — engine: MarkPRReady failure is never retried, can permanently strand a draft PR

## Context

`markPRReady` (`engine/pr.go`) pushes the issue branch and calls `client.MarkPRReady` to transition a
stage's draft PR to ready-for-review, after `mark_pr_ready_on_complete: true` is configured for that
stage. It already retries transient errors up to 3 times in-process (#599), but on a non-transient
error, or once that retry budget is exhausted, it logs a warning and returns. `handleStageComplete`
proceeds regardless — `stage:<name>:complete` is granted whether or not the PR ever became ready. No
settle scan, retry counter, or durable marker existed to pick the call back up.

This matters because `attemptMergeOnValidate`'s direct-merge fallback treats a draft PR's
`mergeable_state` as unconditionally `"draft"`, checked ahead of any check-run/dirty logic — so a lost
`MarkPRReady` call does not just leave the PR cosmetically in draft, it can permanently block the issue
from ever reaching Done via the direct-merge path. `tests/sim/restart_recovery_test.go`'s
`TestRestartRecovery_KillAfterPRCreatedBeforeReady` reproduced this deterministically: a PR that
genuinely exists in the simulated GitHub model, stuck in draft forever, with the issue wedged at
Validate.

This is structurally the same shape ADR-1097 (`fabrik:awaiting-close`) and ADR-061
(`fabrik:awaiting-member-close`) already solved: a single at-risk call, positioned as a fire-and-forget
side effect after the decision it supports (stage completion) has already been made — not a multi-call
chain like ADR-060's `fabrik:awaiting-done`.

## Decision

Add a new durable label, `fabrik:awaiting-pr-ready`, written **inline inside `markPRReady` itself** —
no signature change, no call-site changes at any of its three callers (`engine/item.go` ×2,
`engine/comments.go`) — in exactly the two places `markPRReady` currently just logs and returns: the
non-transient-error branch, and the retry-exhausted fallthrough at the end of its loop.
`markPRReady`'s success path clears the marker (`clearPRReadyMarker`) when it happens to already be
present, so an issue previously escalated and un-paused self-heals the moment its own next
`markPRReady` call succeeds directly, without waiting for the settle scan to notice.

The retry mechanism (`engine/pr_ready_settle.go`) is a self-contained scan, **not** wired into
`itemMayNeedWork`/`itemNeedsWork` or `deepFetchCandidates`, and **not** added to
`transientLifecycleLabels`:

- `settlePRReadyScan` (called from `poll()` in `engine/poll.go`, alongside the other ADR-1270-family
  scans) runs unconditionally every poll. It iterates the **raw `board.Items`**, checking only
  `hasLabel(item, "fabrik:awaiting-pr-ready")` and skipping items carrying `fabrik:paused`.
- `settlePRReady` re-resolves the PR live via `FetchLinkedPR` on every pass — a GitHub label carries
  no payload, and the scan runs on a completely independent poll cadence from when the marker was
  written. Idempotent short-circuits, all clearing the marker without calling `MarkPRReady` again: no
  PR found at all; the PR is closed or merged; the PR is already not-draft (self-healed by a later
  stage's own `markPRReady` call, or a human clicking "Ready for review" manually). Only a
  genuinely-still-draft, open PR triggers a real `client.MarkPRReady` retry call — safe regardless,
  since GitHub's `markPullRequestReadyForReview` mutation is a documented no-op success on an
  already-ready PR.
- Retries reuse the existing generic `recordSettleRetry`/`escalateSettle`/`clearSettleMarker` helpers
  (`engine/settle.go`), keyed by a dedicated constant, `"__awaiting_pr_ready__"` (same
  double-underscore-wrapped, YAML-unrepresentable shape as its siblings). Once `MaxRetries` is
  reached, `escalatePRReadyFailure` fires: `fabrik:paused` is added, `fabrik:awaiting-pr-ready` is
  removed, an explanatory comment naming the draft PR (`item.LinkedPRNumber`, when known) with the
  manual `gh pr ready <N>` recovery step is posted, and `itemstate.EnginePaused` is applied.

No changes to `itemMayNeedWork`, `itemNeedsWork`, `transientLifecycleLabels`, or
`attemptMergeOnValidate` are needed or made — the latter already retries indefinitely on every poll
while un-gated, so once the settle scan flips the PR out of draft, the very next poll's existing merge
attempt sees a non-draft `mergeable_state` and proceeds normally.

## Rationale

### Why a durable GitHub label, not an `itemstate.Store` mutation?

Same reasoning as ADR-060/ADR-061/ADR-1097: `itemstate.Store` does not survive a restart, and there is
no artifact to safely "redo" here except the `MarkPRReady` call itself. An in-memory-only marker would
silently lose the outstanding-mark-ready decision across a restart — precisely the gap
`TestRestartRecovery_KillAfterPRCreatedBeforeReady` was written to expose.

### Why write the marker only on failure, not unconditionally beforehand?

Identical to ADR-061/ADR-1097's reasoning: there is exactly one at-risk call
(`client.MarkPRReady`), not a chain. Writing the marker unconditionally before it would mean
adding-then-immediately-removing a label on every successful call (the overwhelmingly common case),
for no correctness benefit.

### Why write the marker inline inside `markPRReady`, rather than growing its return signature?

`markPRReady`'s void signature and logging style were deliberately preserved by #599 ("no return value
added"). Every value the marker write needs (`item`, `owner`, `repo`, the resolved `prNumber`) is
already in scope at both of `markPRReady`'s existing failure points. Growing the signature would touch
3 call sites and require every existing caller to decide what to do with a new return value, for no
benefit over writing the marker at the one place that already knows it failed.

### Why is this marker not terminal-only, unlike most of its `fabrik:awaiting-*` siblings?

`fabrik:awaiting-close`, `fabrik:awaiting-member-close`, and `fabrik:awaiting-advance` are all applied
only after the item has already reached a terminal board state (Done) — by construction, there is no
redispatch risk left to guard against, so hooking into dispatch-suppression machinery would add
complexity for no benefit. `fabrik:awaiting-pr-ready` diverges: `mark_pr_ready_on_complete: true` can
be configured on a non-terminal stage (e.g. Implement, with Review and Validate still ahead), so the
marker can be written while the item is still mid-pipeline. The item is deliberately left free to keep
advancing through subsequent stages while the marker is outstanding — nothing about a later stage's
dispatch depends on the PR being ready, and gating stage completion on `MarkPRReady` succeeding was
explicitly out of scope for #1582 (a bigger change to the conjunctive-gate model). This is the one
place this label's shape differs from its closest precedents, and is called out here, in
`docs/state-machine.md`, and in both label reference docs so a future reader does not assume it
behaves like `fabrik:awaiting-close`.

### Why a `board.Items` scan instead of a `deepFetchCandidates`-based settle-scan shape?

Consistent with every ADR-1270-family scan: `board.Items` is the raw, always-available snapshot,
independent of the `itemMayNeedWork`/`selectDeepFetchCandidates` admission machinery a settle scan of
this shape has no need for.

### Why a dedicated retry-counter constant instead of reusing an existing one?

Reusing an existing `__…__` constant would conflate two unrelated failure classes under one counter,
muddying `MaxRetries` bookkeeping. `"__awaiting_pr_ready__"` is, like its siblings, deliberately
unrepresentable as a real YAML stage `name:`, so it can never collide with a configured stage's own
counter — no new `itemstate.Mutation` type is introduced.

## Consequences

**Positive:**
- A PR that genuinely exists but never transitioned out of draft because of a lost `MarkPRReady` call
  is no longer permanently stranded — closing the exact gap #599 deferred and #1582 was filed for.
- The fix does not touch `itemMayNeedWork`, `itemNeedsWork`, `transientLifecycleLabels`, or the
  Validate merge-gate — it cannot regress the terminal-skip optimization (#689) or interact with any
  other gate label, by construction.
- Reuses the same generic settle/escalate helpers as every other member of this family, so this is
  roughly the seventh structurally identical instance of the pattern, not a new mechanism.
- `markPRReady`'s signature and every existing call site are unchanged; only its two failure branches
  and its success branch gained one call each.

**Negative / Trade-offs:**
- The marker is written only in the failure branch, not unconditionally first — an engine crash in the
  narrow window between the `client.MarkPRReady` request being sent and its response being processed
  would leave neither a marker nor a completed mark-ready, silently reproducing the original bug for
  that one crash. Accepted, consistent with every other single-call escalation site in this engine.
- `settlePRReadyScan` scans all of `board.Items` every poll (a cheap label-membership check), rather
  than a smaller subset — negligible cost, bounded by the small, transient number of issues
  mid-outstanding-mark-ready at any time.
- Unlike its terminal-only siblings, this marker can coexist with an item that is still actively being
  dispatched through later stages — a deliberate, documented divergence (see Rationale above), not an
  oversight.

## Sibling Audit

The merge-train's own inline `MarkPRReady` call (`engine/merge_train.go`, promoting a reused draft
integration PR to ready before landing) is out of scope: a failure there already leaves the affected
members in `Queued` and is retried naturally on the worker's own next iteration — it does not exhibit
the permanent-stranding failure mode this ADR addresses.

**References:** [ADR-1097: Non-Default-Base Explicit Close Retry](1097-non-default-base-close-retry.md), [ADR-061: Merge-Train Singleton Member-Issue Close Retry](061-merge-train-member-close-retry.md), [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md), [ADR-1616: Post-Done Landing Verification](1616-post-done-landing-verification.md)
