# ADR 1769: Live Re-Read of Autonomy Labels at Both Advance Decision Points

**Date**: 2026-09-17
**Status**: Accepted
**Issue**: #1769 — Autonomy-label removal mid-stage is not observed: advance decisions read a stale snapshot

## Context

An operator removing `fabrik:cruise` or `fabrik:yolo` from an in-flight item could not stop it
advancing. The removal was not observed until the next full board refresh, so the stage completed
and auto-advanced (or, at Validate, auto-merged) under authority the operator had already
withdrawn. Reported in #1768.

Two independent decision points read `hasYoloLabel(item)`/`hasCruiseLabel(item)` off `item.Labels`
at the moment they decide whether to advance:

- **Path 1 — `handleStageComplete`** (`engine/stages.go`), which runs synchronously right after a
  stage's `FABRIK_STAGE_COMPLETE`. `item` here is the pre-invocation dispatch-time snapshot, which
  can be stale by tens of minutes on a long-running stage. This function already *attempted* a
  mid-run label re-fetch, but targeted `e.cfg.Owner`/`e.cfg.Repo` instead of the item's own repo —
  empty in multi-repo mode (producing a 404, silently swallowed by an `err == nil &&` guard), and
  on a single-repo instance whose configured repo differs from the item's own, silently re-fetching
  the labels of whichever issue happens to hold that number in the configured repo instead — wrong
  data, not merely a failed call.

- **Path 2 — the catch-up loop's `runCatchUpPhase2`** (`engine/poll.go`), reached whenever
  `stage:<name>:complete` is already present — which is every `wait_for_ci: true` stage, since
  `handleStageComplete` defers to the catch-up loop there rather than reaching its own advance
  logic. On the default stage set this is **Validate**, where advancing under withdrawn authority
  means auto-merging the PR. This path performed no re-fetch at all: `item.Labels` is not part of
  the deep-fetch cache contract (`copyDeepFieldsFromState` excludes `Labels`), so an item that went
  through a "fresh" deep fetch earlier in the same poll pass could still carry a stale
  `fabrik:yolo`/`fabrik:cruise` from before an operator removed it.

Both defects meant a label *removal* made mid-run was never reliably observed by either path. This
is the same class of defect two prior fixes in this codebase addressed by moving from a cached
snapshot to a live re-read at the decision point: ADR-1419 (`checkDependencies` re-reads
`BlockedBy` live rather than trusting a stale-possibly-empty cache) and ADR-1216
(`reviewGateBlocksLanding` re-reads review state live rather than trusting the item snapshot). Both
precedents apply directly here: a decision with real consequences (advance, or at Validate, merge)
must not be made on a cached snapshot that a concurrent operator action can invalidate.

## Decision

Add one new shared helper, `refreshAutonomyLabels(item *gh.ProjectItem)` (`engine/stages.go`), that
performs a live, item-scoped `FetchLabels` call and mutates `item.Labels` in place:

1. Resolve `owner, repo` via `itemOwnerRepo(*item, e.defaultRepo())` — the item's own coordinates,
   never `e.cfg.Owner`/`e.cfg.Repo`.
2. Call `e.client.FetchLabels(owner, repo, item.Number)` — `e.client`, the raw, uncached
   `GitHubClient`, never `e.readClient`, since a label change made moments ago may not have an
   applied webhook delta yet, and `Labels` is not part of `e.readClient`'s deep-field cache
   contract in any case.
3. On error: log a warning naming the item and the error, and leave `item.Labels` untouched.
   Silent fallback is exactly what hid this defect for D1's original re-fetch attempt, and must not
   survive the fix.
4. On success: replace `item.Labels` unconditionally, gated on `err == nil` alone — dropping the
   previous `len(freshLabels) > 0` guard entirely. A successful fetch returning zero labels is real
   data (the operator removed the last autonomy label), not "no change," and must not be discarded.

Both `handleStageComplete` and `runCatchUpPhase2` call this helper as the very first step of their
autonomy-decision logic, before either reads `hasYoloLabel`/`hasCruiseLabel` — so the two paths can
never disagree about the same item's autonomy state. This follows the shape of
`effectiveReviewAuthority`/`effectiveExpectedReviewers` (ADR-1250, ADR-1283, `engine/reviews.go`):
"one shared helper, every gate that needs the same answer" — except, unlike those two, which only
*resolve* a label already present on the snapshot, `refreshAutonomyLabels` performs the fetch
itself. A future reader should not assume it is fetch-free by analogy to its siblings.

`attemptMergeOnValidate` — the single landing-decision function both paths call into (ADR-1216) —
is not modified beyond a comment. It receives `item` by value from both callers and performs its
own `hasCruiseLabel(item)` check; because both callers now call `refreshAutonomyLabels` upstream of
every `attemptMergeOnValidate` call site, that check is fresh too, at zero *additional* fetch cost
from this fix. Adding a second, independent autonomy-label fetch inside `attemptMergeOnValidate`
itself would have violated the cost bound below and was rejected.

Note: `attemptMergeOnValidate` already performs a live re-read of its own for an unrelated purpose
— its pre-existing ADR-1419 dependency guard (`FetchItemDetails`, called for `BlockedBy`) — and that
call's GraphQL query resets `item.Labels` as a side effect, overwriting the REST-fresh snapshot
`refreshAutonomyLabels` just produced. This is harmless: the guard runs after both the
`hasCruiseLabel` check above and the `fabrik:auto-merge-enabled` idempotency check, and nothing in
the function reads `hasYoloLabel`/`hasCruiseLabel` afterward. But on the Validate/yolo merge path,
`item.Labels` is genuinely fetched twice per invocation — once via `refreshAutonomyLabels` (REST),
once via the dependency guard (GraphQL) — not once. A future label check added after the dependency
guard must not assume it is reading the REST-fresh snapshot.

### Cost bound

The helper always fetches, once, unconditionally — there is no "already fresh" fast path, since
`Labels` is never part of the deep-fetch cache contract for either caller regardless of how
recently a deep fetch ran. This is deliberately simple: reasoning about cache staleness here would
buy nothing, since the answer is always "not fresh." The bound this satisfies is at most one live
REST call per item per poll: `handleStageComplete` and `runCatchUpPhase2` are mutually exclusive
admission for the same item in the same pass, so at most one of them runs per item per poll, and
every downstream `hasYoloLabel`/`hasCruiseLabel` read (including the one inside
`attemptMergeOnValidate`) is covered by that one call. This is *not* "never per-poll" — an item
that keeps reaching `runCatchUpPhase2` unclaimed across many poll cycles (e.g. a cruise item parked
at Validate awaiting human merge) incurs a fresh `FetchLabels` call on every one of those passes.
The bound is on concurrency within a pass, not on the fetch recurring over an item's lifetime.

## Consequences

**Positive:**
- An operator's autonomy-label removal made while a stage is in flight is now observed at the next
  advance decision, on both paths, not at the next full board refresh.
- The original re-fetch's own intent — an autonomy-label *addition* made mid-run — is now correctly
  honoured on multi-repo instances, where it was previously silently broken.
- Both advance paths are structurally guaranteed to agree, since they share one helper rather than
  two independent (and, before this fix, differently-broken) re-fetch implementations.
- The added cost is bounded, not unbounded: at most one live `FetchLabels` call per item per poll,
  since `handleStageComplete` and `runCatchUpPhase2` are mutually exclusive admission for the same
  item in the same pass (see "Cost bound" above) — the fix does not multiply per-gate.

**Negative / Trade-offs:**
- `runCatchUpPhase2` now performs one additional live REST call per invocation that it did not
  perform before (D2 had no re-fetch at all). This is the intended fix, not a regression, but it is
  a real, non-zero cost increase on that path.
- A test-infrastructure consequence: `mockGitHubClient`'s zero-value `FetchLabels` response changed
  from `(nil, nil)` to a sentinel "not configured" error, since removing the `len > 0` guard means a
  bare `(nil, nil)` "confirmed zero labels" default would now silently wipe every test's
  directly-constructed `item.Labels`. Every existing test that doesn't wire up `fetchLabelsFn`
  relies on the new error-and-fallback behavior to leave its `item.Labels` untouched — a test-only
  change with no production-code effect, but one that touched a shared mock used across the
  package.

## Sibling Audit

The merge-train's own paths into `attemptMergeOnValidate` (both the batch-landing and singleton
routes) are covered automatically, since they route through the same two callers
(`handleStageComplete`, `runCatchUpPhase2`) rather than reading autonomy labels independently. No
other call site in the engine reads `hasYoloLabel`/`hasCruiseLabel` as part of an advance or merge
decision outside these two functions.

**References:** [ADR-1419: Cross-Repo Spawn Servability and Mid-flight Recognition](1419-cross-repo-spawn-servability-and-midflight-recognition.md), [ADR-1216: Review Gate at Landing Decision](1216-review-gate-at-landing-decision.md), [ADR-1250: Review Authority Orthogonal to Autonomy](1250-review-authority-orthogonal-to-autonomy.md), [ADR-1283: Declared Unrequested Reviewers](1283-declared-unrequested-reviewers.md), issue #1768 (originating report), issue #1769 (this fix).
