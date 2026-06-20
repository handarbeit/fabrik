# ADR 056: Consolidate the Convergence / Gate / Recovery Architecture

**Date**: 2026-06-17
**Status**: Accepted (implementation tracked by the dependency-chained issues #828 → #888 → #887 → #889 → #890)
**Issues**: #828, #888, #887, #889, #890 — chained via `blockedBy`; each carries its own self-contained spec
**Supersedes**: ADR-053 (paused-item recovery loop — the *separate-loop-per-case* pattern)
**Amends**: ADR-033 (mergeable-state over check-runs), ADR-050 (convergence budget + native auto-merge), ADR-032 (CI-gate conjunctive completion label)
**Review**: `notes/state-machine-review-2026-06-17.md`

## Context

A structured review on 2026-06-17 of the engine against `docs/state-machine.md` found the
`Validate → PR converges → board advances to Done` subsystem — its gate stack (review / CI /
merge-conflict / convergence) and its recovery loops — in whack-a-mole territory. The rest of the
engine (dispatch core, worker-guard, observer/cycleSet pipeline, comment 11-step flow, spawn) is
coherent. This decision is scoped to the convergence subsystem only.

The evidence that fixes were regressing each other rather than converging:

- **Documented fix-patching-fix events**: #831 "regression from #829"; #835 "clean status was only
  the first variant" (the #831 fix was too narrow); #871 "#855 mergeable_state guard falls through
  during the post-push window."
- **Backstop-for-a-backstop**: #881 opened and closed not-planned the same day, superseded by #883
  (PR #885), whose spec required "7 non-negotiable race-safety invariants" so the new scanner would
  not fight the existing four.
- **Churn**: 34 engine commits on 2026-06-16 alone, concentrated in `poll.go` / `ci.go` /
  `merge_gate.go` / `paused_recovery.go`.

### Three structural root causes

1. **No single owner of the "PR converged → advance to Done" transition.** It is spread across
   `checkAutoMergeConvergence` (`merge_gate.go`), the Phase-2 advance in `poll.go`,
   `runPausedItemMergedPRRecovery` (`paused_recovery.go`), the janitor, and the proposed #883
   drift-repair scanner. Items fall *between* these paths (that is #873: "items stay at Validate
   forever"). Their non-overlap is enforced by **hand-maintained label negations**:
   `paused_recovery.go:35` runs for `paused + (awaiting-ci OR awaiting-review)`; the convergence
   recovery loop runs for `paused + NOT(awaiting-ci OR awaiting-review) + complete`. Any future
   pause path that sets a *different* label falls through **both** loops and strands the item —
   which is exactly what #874 was (handled `wait_for_ci` but not `wait_for_reviews`). The bug class
   is structurally open and each instance has been closed by adding another loop.

2. **GitHub merge/CI readiness is read with no settling model, by two gates reading two signals.**
   `checkMergeabilityGate` reads `mergeable` (bool, `merge_gate.go:52`); `checkCIGate` independently
   reads `mergeable_state` (string, `ci.go:91`) and clears the whole gate on `clean`/`unstable`. A
   PR can be `mergeable == true` while `mergeable_state` is `behind`/`blocked` — the two gates
   disagree on the same PR within one poll. Every #779 → #845 → #855 → #871 fix is the engine
   guessing wrong in a read-after-write window and getting a new ad-hoc timer (`PostPushDwell`,
   `CIWaitTimeout`, `RepairDwellSeconds`).

3. **The `poll.go` catch-up loop is a god-function** (~lines 1223–1578, ~25 touches since
   2026-05-20). Every gate and recovery loop is inlined into one `continue`-based sequence, so a
   guard added for one gate silently changes fall-through for the next.

### Why ADR-053's reasoning no longer holds at scale

ADR-053 chose "a separate loop, not an exception in the main catch-up loop" to avoid punching a hole
in the `isPaused` guard. That local reasoning was sound for *one* loop. It does not compose: each new
recovery condition became another disjoint-by-negation loop, and the Nth loop must be defended
against the prior N−1 (the #883 "7 invariants" requirement is the symptom). The cost of correctness
is now super-linear in the number of recovery paths. The pattern itself is the problem.

## Decision

Stop adding recovery scanners. Consolidate the subsystem along three axes.

### D1 — One settling primitive for GitHub PR merge/CI state (#888)

A single function/type answers "is this PR's merge + CI state final and stable?", consolidating
`mergeable`, `mergeable_state`, `check_runs`, HEAD-SHA freshness, and the post-push dwell into one
typed result (e.g. `stable+mergeable`, `stable+blocked/conflicting`, `unsettled`, with a reason).
`checkMergeabilityGate` and `checkCIGate` both consume it; neither reads the raw signals
independently. This eliminates the split-brain and folds the #779/#845/#855/#871 timer band-aids
into one settling model. `unsettled` produces no label churn (preserves ADR-032's R10c principle).

This **amends ADR-033**: `mergeable_state` is still preferred, but it is no longer read in two places
with two interpretations — the settling primitive is the sole interpreter.

### D2 — One authoritative "settle PR → advance to Done" owner (#887)

A single owner performs: linked PR observed terminal (merged, or closed-without-merge) → fill any
missing gate-checked `stage:<X>:complete` labels → advance the board column → for yolo trigger Done,
for cruise stop at Validate-complete. It runs **regardless of which gate label is present** — no
disjointness maintained by label negation anywhere. It collapses and removes the Phase-2 advance
special-case, `runPausedItemMergedPRRecovery`, and the convergence-paused recovery loop, and it
**absorbs #883's drift-repair intent** as a normal consequence of the advance path rather than a
separate race-guarded scanner. The drift *detection warning* (#873 / PR #880, merged) stays.

This **supersedes ADR-053**: the merged-while-paused self-heal (Case A) and its #874 amendment are
subsumed by the single owner. ADR-053's preserved constraints (no goroutines/semaphore in the
recovery path, `e.client` for freshness, unconditional advance on terminal merge) carry forward into
the owner — only the *separate-loop* structure is replaced.

### D3 — Explicit gate precedence (#889)

The catch-up loop's gate/recovery handlers become an ordered, individually-tested list with
precedence expressed as data, not implied by `continue` placement. ADR-028's "merge gate before CI
gate" ordering becomes structurally enforced rather than positional.

### D4 — Fix the source first (#828) and reconcile the spec (#890)

#828 (Validate falsely signals `FABRIK_STAGE_COMPLETE` on a non-mergeable PR) is the upstream
trigger of the cascade and is sequenced first — it stops the lie cheaply before any structural work.
#890 reconciles spec invariants the code already violates (notably ADR-032 / §6.4.1's "only place
`stage:X:complete` is added", which is false today) and defines the undefined "new comment during an
active gate" transition.

### Sequencing

`#828` (stop the source) → `#888` (one truth about PR state) → `#887` (one advance path consuming it;
close #883/#885 here) → `#889` + `#890` (lock in structure and docs).

## Consequences

- The #874 bug class is closed **structurally**: a new pause-label variant cannot fall through,
  because advancement no longer keys on gate labels at all.
- The `mergeable` vs `mergeable_state` split-brain is eliminated; future GitHub-eventual-consistency
  bugs have one place to be fixed instead of two gates plus N timers.
- Net code reduction in the convergence subsystem: four recovery paths + the Phase-2 special-case
  collapse to one owner; scattered timers collapse to one settling primitive.
- **Reversal cost**: #883/PR #885 (built, race-guarded drift-repair scanner) is closed not-merged.
  Its branch is preserved; if #887 proves insufficient the scanner can be reconsidered.
- **Risk**: the consolidation touches the hottest, most incident-prone code in the engine. It must
  land behind the existing gate test suite (unchanged behavior for D3) plus new tests proving single
  dispatch / no double-advance / no strand across `{gate label} × {PR merged/closed/open}`, and a
  regression test that introduces a synthetic third gate label to prove the #874 class is closed.
- Until the consolidation lands, treat **any** newly-proposed recovery scanner as a regression of
  this decision. The answer to a stranded-item bug is "extend the owner," not "add loop N+1."
- **Cruise items never enter `checkAutoMergeConvergence`** because cruise suppresses auto-merge at
  Validate (`engine/poll.go`: `if !yoloActive { continue }`). The `fabrik:auto-merge-enabled` label
  is therefore never applied to cruise items; the engine stops at `stage:Validate:complete` and waits
  for a human merge. This is verified end-to-end by `TestCruiseFullPipeline` (`tests/e2e/cruise_test.go`).

## Non-goals

This ADR does not change the human-in-the-loop pause semantics (ADR-008), the rebase-by-Claude
decision (ADR-028 §6.5.4), the convergence budget concept (ADR-050) — only *where* and *how many
times* PR-state is interpreted and the board is advanced.

## Addendum (2026-06-19): closed-issue admit gate — the strand lived one layer up

The single settle-owner `runValidatePRTerminalAdvance` is gate-label-agnostic, but it only ever
iterates items that survive the closed-issue admit gate in `itemMayNeedWork` / `itemNeedsWork`
(`engine/item.go`). That gate admitted closed items only for `stage:<X>:complete`,
`fabrik:awaiting-ci`, or `fabrik:auto-merge-enabled`. A PR merged externally while the issue was
paused at Validate with **`fabrik:awaiting-review`** (or bare `fabrik:paused`, or no gate label)
closed the issue via `Closes #N`; on the next poll the closed item was dropped at this gate, so the
settle-owner never saw it and the item stranded (closed, paused, no `stage:Validate:complete`). The
`awaiting-ci` variant only worked because that one label was already on the allowlist.

This is the same #874 strand the consolidation set out to kill — the label coupling simply survived
one layer **upstream** of the owner. Consistent with this ADR ("extend the owner, not add a
scanner"), the fix widens the admit gate rather than adding a recovery loop: a closed item at a
**gate-checked stage** (`wait_for_ci` / `wait_for_reviews`, via `stageIsGateChecked`) lacking its
`stage:<X>:complete` label is admitted regardless of gate label, so the owner can observe the
terminal PR and advance/heal it. Surfaced by `TestPausedMergedPRRecovery` (the awaiting-review and
no-gate-label variants) once its harness race was removed; guarded by unit tests in
`engine/closed_admit_settle_test.go`.
