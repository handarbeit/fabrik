# ADR 1097: Non-Default-Base Explicit Close Retry

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1097 — Durability: settle scan + escalation for the non-default-base explicit issue close

## Context

ADR-1096 added `closeIssueIfNonDefaultBase`, called from both terminal merge-advance sites
(`runValidatePRTerminalAdvance`'s cruise path, `advanceConvergedPRToDone`'s non-train yolo path)
immediately after their board advance to Done, to explicitly close an issue whose linked PR merged
onto a non-default `base:<branch>` — a case where GitHub's own `Closes #N` auto-close is inert. That
fix is deliberately best-effort: if the explicit `CloseIssue` call itself fails (rate-limit
exhaustion, network blip, a restart between the Done-advance and the close), the issue is left open
with its PR merged and its board item at Done, reproducing the exact community symptom (#1095) that
motivated ADR-1096 in the first place — downstream `fabrik:blocked` items never unblock, because
native dependency edges clear on issue *close*, not PR merge. ADR-1096's own doc comment and Sibling
Audit named this as "Issue B," a chained `blockedBy` follow-up modeled on the
`fabrik:awaiting-member-close` settle pattern (ADR-061).

This is structurally the same shape ADR-061 already solved: a single at-risk `CloseIssue` call,
positioned *after* every other side effect (the Done-move) has already landed — not a multi-call
chain like ADR-060's `fabrik:awaiting-done`. ADR-061's own Rationale distinguishes exactly this case
from ADR-060's: no per-stage redispatch risk to guard against, since the item has already reached its
terminal outcome by the time the marker could be written.

## Decision

Add a new durable label, `fabrik:awaiting-close`, written **only in the failure branch** of
`closeIssueIfNonDefaultBase`'s explicit `CloseIssue` call (not unconditionally beforehand) — mirroring
ADR-061's `fabrik:awaiting-member-close` call-for-call, not ADR-060's chain-protecting marker.

The retry mechanism (`engine/close_nondefault_base_settle.go`) is a self-contained scan, **not** wired
into `itemMayNeedWork`/`itemNeedsWork` or `deepFetchCandidates`, and **not** added to
`transientLifecycleLabels`:

- `settleNonDefaultBaseCloses` (called from `poll()` in `engine/poll.go`, immediately alongside
  `settleMergeTrainMemberCloses`) runs unconditionally every poll. It iterates the **raw
  `board.Items`** (not `deepFetchCandidates`), checking only `hasLabel(item, "fabrik:awaiting-close")`
  and skipping items carrying `fabrik:paused` (mirroring `settleMergeTrainMemberCloses`'s own
  paused-item guard).
- `settleNonDefaultBaseClose` does the retry itself: if `item.IsClosed`, skip the redundant
  `CloseIssue` call and clear the marker. Otherwise call `CloseIssue`; on success, clear the marker
  (and write through the boardcache via `ApplyIssueClosed`, matching `closeIssueIfNonDefaultBase`'s
  own success path); on failure, record a retry.
- Retries reuse the existing generic `recordSettleRetry`/`escalateSettle`/`clearSettleMarker` helpers
  (`engine/settle.go`), keyed by a dedicated constant, `"__non_default_base_close__"` (same
  double-underscore-wrapped, YAML-unrepresentable shape as its siblings). Once `MaxRetries` is
  reached, `escalateNonDefaultBaseCloseFailure` fires: `fabrik:paused` is added,
  `fabrik:awaiting-close` is removed, an explanatory comment naming the merged PR
  (`item.LinkedPRNumber`, already populated on the board item — no new storage needed) with the
  manual `gh issue close` recovery step is posted, and `itemstate.EnginePaused` is applied.

No changes to `itemMayNeedWork`, `itemNeedsWork`, or `transientLifecycleLabels` are needed or made.

## Rationale

### Why a durable GitHub label, not an `itemstate.Store` mutation?

Same reasoning as ADR-060/ADR-061: `itemstate.Store` does not survive a restart, and there is no
artifact to safely "redo" here — the explicit close is a one-shot action with nothing
idempotent-by-replay except the call itself. An in-memory-only marker would silently lose the
outstanding-close decision across a restart.

### Why write the marker only on failure, not unconditionally beforehand?

Identical to ADR-061's reasoning: there is exactly one at-risk call (`CloseIssue`), not a chain like
ADR-060's ~10-call sequence. Writing the marker unconditionally before it would mean
adding-then-immediately-removing a label on every successful close (the overwhelmingly common case),
for no correctness benefit. The only unclosed window is an engine crash between the `CloseIssue`
request being sent and its response being processed — the same narrower, accepted risk every other
single-call escalation site in this engine already carries.

### Why `item.LinkedPRNumber` instead of threading `prNumber` through the marker?

`closeIssueIfNonDefaultBase(item, prNumber)` already has the merged PR number in scope at
mark-time, but a GitHub label cannot carry a payload, and stashing it in `itemstate.Store` would
reintroduce the exact restart-fragility this label exists to avoid. `LinkedPRNumber` is already
populated on `gh.ProjectItem` from the board query, so it is available identically whether the
marker was just written or is being retried N polls later — the escalation comment reads it directly
from the item passed to `settleNonDefaultBaseClose`/`escalateNonDefaultBaseCloseFailure`, with no new
storage.

### Why a `board.Items` scan instead of a `deepFetchCandidates`-based settle-scan shape?

Same distinction ADR-061 already drew from ADR-060: this marker has no dispatch-suppression job. By
construction, `closeIssueIfNonDefaultBase` only becomes reachable after both terminal-advance callers
have already moved the item's board status to Done. There is no redispatch risk to guard against, so
hooking into the dispatch-suppression machinery would add complexity (and revive the
terminal-skip/`transientLifecycleLabels` interaction ADR-061's Context describes) without buying
anything.

### Why a dedicated retry-counter constant instead of reusing `mergeTrainMemberCloseRetryStage`?

Reusing it would conflate two unrelated failure classes ("a merge-train singleton's member-issue
close stalled" vs. "a non-default-base explicit close stalled") under one counter, muddying
`MaxRetries` bookkeeping. `"__non_default_base_close__"` is, like its siblings, deliberately
unrepresentable as a real YAML stage `name:`, so it can never collide with a configured stage's own
counter — no new `itemstate.Mutation` type is introduced.

## Consequences

**Positive:**
- An issue whose PR merged onto a non-default base, but whose explicit close then failed, is no
  longer left landed-but-open forever with no self-healing path — closing ADR-1096's own deferred gap
  and the community-reported #1095 symptom for this failure mode.
- The fix does not touch `itemMayNeedWork`, `itemNeedsWork`, `transientLifecycleLabels`, or any
  dispatch-suppression code — it cannot regress the terminal-skip optimization (#689) or interact with
  any other gate label, by construction.
- Reuses the same generic settle/escalate helpers as `fabrik:awaiting-done` and
  `fabrik:awaiting-member-close`, so this is the fourth structurally identical instance of the
  pattern, not a new mechanism.

**Negative / Trade-offs:**
- The marker is written only in the failure branch, not unconditionally first — an engine crash in
  the narrow window between the `CloseIssue` request being sent and its response being processed would
  leave neither a marker nor a completed close, silently reproducing the original bug for that one
  crash. Accepted, consistent with every other single-call escalation site in this engine (including
  ADR-061's identical acceptance).
- `settleNonDefaultBaseCloses` scans all of `board.Items` every poll (a cheap label-membership
  check), rather than a smaller subset — negligible cost, bounded by the small, transient number of
  issues mid-outstanding-close at any time.

## Sibling Audit

The merge-train member-issue close (`landSingleton`) already has its own durable settle path
(`fabrik:awaiting-member-close`, ADR-061); this ADR does not touch it. `landMergeTrainBatch`'s
analogous, still-unretried member-issue close remains a separate, deliberately out-of-scope follow-up
(noted in both ADR-061's own Sibling Audit and CLAUDE.md) — untouched by this issue's scope.

**References:** [ADR-1096: Explicit Issue Close on Non-Default-Base Merge](1096-explicit-close-on-nondefault-base-merge.md), [ADR-061: Merge-Train Singleton Member-Issue Close Retry](061-merge-train-member-close-retry.md), [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md)
