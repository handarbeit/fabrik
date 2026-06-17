# Fabrik State-Machine Review — 2026-06-17

Review of the engine against `docs/state-machine.md` + `docs/stage-lifecycle.md`, two angles:
(1) the spec's own completeness/correctness, (2) the code vs the spec. Plus a direct read on
the "are we in whack-a-mole territory?" question.

Method: five parallel audits (spec-internal, gate machinery, comment/marker subsystem,
dispatch/recovery/cache, regression archaeology), cross-corroborated, top structural claims
verified against code by hand.

---

## Headline verdict

**Yes — but it's localized, not systemic.** Roughly 80% of the engine is in good shape and the
spec is unusually mature. The whack-a-mole is concentrated in **one subsystem**: the
`Validate → PR-converges → board-advances-to-Done` path and the gate stack + recovery loops that
serve it. That subsystem is genuinely unstable and is being re-patched in place. The rest
(dispatch core, semaphore/worker-guard, observer/cycleSet pipeline, comment 11-step flow, spawn)
is coherent and converging.

The "stepping on itself" intuition is **correct and evidence-backed**, but the cause is not
sloppiness — it's a missing abstraction that piecemeal fixes keep circling.

---

## The root cause (one problem, three faces)

**RC1 — No single owner of the "PR converged → advance board to Done" transition.**
The decision is smeared across `checkAutoMergeConvergence` (merge_gate.go), the Phase-2 advance
in `poll.go`, `runPausedItemMergedPRRecovery` (paused_recovery.go), the janitor, and the proposed
drift-repair (#883). Because no path is authoritative, items fall *between* them (that's literally
#873: "items stay at Validate forever"), and each gap gets plugged with another scanner. There are
now **4+ overlapping recovery loops** scanning the same board for items the main path mishandled,
and their non-overlap is enforced by **hand-maintained label negations**, not structure. #883's
spec listing "7 non-negotiable race-safety invariants" is the tell: the loops now have to be
defended against each other.

**RC2 — CI/merge readiness is inferred from GitHub's eventually-consistent state with no settling model.**
`mergeable_state`, `check_runs`, and HEAD SHA are read-after-write and ambiguous in transient
windows. Every #779 → #845 → #855 → #871 fix is the engine guessing wrong during a settling window
and getting a new timing band-aid (`PostPushDwell`, `CIWaitTimeout`, `RepairDwellSeconds`). #871's
title literally says "#855 guard falls through during the post-push window." Without a real
"wait until state is stable" primitive this chain keeps producing #87x-style guards.

**RC3 — the `poll.go` catch-up loop is a god-function.**
25 touches since 2026-05-20. Every gate (review, convergence, mergeability, CI) and every recovery
loop is inlined into one `continue`-based sequence (~lines 1223–1578). New behavior = another branch,
which is *why* fixes regress each other: a guard added for one gate changes fall-through for the next.

RC1 and RC3 are the same disease at different scales. RC2 is independent and also live.

---

## Evidence the concern is real (archaeology)

- **Three documented fix-patching-fix events in one subsystem:**
  - #831 "EMERGENCY… regression from #829"
  - #835 "generalize the fallback… clean status was only the first variant" (the #831 fix was too narrow)
  - #871 "#855 mergeable_state guard falls through during the post-push window"
- **Same-day sibling bugs:** #871/#873/#874 all closed 2026-06-17 within ~25 min of each other;
  #881 opened *and* closed not-planned the same day, immediately superseded by #883 (a backstop for a backstop).
- **Velocity spike:** 34 engine commits on 06-16, 20 on 06-15, vs 1–3/day before — a burst of
  localized patching, not steady hardening.
- **Still-open upstream cause:** **#828** (Validate falsely signals `FABRIK_STAGE_COMPLETE` on a
  non-mergeable PR) is the *trigger* for the whole convergence cascade and is still unfixed. The
  convergence patches are treating symptoms while the source is live.

### The two regression chains

**Chain A — "yolo PR won't converge to merged/Done"** (dominant): #829, #831, #835, #853, #873,
#881, #883, and upstream #828. Eight issues, one subsystem.

**Chain B — "checkCIGate can't decide if CI is real"**: #779 → #845 → #855 → #871. Four issues,
all the same question ("is this PR's CI actually green?"), each fixing a corner the previous exposed.

**Chain C — "stuck items need yet another recovery loop"**: R4 paused-CI recovery → #874 (R4 handles
`wait_for_ci` but not `wait_for_reviews` — same loop, missing branch) → `runPausedItemMergedPRRecovery`
→ janitor → #872 stranded-worker → #881/#883 drift-repair. Backstop proliferation.

Healthy/separate tracks (NOT whack-a-mole): #847 history accounting, #815/#797/#884/#886 (distinct
features), spawn subsystem. These are normal backlog.

---

## Angle 1 — spec completeness/correctness

The spec is mature and battle-tested; weaknesses cluster exactly in the conjunctive/multi-handler
regions and in the diagrams/tables that haven't kept pace with recent additions.

**High:**
- **New user comment while a non-pause gate (CI / review / rebase / convergence) is active is
  undefined.** §2.2 covers only unpaused-active / paused / awaiting-input. An `awaiting-ci` or
  `awaiting-review` item is neither — guard 12 (§8.1) would dispatch `processComments` (which pushes
  commits), racing the catch-up-loop reinvoke worker. Reachable, common, genuine race potential.
  *(Corroborated by the comment-subsystem audit: item.go:497 short-circuits to processComments
  before any gate consideration.)*
- **Conjunctive CI ∧ review gate has no described joint-clearing path and no test.** With both flags
  set, only `awaiting-ci` is seeded; review seeding is suppressed (#617). After CI clears, review is
  checked only on the *next* poll via the `hasComplete` path — a two-poll handoff with no atomicity
  statement, and a reviewer who comments during CI-await is ignored until CI is green.
- **`fabrik:awaiting-ci` has too many clear-path claimants** (`checkCIGate` sub-rules,
  `attemptMergeOnValidate` shortcut, closed-item cleanup, never-running-check guard, pause paths,
  R4 recovery) with no stated precedence when an item is simultaneously Validate + yolo + `wait_for_ci`.

**Medium:**
- `fabrik:rebase-needed` is dual-owned (`checkMergeabilityGate` and `checkAutoMergeConvergence`) with
  no stated owner when both `auto-merge-enabled` and `rebase-needed` are present.
- `pauseForRebaseCycleLimit` retains `rebase-needed`, but the doc never says whether unpause/`clearFailedStage`
  clears it — an unpaused item can re-enter the rebase gate with a stale label.
- Stage-lifecycle vs state-machine disagree on the Validate/yolo merge mechanism: §3.1 table row
  (line 458) still describes a synchronous "PR merged; advance to Done", contradicting the deferred
  convergence-monitor model in §5.5.
- §2.13 (assignee), §2.15 (revalidate), §2.16 (SHA-invalidation), §5.6 (`FABRIK_PR_CREATE`),
  §6.7 (`FABRIK_NO_WORK_NEEDED`) are **absent from the §3 transition tables and §10 diagrams**, which
  claim to cover "every reachable state." The Validate re-entry back-edge is invisible to a
  diagram-only reader.

**Low:** half-unpaused state (`awaiting-input` without `paused`) never walked; `extend-turns` removal
site attributed to different functions across docs; comment-extension gating described two ways
(§1.2 line 86 vs §4.3).

---

## Angle 2 — code vs spec (confirmed divergences)

**Verified by hand:**
- **Split-brain mergeability signal.** §6.5.2 claims the merge gate is the single authority for
  mergeability, but `checkMergeabilityGate` reads `mergeable` (bool, `merge_gate.go:52`) while
  `checkCIGate` independently reads `mergeable_state` (string, `ci.go:91`) and clears the whole gate
  on `clean`/`unstable`. A PR can be `mergeable==true` while `mergeable_state` is `behind`/`blocked`;
  the two gates can disagree on the same PR in one poll. The spec's "single source of truth" is two sources.
- **Recovery-loop negation coupling (the #874 bug class is structurally still open).**
  `paused_recovery.go:35` runs only for `paused + (awaiting-ci OR awaiting-review)`; the convergence
  recovery loop runs for `paused + NOT(awaiting-ci OR awaiting-review) + complete`. Disjointness is a
  hand-maintained negation. Any future pause path that sets a *different* label (exactly #874's shape)
  falls through **both** loops and strands the item. `paused_recovery.go:80` also silently skips
  non-gated intermediate stages.

**Reported, high-confidence:**
- **"Only place `stage:X:complete` is added for wait_for_ci stages" (§6.4.1) is already false.**
  `addCompleteLabelAndRemoveCI` (ci.go) AND the paused-recovery loop both add it; mutual exclusion
  rests *only* on the `fabrik:paused` predicate being checked identically at 3+ independent sites.
  This is the canonical "fixes to one gate violate an invariant another silently depends on."
- **Cross-mode `RebaseCycles` contamination.** Convergence rebase (merge_gate.go:~370) increments
  `RebaseCycles` but never checks `MaxRebaseCycles`; the merge-conflict gate (poll.go) *does* enforce
  it. An issue that leaves convergence mode (user toggles `auto-merge-enabled`) without passing through
  `clearFailedStage` carries a polluted counter and can trip the limit on the first real conflict.
- **`FABRIK_CONVERGENCE_BUDGET=0` + unresolvable conflict = infinite rebase dispatch, no pause.**
  Budget check skipped, `MaxRebaseCycles` not consulted — the only gate with literally no exit.
  Documented as intended, but it's a real no-exit livelock.
- **Recent commit `5acf2609` introduced a stage-path/comment-path asymmetry.** `comments.go:229` now
  sets `completed := err == nil && checkCompletion(...)`, so `FABRIK_STAGE_COMPLETE` + trailing
  non-zero exit completes the stage when run as a *stage* (item.go:734 returns completed=true) but
  **not** when run via comment processing. `stage-lifecycle.md:502` still claims parity ("honored even
  on non-zero exit… stage runs AND comment processing"). A recent fix made the doc false — micro-whack-a-mole.
- **`FABRIK_BLOCKED_ON_INPUT` is silently stripped, never honored, during comment processing**
  (comments.go:267). A comment-triggered run that needs input posts output, rockets the comment, and
  goes quiet with no `awaiting-input` @mention. §4.2's 11-step table never mentions the marker.

**Reported, medium:**
- Stranded-worker handle blocks *dispatch* for up to `WorkerStaleTimeout` (default 5 min), indefinitely
  on Windows: the dispatch guard (poll.go:~1624) is bare `snap.Worker() != nil` with no `isWorkerStale`
  qualifier (the janitor has the qualifier; the dispatcher doesn't). This is the #872 class, only
  partially closed.
- `LightReconcile` → `Reconcile(freshBoard)` can clobber a webhook label-delta applied in the window
  between the fetch and `Pause()` (boardcache), until the next deep-fetch heals it.
- Idle backoff never engages while any `awaiting-ci` / `auto-merge-enabled` / `rebase-needed` item sits
  on the board (bypass labels force a deep-fetch → `result.Active=true`). A single stuck auto-merge item
  pins the poll loop at base interval, negating the 60-min idle cap.
- Three different lists for the "bypass" label set: poll.go:1146 (5 labels) vs Appendix B (3) vs §9.8
  table (2, omits `awaiting-review`).

---

## Recommendation (sequence matters)

1. **Fix #828 first.** Stop Validate emitting `FABRIK_STAGE_COMPLETE` on an aborted-rebase /
   non-mergeable PR. This is the *source* that feeds the entire Chain A cascade — fixing it kills a
   class of downstream auto-merge / 405-loop / cycle-limit-pause churn at the root, cheaply.
2. **Do NOT ship #883 as currently specced.** As a fifth scanner that must be defended against loops
   1–4, it *deepens* RC1. Instead, consolidate the Phase-2 advance + `runPausedItemMergedPRRecovery`
   + convergence recovery + drift-repair into **one authoritative "settle PR → advance" state owner**,
   keyed on "PR merged/closed regardless of which gate label is present." That single change collapses
   R4/R4b and the negation coupling, and closes the #874 bug class structurally.
3. **Introduce a settling primitive for GitHub state** (RC2): one function that answers "is this PR's
   merge/CI state stable and final?" with the dwell/timeout logic in one place, consulted by both gates.
   Kills the #779/#845/#855/#871 band-aid sequence and the split-brain (`mergeable` vs `mergeable_state`).
4. **Decompose the `poll.go` catch-up loop** (RC3). The extraction of `runPausedItemMergedPRRecovery`
   and `board_drift.go` is the right instinct, unfinished. Each gate/recovery concern should be a named
   function with an explicit precedence list — the precedence is currently implicit in `continue` order.
5. **Spec hygiene** (cheap, do alongside): pick single owners for `awaiting-ci` / `rebase-needed`
   clearing and state them as invariants; define "new comment during an active gate"; regenerate the
   §3 tables and §10 diagrams to include revalidate / SHA-invalidation / convergence / no-work / PR-create;
   fix the `stage-lifecycle.md:502` parity claim broken by `5acf2609`; reconcile §6.4.1's "only place"
   invariant with reality.

### One-line answer to "has Fabrik started stepping on itself?"
In the convergence/gate/recovery subsystem, yes — and the fix is consolidation (one transition owner +
one settling primitive), not another scanner. Everywhere else it's healthy. Resist adding loop #5 until
loops #1–4 are merged.
