# ADR 1647: Exclude Non-Default-Base Merge-Train Members at Pre-Dispatch Selection

**Date**: 2026-08-24
**Status**: Superseded by [ADR-1648](1648-merge-train-per-base-partitioning.md)
**Issue**: #1647 — fix(merge-train): exclude non-default-base members from batching instead of
silently mis-basing them (from community report #1646)

> **Superseded**: this ADR's exclusion (`nonDefaultBaseExclusion`/`filterNonDefaultBaseMembers`/
> `markNonDefaultBaseExcluded`, `fabrik:non-default-base-excluded`) made the train *safe* on
> `base:<branch>` members without making it *capable* — such members were skipped, not batched,
> and had to be merged by hand. #1648 removes this exclusion entirely and replaces it with
> per-(repo, base) partitioning: every distinct resolved base gets its own independent train
> instead of the single default partition being kept and everything else excluded. This document
> is retained for its historical rationale (why the exclusion lived where it did, and the
> `nonDefaultBaseExclusion` placement analysis that ADR-1648 builds on directly) but no longer
> describes current behavior — see ADR-1648.

## Context

The internal merge train (ADR-059) resolves its base branch exactly once per dispatch, from the
repository default (`wm.DefaultBaseBranch()`), and uses that single value for every subsequent
train operation: trial branch naming, base SHA pinning, the Claude invocation's `BaseBranch`, the
draft CI PR, the final integration PR, and the landing decision. It never consults
`baseBranchForItem` (`engine/item.go`), the resolver every other per-item base-branch call site in
this engine already uses (`ci.go`, `close_nondefault_base.go`, `comments.go`, `context.go`,
`landing_verification_settle.go`, `item.go`, `merge_gate.go`).

A Queued member carrying `base:maint/0.13.4` therefore got silently folded into a trial branched
from `origin/main`: merged into the wrong base, diffed against the wrong base, and — had CI gone
green — landed via an integration PR targeting `main`. Nothing errors; the work simply lands in the
wrong place. Reported in #1646 from real use of `base:<branch>` on a maintenance release; no
mis-land occurred only because the reporter caught it and merged both PRs by hand instead of
letting the train run.

This issue deliberately does not teach the train to batch multiple bases at once (a follow-up,
larger design effort). It only needs to stop touching what it cannot correctly base — convert
today's silent mis-basing into an explicit, visible "not batched," leaving the member safely in
`Queued` for a manual merge.

Two placement candidates were considered, both discussed in this issue's Research/Plan stages:

1. **Inside `prepareTrainWorker`** (the merge-train worker goroutine) — the community reporter's
   own suggested fix point in #1646. `wm` and `baseBranch` are already resolved there, so no new
   `WorktreeManager` lookup risk. But it runs *after* `routeQueuedGroup`'s batch cap
   (`capBatch`) and *after* `dispatchMergeTrainWorker` has already snapshotted
   `mergeTrainWorkerState.batchNumbers` from the pre-filter batch.
2. **Inside `routeQueuedGroup`** (the poll goroutine), immediately after `trainItems` is built and
   before both the batch cap and dispatch. Requires resolving each item's base *without* the
   guaranteed-registered `WorktreeManager` `prepareTrainWorker` enjoys, since the poll goroutine
   must never call `ensureRepoReady` (it always shells out to git, including a real `git fetch
   origin` even on its fast "already cloned" path).

## Decision

Filter non-default-base members out of `trainItems` **inside `routeQueuedGroup`**, before the
batch cap and before dispatch, entirely in the poll goroutine:

- `nonDefaultBaseExclusion(item, repoKey)` resolves `(exclude, base, def, resolved)` for one item.
  It fast-paths `(false, "", "", true)` when the item carries no `base:` label at all
  (`itemHasBaseLabel`) — no `WorktreeManager` lookup, no git call. When a `base:` label is
  present, it peeks `e.worktreeManagers[repoKey]` under `e.mu` (the same non-panicking pattern
  `closeIssueIfNonDefaultBase` already established, never `e.worktreesFor`/`ensureRepoReady`). If
  the `WorktreeManager` isn't registered yet, it returns `(true, "", "", false)` — **excluded, not
  yet resolved** — rather than including the item. Otherwise it resolves via the same
  `baseBranchForItem` every other per-item call site uses, and compares against
  `wm.DefaultBaseBranch()`.
- `markNonDefaultBaseExcluded` posts a one-time explanatory comment and applies
  `fabrik:non-default-base-excluded`, gated on the label's own prior absence (R3) — see Label
  Design below.
- `filterNonDefaultBaseMembers` applies both across a batch: excluded+resolved items are logged
  (R2, both branch names) and marked; excluded+unresolved items are logged distinctly and skipped
  without posting the comment (the true base is still unknown, so nothing to report yet); included
  items self-clear the label if a prior exclusion no longer applies.
- Wired into `routeQueuedGroup` immediately after the precedence loop builds `trainItems`, before
  the `len(trainItems) == 0` early return and before `capBatch`.

`engine/merge_train.go` (`prepareTrainWorker`, `baseBranchForItem`, the single-base-per-batch
design) is untouched — this is a pure narrowing at the selection boundary, not a change to how the
train itself operates once dispatched.

## Rationale

### Why pre-dispatch, not inside `prepareTrainWorker`?

Two concurrency interactions the `prepareTrainWorker` placement would have to solve, that the
`routeQueuedGroup` placement avoids by construction:

1. **`batchNumbers`'s "safe upper bound" contract (ADR-1208).**
   `mergeTrainWorkerState.batchNumbers` is snapshotted in `dispatchMergeTrainWorker`, *before*
   `prepareTrainWorker` ever runs, from whatever batch `routeQueuedGroup` decided to dispatch, and
   is documented as never mutated afterward. `settleQueuedReviewFindings` (ADR-1208) reads it to
   decide whether a Queued member with an unresolved PR review-thread finding should be ejected
   directly (not in any live worker's batch) or via a pending-eject signal consumed later by the
   worker's own checkpoints (`applyPendingReviewEjects`, which only ever iterates the worker's live
   `survivors`/`current`). A `prepareTrainWorker`-only exclusion would leave an excluded item's
   number in `batchNumbers` even though the worker will never look at it again — if that item also
   picks up a review finding, `settleQueuedReviewFindings` would record a pending signal that is
   never consumed (the item is excluded on every future dispatch too, deterministically),
   reproducing the exact "overflow member" blackout class ADR-1208 already fixed once, for this new
   exclusion category instead. A backstop fix would require shrinking `batchNumbers` after
   `dispatchMergeTrainWorker` already snapshotted it, violating its documented invariant and
   creating an unsynchronized concurrent map access the settle-scan reader (`mergeTrainBatchMembers`,
   which takes no lock on the immutable map) would not tolerate under `-race`.
2. **Batch-cap starvation.** `routeQueuedGroup` applies `capBatch(trainItems,
   effectiveMaxBatchSize())` before dispatch. A non-default-base item filtered only inside
   `prepareTrainWorker` (post-cap) would occupy one of the (default 5) batch-cap slots every poll
   indefinitely, since it's excluded every single dispatch cycle deterministically — silently
   starving a default-base sibling that would otherwise have fit under the cap. A real, if narrow,
   tension with R5's "pure narrowing for every existing train user" framing.

Filtering in `routeQueuedGroup`, before both the cap and the snapshot, means an excluded item never
becomes a `trainItem`, never occupies a batch-cap slot, and never appears in any worker's
`batchNumbers` — both interactions are avoided by construction, not patched around afterward.

### Why fail-closed, not fail-open, when the `WorktreeManager` isn't registered?

The alternative (include the item, since `prepareTrainWorker` will register the `WorktreeManager`
moments later anyway once dispatched) reopens exactly the risk AC1 forbids: a non-default-base
member must **never** be included in a trial batch, not merely usually excluded. Fail-closed costs
at most one extra poll cycle of non-batching for a same-poll edge case that, in practice, essentially
never occurs — a repo only has Queued items after earlier stages already registered its
`WorktreeManager` during normal pipeline processing. R4 already establishes that "excluded, still
`Queued`" is a safe, expected outcome, so being occasionally over-conservative here is free. No
explanatory comment is posted for the unresolved case, since the true base is still unknown — only
a confirmed non-default resolution triggers R3's comment.

### Label design: `fabrik:non-default-base-excluded`, not a comment-content scan

R3 requires the exclusion be posted exactly once across repeated poll cycles, using the
"`fabrik:awaiting-ci` non-spamming convention." The established idiom for that convention elsewhere
in this engine (`fabrik:api-key-helper-detected`, `fabrik:tools-denied`, `fabrik:claude-limit`) is:
post once, gated on a label's own prior absence, self-clear once the condition no longer holds. That
idiom is the *only* option here, not merely the preferred one: `FetchProjectBoard`'s shallow query
(what `board.Items`/`groupQueuedByRepo`/`routeQueuedGroup`'s `trainItems` are all sourced from)
explicitly excludes comments, and Queued items are excluded from `deepFetchCandidates`, so
`item.Comments` is never reliably populated for a Queued item — the `hasSkippedComment`-style
"scan prior comments" idempotency idiom used elsewhere in this engine is not available here. Labels,
by contrast, are fetched even on the shallow query (`labels(first: 30)`), which is why
`groupQueuedByRepo` can already safely check `fabrik:paused` directly off `board.Items` — the same
reliability `fabrik:non-default-base-excluded` depends on.

The label self-clears (rather than requiring manual removal) because `routeQueuedGroup` re-evaluates
every Queued item every poll — no settle scan is needed; the very next poll after a `base:` label
is removed or its target branch starts existing naturally clears the exclusion.

**Comment-failure retry (found in review, PR #1652, handarbeit-pruefer):** the label being the sole
idempotency gate means an unconditional `addLabel` after a possibly-failed `AddComment` would
silently and permanently lose R3's explanation for that member — `hasLabel` would short-circuit
every later poll with no retry path, unlike `fireRunawayGuard`'s `fabrik:awaiting-runaway-alert`
(ADR-1533) or `closeIssueIfNonDefaultBase`'s `fabrik:awaiting-close` (ADR-1097), both of which pair
their marker label with a dedicated settle scan for exactly this failure mode. This label doesn't
need an equivalent settle scan, though: unlike those two (which guard one-shot terminal transitions
never otherwise re-evaluated), `routeQueuedGroup` already re-evaluates every Queued item's exclusion
status every poll. So `markNonDefaultBaseExcluded` simply checks the comment post's own error and
applies the label only on confirmed success — a failed attempt leaves the label off, and the next
poll's `filterNonDefaultBaseMembers` call retries the comment for free.

**Label-add failure retry (found in review, PR #1652, handarbeit-pruefer):** the comment-failure
fix above still left a symmetric gap on the other side of the same `if` — once `postComment`
succeeds, `markNonDefaultBaseExcluded` applied the label via `addLabel`, which swallows
`AddLabelToIssue`'s error (log-and-continue, the correct default for most call sites, but wrong
here). If the label write itself failed after a successful comment post, `hasLabel`'s gate would
never close, and the same explanatory comment would be reposted every subsequent poll for as long
as the label add kept failing — a narrower failure mode than the one just fixed (duplicate
comment noise, not silent permanent loss of the explanation), but the same class of gap. Fixed by
switching to `addLabelChecked` (`engine/mutate.go`), the engine's existing error-surfacing variant
of `addLabel` built for exactly this "next action depends on whether the label actually landed"
shape (its only other caller, `markCreditedLanding`, exists for the identical reason — see
`landing_verification_settle.go`). On a checked failure, `markNonDefaultBaseExcluded` logs a
warning and returns without further action; `hasLabel`'s gate stays open, so the next poll's
`filterNonDefaultBaseMembers` call reaches this function again and reposts the comment for free —
identical retry shape to the comment-failure path.

### Runaway-guard interaction (Hook 2)

`routeQueuedGroup` has a third pre-dispatch interaction the initial implementation missed:
Hook 2 of the runaway guard (ADR-059 D8) — the poll-side check that pauses the current
Queued snapshot outright when the trial counter is already tripped, to catch beyond-cap
members Hook 1 (the worker goroutine) can't reach — ran on the *uncapped* `items` slice,
unfiltered. A member `filterNonDefaultBaseMembers` had just excluded this same poll, whose
own comment promises it "remains safely in `Queued`, not paused, not failed, not moved,"
could still be swept into that pause+alert if the guard tripped in the same call —
directly contradicting R4 and the comment text itself. Found in review (PR #1652,
handarbeit-pruefer).

Fixed by `excludeNonDefaultBaseExclusions`: it diffs the pre-filter `trainCandidates`
against the post-filter, pre-cap `filteredTrainItems` to identify exactly which member
numbers `filterNonDefaultBaseMembers` excluded this poll, and drops only those from the
slice passed to `fireRunawayGuard`. Batch-cap overflow and queue-enabled members are
unaffected — both are still paused as before, matching the existing "uncapped items slice
so all Queued members are paused, not just the batch cap" intent for every category the
runaway guard *does* mean to catch. A base-excluded member never contributes a trial to
the count the guard is measuring, so excluding it from the guard's blast radius is
correct, not merely convenient.

### Multiple `base:` labels

`baseBranchForItem` already resolves multiple `base:` labels on one item (first wins, warns on the
rest) — no new handling was added here; `nonDefaultBaseExclusion` simply delegates.

## Consequences

**Positive:**
- AC1 is now structural: a non-default-base member cannot reach the trial-assembly path at all,
  proved by `TestRouteQueuedGroup_MixedBatch_NonDefaultBaseMemberNeverInTrial` (which was
  confirmed to fail — reproducing the #1646 bug exactly — with the guard temporarily removed).
- Zero-cost for the common case (R5): `itemHasBaseLabel`'s fast path means a train with no
  `base:`-labelled members — every existing train user — takes no new `WorktreeManager` lookup, no
  new git call, and no new log/label/comment traffic.
- The `batchNumbers`/`settleQueuedReviewFindings` interaction and the batch-cap starvation risk
  are avoided by construction rather than by a second patch layered on top.
- `engine/merge_train.go` is untouched, keeping this change a pure narrowing at the selection
  boundary — consistent with the issue's explicit scope boundary.

**Negative / Trade-offs:**
- A same-poll cold-start window (a repo's `WorktreeManager` not yet registered when its first
  Queued batch is evaluated) defers a genuinely-default-base item by one extra poll cycle,
  fail-closed. Accepted: narrow, self-resolving, and R4 already treats "stays in `Queued`" as a
  safe outcome.
- `fabrik:non-default-base-excluded` is a new durable label operators must recognize; documented
  in `docs/state-machine.md` and `CLAUDE.md` alongside its siblings.

## Sibling Audit

Structurally distinct from `fabrik:awaiting-close`/ADR-1097 and `fabrik:awaiting-member-close`/
ADR-061: those retry a single at-risk mutation after every other side effect has already landed.
This label instead gates a `routeQueuedGroup`-re-evaluated-every-poll *decision* — closer in shape
to `fabrik:api-key-helper-detected`/`fabrik:tools-denied` (ADR-1346/ADR-1523), which is why it
follows their "self-clearing, no settle scan" idiom rather than the retry-counter idiom. Added to
`transientLifecycleLabels` so it is swept from closed issues like those siblings.

**References:** [ADR-059: Internal Merge Train](059-internal-merge-train.md), [ADR-1096: Explicit
Issue Close on Non-Default-Base Merge](1096-explicit-close-on-nondefault-base-merge.md), [ADR-1097:
Non-Default-Base Explicit Close Retry](1097-non-default-base-close-retry.md), [ADR-1208: Queued
Review-Finding Ejection](1208-queued-review-finding-ejection.md), [ADR-1346: Scrub Anthropic Auth
Env Namespace](1346-scrub-anthropic-auth-env-namespace.md), [ADR-1523: Exempt Tool Permission
Denials From max_retries](1523-exempt-tool-permission-denials-from-max-retries.md)
