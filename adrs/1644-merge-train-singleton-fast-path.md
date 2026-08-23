# ADR 1644: Merge-Train Singleton Fast Path

**Date**: 2026-08-23
**Status**: Accepted
**Issue**: #1644 — skip the trial for a single up-to-date member — land its PR directly

## Context

A merge-train batch containing a single Queued member pays the full trial cycle — fork a trial
worktree off the pinned base, merge the member, push the branch, open a draft CI PR, wait out a
complete CI run, land, then clean up — even when that trial is guaranteed to produce a tree
byte-identical to one CI has already validated on the member's own PR. `landSingleton` exists, but
only as a fallback reached via `landOneAtATime` after a multi-member batch goes red and bisection
isolates survivors down to singletons — a lone Queued member arriving fresh at Queued takes the
normal `assembleAndValidate` trial path regardless.

The trial branch is `pinned_base + member`. If the pinned base SHA is already an ancestor of the
member's head, that merge is a fast-forward — the trial's tree would be identical to the member's
own PR head tree, which its own CI already ran against. Fabrik's own Validate-stage convention
(rebase onto base before signalling completion) makes this the common case by construction: an item
arriving fresh from Validate is typically already up to date with base. A live measurement on a
sibling repo (verveguy/liminis-context-graph, issue #477) showed 15+ minutes end to end for a
single-member batch, 11 of which were Actions runner queue wait — re-validating a tree that had
already passed CI on the member's own PR, a cost paid twice per issue under merge-train.

This is not a bug report. ADR-1153's check-completeness gate and `pollTrainCI`'s backstop timeout
both behaved correctly throughout — the trial's result was simply redundant, not wrong.

**The inverse risk from #1614.** #1614 was a merge-train batch attribution bug that wrongly
*skipped* landing a merge — this issue is the opposite shape: skipping *validation*, not landing.
A false positive here would land an untested combination directly, no different in kind from
`pollTrainCI` returning a wrong green — so the guard must be gated by strictly positive evidence,
with any ambiguity falling through to the unchanged trial path (same discipline as #1615's R8 and
#1631's R3).

## Decision

Add a new, narrowly-scoped guard checked from `runMergeTrainWorker`'s re-form loop
(`engine/merge_train.go`), immediately before the trial-name/`assembleAndValidate` call, gated on
`len(current) == 1`. Three new functions, added near `landSingleton`:

1. **`singletonFastPathEligible(p trialParams, m trainMember, pr *gh.PRDetails) (bool, string)`** —
   the pure decision function. Given the member's already-fetched live PR details, checks, in
   cheapest-first order:
   - the live PR head SHA still equals the cached `trainMember.headSHA` snapshot (TOCTOU guard —
     closes the gap between batch formation and this decision);
   - `FetchCommitsBehind(owner, repo, p.baseSHA, m.headSHA) == 0` — ancestry checked against the
     **pinned** `baseSHA` already on `trialParams`, never a live `origin/<base>` read;
   - `gh.MergeableStateAccepted(pr.MergeableState)` (ADR-072) — the narrower "not dirty" check;
   - `FetchCheckRuns` + `classifyLandingCI(...) == TrainCIGreen` (ADR-1153/ADR-1441), reused
     unmodified.

   Every error return and every non-affirmative classification returns `(false, reason)` — there is
   no code path that returns eligible except by every check having been positively satisfied.

2. **`finishSingletonFastPathLanding(state *mergeTrainWorkerState, p trialParams, m trainMember)`**
   — the Done-transition sequence, modeled on `advanceConvergedPRToDone`
   (`engine/merge_gate.go`), not `landSingleton`: `recordAdvanceOutcome` (Queued → Done) →
   `closeIssueIfNonDefaultBase` (unconditional on the confirmed merge) → apply
   `fabrik:awaiting-landing-verification` only once the advance itself succeeded, with **no**
   `fabrik:credited-pr:<N>` label. `resetEjectionCount`/`resetTrialCounter` still run. A
   restart-safety branch mirrors `landMergeTrainBatch`'s own "already Done from a prior partial
   run" idiom.

3. **`trySingletonFastPath(ctx, state, p, m) bool`** — the orchestrator. Fetches the member's PR
   details once; if already `Merged` (a prior run merged but died before advancing), resumes
   straight to `finishSingletonFastPathLanding` without re-running eligibility. Otherwise runs
   `singletonFastPathEligible`, logs the disposition either way (R4), and on eligible calls
   `MergePR` then `finishSingletonFastPathLanding`. Once eligibility is confirmed positive, a
   subsequent execution failure (e.g. a transient `MergePR` error) still returns `true` — "handled,
   don't build a trial this poll" — mirroring every existing landing helper's "leave in Queued,
   retry the whole disposition next poll" idiom.

`assembleAndValidate`, `assembleAndValidateInner`, `bisect`, `landOneAtATime`, and
`landGreenBatch`'s main-moved rebase loop are all unchanged.

## Rationale

### Why a loop-level guard instead of literally inside `assembleAndValidate`?

`assembleAndValidate` has four call sites, and only one represents a genuinely-fresh Queued-batch
decision:

- **The main re-form loop** — the target of this feature.
- **`bisect`'s poisoner-isolation sub-trials.** A length-1 `half` here is being tested as part of
  isolating a poisoner in an already-red combination; the member's own-PR CI passing says nothing
  about whether it's the poisoner. Fast-pathing here would be a correctness bug, not just scope
  creep.
- **`landOneAtATime`'s one-at-a-time fallback.** Explicitly out of scope per the issue — this call
  site literally passes a single-member slice, so an in-`assembleAndValidate` guard would silently
  widen scope into a path the issue says must stay untouched.
- **`landGreenBatch`'s main-moved rebase loop.** Re-validates an *already-green* trial after the
  base moved mid-flight — a different problem (D5 recovery), not "should a trial be built at all."

Only the main-loop call site matches every acceptance criterion without also silently reaching into
bisection or the fallback path. Checking immediately before the call, rather than modifying
`assembleAndValidate` itself, makes this structural rather than conditional: the other three call
sites are categorically unreachable by this guard, not merely unlikely to trigger it. The guard
runs on every loop iteration, not just the first, so a batch that bisects down to a single clean
survivor and re-forms is also eligible.

### Why `advanceConvergedPRToDone`'s shape instead of `landSingleton`'s?

`landSingleton` is built entirely around minting a *dedicated* landing PR off a validated trial
branch (`title := "[merge-train] singleton: #%d"`, then `CreatePR`) — a shape this feature has no
trial branch to produce, by construction (the entire point of the fast path is skipping the trial).
The green-landing pipeline downstream of `assembleAndValidate` (`landGreenBatch` →
`landMergeTrainBatch`) is likewise built around a pushed trial branch and a draft CI PR that already
exists — there is no way to make the fast path "look like" a green trial result without actually
building one, which would defeat the purpose.

`advanceConvergedPRToDone` (the ordinary auto-merge path's own Done-transition function) already
does exactly what this feature needs: merge a PR that is the item's own linked PR, advance to Done,
`closeIssueIfNonDefaultBase`, apply `fabrik:awaiting-landing-verification` with no
`fabrik:credited-pr:<N>`. This is a template match, not a coincidence — the fast path is
structurally closer to an ordinary auto-merge (one PR, merged directly, `Closes #N` does the
closing) than to a merge-train batch landing (a synthetic PR aggregating several members).

### Why does `classifyLandingCI` (ADR-1153) govern mergeability, not ADR-072's weaker single-PR bar?

ADR-072 established `mergeable_state ∈ {clean, unstable}` as sufficient for `MergePR`'s own
self-gate — a *human-observed* single-PR case (someone chose to invoke `MergePR` on this PR right
now). ADR-1153 established the stricter check-run-based standard because `mergeable_state` alone is
insufficient evidence of CI completeness for an *unattended* batch merge. This fast path is
unattended — no human reviews it in the moment, the same posture ADR-1153 targeted — so it reuses
`classifyLandingCI` unmodified for the CI-completeness half of the decision. `gh.MergeableStateAccepted`
(ADR-072's narrower "not dirty" check) still governs the separate, uncontroversial "no merge
conflict" half — `classifyLandingCI`'s own doc comment specifies that dirty is checked by the caller
before it is reached, and R1.3's mergeability condition is satisfied by this narrower check alone.

### Why ancestry against the pinned base SHA, and why not reuse `trialBehind`'s polarity?

R3 requires the ancestry check to use the pinned base (captured once at batch start, ADR-059 D-b),
never a live `origin/<base>` read, so a base that moves mid-flight cannot make a stale decision look
current. `trialBehind` (the existing "has main moved past this trial" check used by
`landGreenBatch`'s D5 recovery) computes the same underlying quantity (`FetchCommitsBehind`), but
treats a fetch error as "assume up to date" — correct for its own purpose (a lower-stakes decision:
whether an *already-green* trial needs re-validating), the *opposite* of what this feature needs.
`singletonFastPathEligible` is a new, independent wrapper around the same client call with inverted
error handling: any `FetchCommitsBehind` error returns not-eligible, never assumed-ancestor.

### Why is `closeIssueIfNonDefaultBase` required here for the first time in merge-train?

Both existing merge-train landing paths (`landMergeTrainBatch`, `landSingleton`) always call
`CloseIssue` explicitly on the member issue, regardless of base — neither has ever depended on
`Closes #N` auto-close, so neither needed this guard. This fast path deliberately relies on the
merged PR's own `Closes #N` (that reliance is the entire point of R5 — land the member's own PR
directly rather than minting a new one), but GitHub only auto-honours that keyword into the
repository's *default* branch (#1096). Omitting this call would silently reintroduce the
landed-but-open-issue bug for a `base:<branch>`-labeled Queued member — nothing today prevents such
a member from reaching Queued, even though the trial itself is always built against
`wm.DefaultBaseBranch()` (an existing, unrelated quirk out of scope here).

### Why does the fast path need no interaction with the runaway guard or trial counter?

`e.recordTrial`, the wrapper around `assembleAndValidate` that feeds `isRunawayTripped`, only fires
for non-green results — its own doc comment notes a green result is "never a 'zero successful
lands' event." The fast path never calls `assembleAndValidate` at all, so it is structurally outside
`recordTrial`'s scope, exactly like every other successful land. A landed fast path is unambiguous
progress, identical in kind to any other green land.

## Consequences

**Positive:**
- The common case — a single Queued member already up to date with base, with green CI — lands in
  one poll cycle with zero trial-branch assembly, zero draft CI PR, and zero re-run of a CI suite
  that already passed on the member's own PR. On CI-queue-bound repos this removes the dominant cost
  of landing a singleton issue.
- The guard is fail-closed by construction: every unmet condition, and every API error, falls
  through to the unchanged trial path. The regression that matters most (AC2 — base moved since
  pinning) cannot silently regress into a false fast-path, since the ancestry check has no code path
  that defaults to eligible.
- No change to `assembleAndValidate`, bisection, the one-at-a-time fallback, or the D5 main-moved
  recovery loop — every existing merge-train test for those paths continues to pass unmodified.

**Negative / Trade-offs:**
- A third landing path now exists with different labeling semantics from the other two merge-train
  paths (no `fabrik:credited-pr:<N>`) — anyone extending `docs/state-machine.md` §6.19's landing-path
  table or `fabrik:credited-pr:<N>`'s consumers must account for three shapes, not two.
- The mergeability check (`pr.MergeableState`) reflects the PR's *live* declared base at its current
  tip, not a check computed against the pinned base SHA specifically — GitHub offers no way to
  compute mergeability against an arbitrary historical SHA pair without doing the merge locally
  (which is exactly what the trial does). Accepted as sufficient evidence, gated behind the ancestry
  check already having passed: mergeable against a live, possibly-newer base implies mergeable
  against the pinned subset.
- `singletonFastPathEligible` adds up to three extra API calls (`FetchCommitsBehind`,
  `FetchCheckRuns`, and the `FetchPRDetails` already fetched by the caller) per poll iteration for a
  length-1 batch that doesn't yet qualify — bounded and cheap relative to the trial cycle it may
  replace, but not free on a batch that persistently fails one condition (e.g. CI still pending).

## Sibling Audit

`landOneAtATime`'s fallback role and `landSingleton`'s dedicated-PR landing shape are unchanged —
this feature is reachable only from a batch that starts as a singleton at the top of the re-form
loop, never from the bisection-driven fallback path. `landGreenBatch`'s main-moved rebase loop
(D5 recovery of an already-green trial) is unaffected, confirmed by inspection: since the guard is
not inside `assembleAndValidate`, that call site is structurally untouched.

**References:** [ADR-1153: Train CI Completeness over Mergeable State](1153-train-ci-completeness-over-mergeable-state.md), [ADR-1441: Unstable Requires Check-Run Classification](1441-unstable-requires-check-run-classification.md), [ADR-072: MergePR Self-Gates on Mergeable State](072-mergepr-self-gates-on-mergeable-state.md), [ADR-1616: Post-Done Landing Verification](1616-post-done-landing-verification.md), [ADR-1096: Explicit Close on Non-Default-Base Merge](1096-explicit-close-on-nondefault-base-merge.md), [ADR-059: Internal Merge Train](059-internal-merge-train.md), issue #1614, issue #1615
