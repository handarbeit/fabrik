# ADR 1616: Post-Done Landing Verification

**Date**: 2026-08-16
**Status**: Accepted
**Issue**: #1616 — No post-Done verification that the work actually landed — a false 'COMPLETED' is undetectable

## Context

Nothing in Fabrik ever verified that an issue advanced to Done actually landed. The Done transition
is driven entirely by *inferred* success — a merge call returned, or a PR was believed merged —
never by observing the credited work actually present on the base branch. That inference held until
#1614, where a merge-train batch attribution bug closed two issues as `COMPLETED` whose work was
never merged. #1615 fixes that specific root cause; this issue adds the cause-agnostic backstop that
would have caught it, and would catch the next one regardless of cause.

The failure class is unusually dangerous because it is silent and self-certifying: the issue reads
`COMPLETED`, the PR reads landed, the Validate report reads green, and no dispatch path ever revisits
a Done item — there is no signal for anything to act on, so detection cannot rely on anything
noticing later. #1614 was found only because someone happened to re-run an old reproduction days
afterward.

### Mechanism selection: branch-tip ancestry was tried and rejected first

The issue's original R1 proposed asserting the item's branch tip is a literal git ancestor of its
base branch. Before implementation, the issue author hand-ran this check against the 25 most
recently closed issues in this repo and found three false positives (#1573, #1523, #1520): each had
genuinely landed, but their branches were rebased onto a later base *after* landing, so the same
content exists on base under different commit SHAs while the branch tip is no longer an ancestor. A
merge-train member is especially prone to this — the train merges the *trial* branch, and the
member's own branch then drifts independently afterward.

A follow-up review (during Specify) identified a second, structural problem with the same mechanism:
under `--auto-merge-strategy SQUASH` or `REBASE`, GitHub creates a new commit object on the base
branch independent of any rebase-after-merge — so a merged PR's original branch-tip commit is
**never** a literal ancestor, in any repo using a non-`MERGE` strategy, regardless of drift. This
would have false-positive-reopened every correctly-shipped issue in such a repo, not just drifted
ones — a guaranteed failure, not an intermittent one, and the same "appears to protect, silently does
nothing" shape as #1614 itself.

Both findings point the same way: **branch-tip ancestry cannot express the property this issue
needs, under any scoping.** R1 was rewritten to enumerate three candidate mechanisms instead:

1. **Merge-state of the crediting PR** — assert the PR credited for the Done transition reached
   `MERGED`. Strategy-agnostic; targets #1614's actual defect (a credit naming a batch that never
   carried the work) directly; no rebase sensitivity, since it never inspects the branch at all.
2. **Content presence** — assert the member's changes are reachable on base (`git cherry`, patch-id
   equivalence, or the merge commit that carried the trial). Robust to rebase, more expensive, and
   fragile under `SQUASH` (patch identity is not preserved across a squash).
3. **Merge-commit provenance** — record the merge commit at Done time, verify it's on base later.
   Cheap and exact, but requires capturing state at the transition rather than reconstructing it
   after the fact, and no merge-commit SHA is fetched anywhere in the codebase today.

## Decision

**Mechanism: option 1 (merge-state of the crediting PR) alone.** No content-intersection layering —
it reintroduces content-identity reasoning that `SQUASH` already makes unreliable, for no added
coverage once branch inspection is off the table entirely. This mechanism is immune by construction,
not by handling, to both false-positive classes found above: it never inspects the item's own
branch, so a rebased-then-landed branch and a `SQUASH`/`REBASE`-merged branch are both simply outside
what it examines. It also satisfies R3 (a branch deleted after a real merge is not a failure) the
same way — the branch is never examined.

**Discovery: a durable `fabrik:credited-pr:<N>` label recorded at the Done transition, not
comment-parsing.** Three landing paths write to Done (merge-train batch, merge-train singleton,
ordinary auto-merge), and `advanceToNextStage`'s bare Status-field mutation carries no PR/merge
identity forward. The ordinary auto-merge path's credited PR is always the item's own linked PR,
durably rediscoverable later via `FetchLinkedPR` — no new label needed. The two merge-train paths
credit a *different* PR (the integration or singleton landing PR, distinct from the member's own PR,
which stays closed-not-merged) with no existing durable pointer to it after the fact.

An earlier design considered parsing the existing `"Landed via batch PR #N"` comment those paths
already post — rejected because `addLandedCommentWithRetry` is best-effort (three attempts, then a
silent log warning) and the pattern is coupled to exact prose with no test tying the two together. A
verifier that goes quiet exactly when the landing path is already misbehaving is not a backstop.
Recording `fabrik:credited-pr:<N>` as a label at the transition itself — cheap (the number is already
in hand at that point), durable (a GitHub label survives a restart; `itemstate` does not, and GitHub
exposes no change-history equivalent to backfill it), and consistent with this repo's "labels are
state" convention — removes that dependency entirely.

**Scope: marker-driven (`fabrik:awaiting-landing-verification`), not backlog-driven.** Applied at the
Done transition itself by all three call sites, only after their own advance actually succeeds. Old
Done items — anything that landed before this feature shipped — never had the marker applied, so
they are naturally out of scope: no backfill pass, no risk of misclassifying a pre-feature
merge-train landing (whose member PR is legitimately closed-not-merged) as a failure.

**Two failure regimes, never conflated:**
1. **Confirmed failure** (credited PR resolves and `FetchPRMerged` definitively reports not merged)
   — fires immediately, never gated by the retry budget, satisfying AC1's "within one poll cycle."
2. **Inconclusive** (API error, or no crediting reference discoverable at all) — the existing
   `recordSettleRetry`/`escalateSettle` idiom (`engine/settle.go`), bounded by `MaxRetries`,
   escalating to `fabrik:paused` — never reopens the issue on ambiguity (R5, AC4).

**On confirmed failure:** reopen the issue, move its board Status back to the fixed target
`Validate` (uniform across all three landing paths — Validate already knows how to attempt/re-attempt
landing), apply the distinguishing label `fabrik:landing-verification-failed`, and post a comment
naming the branch, the resolved base (honoring `base:<branch>` — R4), and the credited PR checked and
found not merged.

## Rationale

### Why a label instead of an `itemstate` field for the crediting PR?

`itemstate.Store` is purely in-memory with no cross-restart persistence, and GitHub exposes no
change-history equivalent to backfill it after a restart — a design that captures ephemeral in-memory
state at Done time and expects it to survive to the next poll would silently lose the crediting-PR
reference across any restart between the Done transition and the settle scan's next pass. A label is
the only storage that survives.

### Why is R4 (`base:<branch>`) satisfied "for free," with no active check gating the merge decision?

Verifying "did the credited PR merge" needs no base-branch resolution at all — the credited PR's own
base is whatever it was opened against, already `base:<branch>`-correct at PR-creation time (#1554).
`baseBranchForItem`/`WorktreeManager` resolution is used only to *name* the base in the failure
comment (best-effort, degrading gracefully when no `WorktreeManager` is registered — mirroring
`closeIssueIfNonDefaultBase`, ADR-1096/§6.13) — never as an input to the merge/no-merge decision
itself. Making it an input would reintroduce exactly the branch-dependent reasoning the mechanism was
chosen specifically to avoid.

### Why "move it off Done" targets a fixed `Validate`, not the merge-train holding stage or a generic predecessor?

The issue's R2 wording ("reopen... apply a distinguishing label... post a comment") reads as
human-gated — parked for inspection, not silently self-healing back into automated re-landing. Every
existing settle-scan escalation in this family (ADR-060/-061/-1097/-1422/-1533) shares that posture:
`fabrik:paused` on exhaustion, not a silent retry loop. Validate is also the one stage all three
landing paths share as their immediate predecessor conceptually, and it already knows how to
attempt/re-attempt landing (auto-merge, or re-entry into the merge-train holding stage on its next
cycle) once an operator clears the pause and re-triggers it — so parking there does not strand the
item somewhere with no forward path, it just requires a human first.

### Why does `fabrik:awaiting-landing-verification` gate on the advance actually succeeding, unlike `closeIssueIfNonDefaultBase`'s unconditional call?

`closeIssueIfNonDefaultBase` (ADR-1096) runs unconditionally regardless of the board-advance
outcome because its own action (closing the issue) is justified by the *confirmed merge* alone,
independent of whether the board Status update itself succeeded. This marker's entire purpose is the
opposite: it asserts "this item just reached Done via a merge," so applying it when the advance
*failed* (e.g. a missing target Status option) would be a false claim the engine never actually made
— the item is still sitting at its prior stage. Confirmed directly by a pre-existing test
(`TestRunValidatePRTerminalAdvance_SettlesIntoStablePause`) that exercises exactly this failure mode
and asserts zero unrelated label mutations on that pass.

### Why `settleLandingVerification` instead of extending an existing settle scan?

Every candidate (`settleNonDefaultBaseCloses`, `settleAwaitingAdvanceScan`) owns a structurally
different single-call retry, not a two-regime (immediate-failure vs. bounded-retry) decision. Folding
this into either would either lose the "fires within one poll cycle" property AC1 requires, or
misapply that immediacy to a call that has no such requirement. A dedicated scan, reusing the shared
`recordSettleRetry`/`clearSettleMarker`/`escalateSettle` helpers (`engine/settle.go`) like every
sibling in the family, keeps the two-regime logic in exactly one place.

## Consequences

**Positive:**
- A false `COMPLETED` of #1614's shape — Done + closed issue + a credited PR that never actually
  merged — is now detected and reversed within one poll cycle, regardless of what caused the false
  credit.
- The mechanism is immune by construction, not by special-casing, to both confirmed false-positive
  classes (rebase-after-landing, `SQUASH`/`REBASE` merge strategies) — it structurally cannot inspect
  the properties that produced them.
- No change to merge-train assembly, bisection, or batching — every write this feature makes is a
  label applied *after* `advanceToNextStage` has already succeeded.
- Reuses the same generic settle/escalate helpers as six prior instances of this pattern
  (ADR-1270 and siblings) — the ninth instance, not a new mechanism.

**Negative / Trade-offs:**
- A merge-train `fabrik:credited-pr:<N>` label write that fails outright (after its landing call
  site's `advanceToNextStage` already succeeded) leaves that landing **unverified** — the item is not
  marked at all, so the backstop simply does not run for it, reverting to the pre-#1616 status quo for
  that one landing. This is deliberate: `markCreditedLanding` checks the credited-PR write's error and
  applies `fabrik:awaiting-landing-verification` only on success, because a marker without a credited-PR
  record would send the scan to its `FetchLinkedPR` fallback, which for a merge-train member resolves
  to the member's own closed-not-merged PR and would fire an **immediate false confirmed-failure**
  against a correctly-landed issue — the exact false positive AC2 forbids. Not verifying is a coverage
  gap; falsely reversing a good landing is a regression, so the gap is the safer failure mode. The gap
  is logged loudly at the landing call site.
- The confirmed-failure remediation is **human-gated, not self-healing**: the board Status moves back
  to `Validate` but `stage:Validate:complete` is deliberately left in place, so Validate does not
  re-dispatch on its own. That is the posture R2 asks for, but a board move that doesn't re-dispatch is
  the same hazard `docs/state-machine.md` §6.16/#1545 R4 documents elsewhere, so the failure comment
  states it explicitly and names `fabrik:revalidate` as the way to actually re-run the stage.
- `advanceToNextStage` and the marker writes are separate, non-atomic calls, so a run can advance a
  merge-train member to Done and die before `markCreditedLanding` executes. Both merge-train landing
  paths therefore also call `markCreditedLanding` on their pre-existing "member already in Done column"
  restart-safety fast path, instead of skipping past it — otherwise that item would be permanently
  unverified (nothing ever revisits a Done item), leaving the backstop silently absent for exactly the
  merge-train restart scenario #1614 came from. The writes are idempotent, so re-marking an
  already-verified item costs at most one redundant `FetchPRMerged` that clears the markers again.
- `resolveLandingVerificationBranchInfo`'s base-branch resolution for the failure comment depends on
  a `WorktreeManager` being registered for the item's repo; if none is (e.g., a repo whose only
  activity this process lifetime was a merge-train landing, after a restart), the comment names no
  base — degraded, not blocked, mirroring `closeIssueIfNonDefaultBase`'s own precedent.
- This is a backstop, not a fix for #1614's root cause — it does not prevent a false `COMPLETED`,
  only detect and reverse one. #1615 remains required for prevention.

## Sibling Audit

This is the ninth instance of the ADR-1270 dedicated-settle-scan pattern (`fabrik:awaiting-done`,
spawned-child board placement, merge-train singleton member-issue close, non-default-base explicit
close, the awaiting-CI settle scan, queued review-finding ejection, terminal-advance escalation, and
runaway guard alert retry preceding it). The closest structural precedent is ADR-1096/ADR-1097
(§6.13) — both are merge-adjacent, best-effort, settle-scan-backed actions gated on a confirmed
merge, both degrade gracefully when no `WorktreeManager` is registered, and both use `base:<branch>`
resolution only to *name* something in a comment, never to drive their core decision.

**References:** [ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md), [ADR-1096:
Explicit Close on Non-Default-Base Merge](1096-explicit-close-on-nondefault-base-merge.md),
[ADR-1097: Non-Default-Base Explicit Close Retry](1097-non-default-base-close-retry.md), [ADR-059:
Internal Merge Train](059-internal-merge-train.md), [ADR-050: Convergence Budget and Native
Auto-Merge](050-convergence-budget-and-native-automerge.md) (introduces `AutoMergeStrategy`), issue
#1614 (the motivating incident), issue #1615 (its independent root-cause fix)
