# ADR 061: Merge-Train Singleton Member-Issue Close Retry

**Date**: 2026-07-16
**Status**: Accepted
**Issue**: #985 — fix(engine): merge-train singleton landing's member-issue close has no retry

## Context

ADR-060's Sibling Audit named `engine/merge_train.go`'s `landSingleton` (the merge-train one-at-a-time landing fallback): after a member's board status has already durably advanced to Done, the final `CloseIssue` call for the member issue itself is logged-and-swallowed on failure, with nothing to retry it. Done is a cleanup stage whose handler only manages the worktree, not issue-closed state, so a persistent failure (most plausibly on a non-default base branch, where GitHub's own `Closes #N` on the landing PR never auto-fires) leaves the issue permanently open, sitting in the Done column, with no self-healing path.

This issue asks for "the same durable-marker-plus-settle-scan treatment as ADR-060." Research found that a literal port of ADR-060's shape — gate `itemMayNeedWork`/`itemNeedsWork` on the new marker, settle scan over `deepFetchCandidates` — collides with a mechanism ADR-060 did not have to contend with: the terminal-skip optimization (#689). `isTerminalPredicate` permanently skips an item from all further polling (no deep-fetch, not even added to `deepFetchCandidates`) once it is at a `CleanupWorktree` stage, carries `stage:<cleanup>:complete`, and carries none of `transientLifecycleLabels`. Because the Done-stage cleanup dispatch runs independently of any awaiting-marker, `stage:Done:complete` lands on essentially the same poll regardless of whether the member-issue close has succeeded — so a marker not added to `transientLifecycleLabels` would very likely stop being retried the moment cleanup dispatch races it, reproducing (one layer deeper) the exact class of bug this issue exists to fix.

Unlike ADR-060's marker, this one is written *after* the Done-move, not before it: by the time `landSingleton`'s member-issue close can fail, the PR merge, the Done-move, and the member-PR close have already run. There is exactly one at-risk call, not a chain of ten. This materially different starting condition removes the reason to hook into the `deepFetchCandidates`/dispatch-suppression machinery at all — the item has already reached its terminal outcome, and nothing about the settle scan needs to touch normal per-stage dispatch to do its job.

## Decision

Add a new durable label, `fabrik:awaiting-member-close`, written **only in the failure branch** of `landSingleton`'s member-issue `CloseIssue` call (not unconditionally beforehand, unlike ADR-060's marker) — this is a single one-shot call, not a multi-call sequence with an internal failure window to close.

The retry mechanism is a self-contained scan, **not** wired into `itemMayNeedWork`/`itemNeedsWork` or `deepFetchCandidates`, and **not** added to `transientLifecycleLabels`:

- `settleMergeTrainMemberCloses` (`engine/poll.go`) runs unconditionally every poll, immediately after `handleMergeTrainBatch` — independent of `merge_train: on/off`, so a marker written while the setting was enabled keeps draining even if it is later turned off. It iterates the **raw `board.Items`** (not `deepFetchCandidates`), checking only `hasLabel(item, "fabrik:awaiting-member-close")` and skipping items carrying `fabrik:paused` (mirroring the no-work-needed settle scan's own paused-item guard: an operator investigating a paused item must not be fought).
- `settleMergeTrainMemberClose` (`engine/merge_train_member_close_settle.go`) does the retry itself: if `item.IsClosed`, skip the redundant `CloseIssue` call and clear the marker (the idempotency check the issue's Requirements ask for). Otherwise call `CloseIssue`; on success, clear the marker; on failure, record a retry.
- Retries are counted against the existing `itemstate.StageRetryIncremented`/`Attempts`/`e.cfg.MaxRetries` mechanism, keyed by a dedicated constant, `"__merge_train_member_close__"` (mirroring `noWorkNeededRetryStage`'s double-underscore-wrapped, YAML-unrepresentable shape). Once `MaxRetries` is reached, `escalateMergeTrainMemberCloseFailure` fires: `fabrik:paused` is added, `fabrik:awaiting-member-close` is removed, an explanatory comment with the manual `gh issue close` recovery step is posted, and `itemstate.EnginePaused` is applied — mirroring `escalateNoWorkNeededFailure` exactly.

No changes to `itemMayNeedWork`, `itemNeedsWork`, or `transientLifecycleLabels` are needed or made.

## Rationale

### Why a durable GitHub label, not an `itemstate.Store` mutation?

Same reasoning as ADR-060: `itemstate.Store` does not survive a restart, and there is no artifact to safely "redo" here — the member-issue close is a one-shot action with nothing idempotent-by-replay except the call itself. If the marker were in-memory only and the engine restarted while the close was still outstanding, the issue would silently stay open forever with no record that anything was ever wrong.

### Why write the marker only on failure, not unconditionally beforehand (unlike ADR-060)?

ADR-060's marker had to be written first because `handleNoWorkNeeded` made upward of ten sequential GraphQL calls, any of which could be the one that first hits an exhausted rate limit — writing the marker only immediately before the risky call would still lose the decision if an earlier call failed. That reasoning does not transfer here: there is exactly one at-risk call (`CloseIssue`), not a chain. Writing the marker unconditionally before it would mean adding-then-immediately-removing a label on every successful close (the overwhelmingly common case), for no correctness benefit — the only unclosed window is an engine crash between the `CloseIssue` request being sent and the response being processed, which is a narrower, accepted risk (the same crash-window property every other single-call escalation site in this engine already has).

### Why a `board.Items` scan instead of reusing the `deepFetchCandidates`-based settle-scan shape?

Both are valid instances of the "durable label + per-poll settle + escalate" pattern family; the difference is *what the label needs to interact with*. ADR-060's marker must suppress normal per-stage dispatch, because the item's board column cannot be trusted while the Done-move itself may be outstanding — that is exactly what `itemMayNeedWork`/`itemNeedsWork`'s gate and the `deepFetchCandidates`-based scan are for. This marker has no such job: by construction, `landSingleton`'s member-issue close only becomes reachable after the PR merge and Done-move have already succeeded (or, in the rarer case where the Done-move itself failed, the item is sitting in the `HoldingStage` column, which is never individually dispatched by `itemMayNeedWork`/`itemNeedsWork` regardless — "batch-scoped, not per-item"). There is no redispatch risk this marker needs to guard against, so hooking it into the dispatch-suppression machinery would add complexity (and revive the terminal-skip/`transientLifecycleLabels` interaction described in Context) without buying anything. The merge-train subsystem already has its own unconditional per-poll entry point over raw `board.Items` (`handleMergeTrainBatch`) for exactly this kind of subsystem-scoped, dispatch-independent scan — this fix reuses that shape rather than the no-work-needed one.

### Why a dedicated retry-counter constant instead of reusing `noWorkNeededRetryStage`?

Reusing it would conflate two unrelated failure classes ("the no-work-needed Done-move/close stalled" vs. "a merge-train singleton's member-issue close stalled") under one counter, muddying `MaxRetries` bookkeeping and any future debugging. `"__merge_train_member_close__"` is, like its sibling, deliberately unrepresentable as a real YAML stage `name:`, so it can never collide with a configured stage's own counter — no new `itemstate.Mutation` type is introduced.

### Why is `landMergeTrainBatch`'s identical member-issue close left unfixed?

`landMergeTrainBatch` (the full-batch, non-singleton landing path — the more commonly hit one, since batch landing is the default and one-at-a-time is the bisection fallback) has the exact same unretried `CloseIssue` call at its own member-issue-close site, discovered during this issue's Specify pass. It is deliberately out of scope here, per the issue's explicit scope guidance and this repo's established convention (ADR-060's own Sibling Audit deferred structurally similar findings rather than folding them in). The settle/escalate helpers in `engine/merge_train_member_close_settle.go` are written generic over `gh.ProjectItem`/owner/repo, so a follow-up issue can drive `landMergeTrainBatch`'s failure into the same `mergeTrainAwaitingMemberCloseLabel`/`settleMergeTrainMemberCloses` machinery by adding one call at its own close site, without duplicating the settle/escalate logic.

## Consequences

**Positive:**
- A merge-train singleton member whose issue-close fails (non-default base, or transient API trouble) is no longer left landed-but-open forever with no self-healing path.
- The fix does not touch `itemMayNeedWork`, `itemNeedsWork`, `transientLifecycleLabels`, or any dispatch-suppression code — it cannot regress the terminal-skip optimization (#689) or interact with any other gate label, by construction.
- The settle/escalate helpers are structured so the `landMergeTrainBatch` follow-up (tracked separately) can reuse them directly.

**Negative / Trade-offs:**
- The marker is written only in the failure branch, not unconditionally first — an engine crash in the narrow window between the `CloseIssue` request being sent and its response being processed would leave neither a marker nor a completed close, silently reproducing the original bug for that one crash. This is accepted as a narrower, lower-probability window than ADR-060's original (a single call vs. a ~10-call sequence), consistent with every other single-call escalation site in this engine (e.g. `escalatePRCreationFailure`'s own `CreatePR` call has the same property).
- `settleMergeTrainMemberCloses` scans all of `board.Items` every poll (a cheap label-membership check), rather than the smaller `deepFetchCandidates` subset — negligible cost, bounded by the small, transient number of issues mid-singleton-landing at any time.

## Sibling Audit

`landMergeTrainBatch`'s analogous member-issue close (`engine/merge_train.go`, its own FR-3 member lifecycle loop) has the identical no-retry shape and is explicitly deferred to a follow-up issue — see Rationale above and the parent issue's Scope section.

**References:** [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md), [ADR-059: Internal Merge Train](059-internal-merge-train.md)
