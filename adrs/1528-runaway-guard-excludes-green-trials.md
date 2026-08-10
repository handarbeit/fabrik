# ADR 1528: Runaway Guard Excludes Green Trials From Its Counter

**Date**: 2026-08-10
**Status**: Accepted
**Issue**: #1528 — merge-train: a successful bisection trips the runaway guard, pausing the
clean members it just vindicated

## Context

The runaway guard (ADR-059 D8) exists to detect a repo-wide infra failure — billing-blocked
CI, a broken base branch — from the outside, without classifying *why* trials fail: if a repo
accumulates `MaxTrainTrialsPerWindow` trial branches with **zero successful landings** within
a rolling window, pause every Queued member and stop dispatching.

`recordTrial(repoKey)` was called unconditionally at the top of `assembleAndValidate`
(D8's own "Design" bullet: *"so every trial — initial batch, bisection sub-trials, and
one-at-a-time singletons — counts"*). This premise is wrong for bisection. Isolating a
poisoner in a 3-member batch takes exactly 6 trials (initial red, 4 alternating
green/red probes, 1 green survivor-validation trial that confirms the surviving pair is
clean and is about to land) — a **successful** run, by design, that happens to look
identical to a spinning train under unconditional counting.

Reproduced in isolation on the e2e bed (`TestMergeTrainBisectionEjectsPoisoner`, run alone,
zero rate-limit backoff events — not throttling):

```
11:18:47 [#4650 merge-train] bisection isolated #4650 as the batch poisoner — ejecting
11:18:47 [#4648 merge-train] merged #4648 cleanly into trial branch
11:18:47 [#4646 merge-train] merged #4646 cleanly into trial branch
11:18:50 [merge-train] opened draft CI PR #4657 (2 survivors)
11:19:22 [merge-train] trial f3bb3937 green — checks: train-poison-guard (success), test (success), slow-gate (success), ci-fix-sentinel (success)
11:19:23 [merge-train] runaway guard fired: 6 trial(s) with zero successful lands within 1h0m0s — pausing 2 Queued member(s)
11:19:47 [merge-train] runaway guard already tripped (6 trial(s)) — pausing 1 Queued member(s) before dispatch
```

The guard fired **on the green trial** — the survivor validation that was about to land —
one second before it would have landed. This is also why R4 (a successful land crediting
the window) never got a chance to help: there was no land to credit, because the guard
preempted it. `FABRIK_MAX_TRAIN_TRIALS_PER_WINDOW` defaults to **20** in the engine; the
bed's `.env` sets it to `6` deliberately, to keep the e2e runaway-guard scenario's wall-clock
bounded (`tests/e2e/mergetrain_runaway_test.go`). Six trials for a 3-member batch is exactly
that bound, so any 3-member poisoned batch hits the ceiling deterministically on that bed — not
under contention, not on a slow day. A 4- or 5-member poisoned batch would exhaust even the
production default of 20 well before isolating the poisoner, since the sequence is closer to
linear (1 initial + 2×(bisect-probes) + 1 survivor-validation) than to a clean binary search.

## Decision

**Exclude by outcome, not by call-origin.** `assembleAndValidate` is split into a thin
wrapper and `assembleAndValidateInner` (the actual git/CI work, verbatim). The wrapper calls
`recordTrial(repoKey)` only when the trial's result is **not** `TrainCIGreen`:

```go
func (e *Engine) assembleAndValidate(ctx context.Context, p trialParams, members []trainMember, trialName string) ([]trainMember, TrainCIResult, int, *trainCIDiagnostic, error) {
	survivors, result, prNum, diag, err := e.assembleAndValidateInner(ctx, p, members, trialName)
	if result != TrainCIGreen {
		e.recordTrial(p.owner + "/" + p.repo)
	}
	return survivors, result, prNum, diag, err
}
```

A green result is, by construction, either the landing attempt itself or a bisection
sub-trial that just proved a sub-batch clean — never a "zero successful lands" event.
Red results, `TrainCIPending`, and assembly errors still count unconditionally — they
represent no progress, exactly as before this fix. No caller changes: Hook 1
(`runMergeTrainWorker`), `bisect`'s own `isRunawayTripped` reads, Hook 2
(`routeQueuedGroup`), `handleRedBatch`, `landOneAtATime`, and `landGreenBatch`'s rebase
revalidation all already just read `isRunawayTripped(repoKey)` after each call, which now
transparently reflects the correctly-scoped count.

### Rejected alternative: exclude by call-origin

A second mechanism was considered: thread a boolean through `assembleAndValidate` so only
the top-level loop's calls, `landOneAtATime`'s per-singleton calls, and
`landGreenBatch`'s rebase-revalidation call invoke `recordTrial` — `bisect`'s own calls
never would, since bisection exists purely to diagnose, not to land.

This is a tighter fit to the issue's R2 wording ("trials spent on bisection MUST be
excluded... the two are semantically different events" — a literal reading would exclude
a *red* bisection sub-trial too, which the by-outcome fix does not). It was rejected
because its effect on **R3's constraint** — `TestMergeTrainRunawayGuardPausesBatch` and its
unit equivalents must keep passing *unmodified* — could not be established with the same
confidence:

- For the by-outcome fix: every existing guard test uses an always-red validator (every
  trial in those scenarios is red), so the by-outcome fix counts **identically** to the
  pre-fix code by construction — not by tracing poll-cycle arithmetic. Hand-tracing
  confirmed the trip point (trial count, member set, `fireRunawayGuard`'s `count`
  argument) is byte-for-byte unchanged for every existing test.
- For the by-origin alternative: hand-tracing the all-poison e2e scenario (every subset
  always red) showed that excluding all of `bisect`'s calls means a single worker
  dispatch against a 4-member all-poison batch no longer reaches the cap on its own —
  most members return to Queued and get re-batched on a later poll, where their
  accumulation continues in the same rolling window until the cap is eventually
  reached across **multiple** poll cycles and worker dispatches, rather than one. The
  test's existing assertions would likely still resolve (its 25-minute budget probably
  absorbs the extra poll cycles), but this is a materially different code path than the
  test's own doc comment describes, and the outcome depends on ejection-ordering details
  that could only be fully confirmed by running the real e2e bed — not available in this
  environment (both regression tests are `//go:build e2e`, requiring a live GitHub bed).

Given R3 explicitly requires the existing test to keep passing **unmodified**, and this
could only be proven for the by-outcome fix without live infra, by-outcome was chosen. It
still substantially addresses R2's practical concern: under this fix, a bisected batch of
`MaxBatchSize` (default 5) accumulates only its ~⌈log₂5⌉≈3 red sub-trials toward the guard,
not all ~6-8 raw calls — R2's own text rules out raising the threshold as a fix, not
"excluding fewer than literally every bisection call."

### Scoping decisions left unchanged

- **`TrainCIPending` / assembly-failure counting is unchanged.** The issue's R1 names only
  the green survivor-validation trial; `TrainCIPending` naturally retries next poll
  regardless of whether it trips the guard on the way out. Expanding scope here would be an
  unreviewed behavior change alongside the required fix — left for a follow-up issue if it
  turns out to matter in practice.
- **The per-episode bisection cost cap (`effectiveBisectCap`/`MaxBisectValidations`) is
  untouched.** It is a different, correctly-scoped budget (bounds bisection depth within one
  red-batch episode before degrading to `landOneAtATime`), unrelated to the cross-poll-cycle
  runaway-guard window this fix addresses.
- **`fireRunawayGuard`'s alert wording gets a small accuracy correction.** It previously
  claimed to have created "%d trial branches" — no longer literally true post-fix, since more
  branches may physically exist than the counted (non-green) trials. Reworded to "%d
  trial(s)" to describe what the count now actually represents. Same file, same PR, no
  logic change.

## Is this a 0.0.78 regression? (issue A5)

**Not a regression.** `recordTrial`'s unconditional placement at the top of
`assembleAndValidate` predates the entire 0.0.78 cycle. `git log v0.0.77..HEAD --
engine/merge_train.go` shows the only commit touching `assembleAndValidate` before this fix
is `c1477cb1` (#1420, threading the combined-Validate diagnostic through bisection), which
does not touch `recordTrial`, `isRunawayTripped`, or counting in any way.

The commit most plausibly implicated by its description — `c036d500` (#1440, "short-circuit
red singleton batches before bisection") — was inspected directly. It adds the
`len(survivors) == 1` arity guard that routes a true red singleton straight to
`ejectRedSingleton` instead of through `handleRedBatch`/`bisect`. Before that change, the
same length-1 red set would have gone through `bisect`'s own base case
(`if len(red) == 1 { return &red[0], diag, false, false }`), which **also issues no
additional `assembleAndValidate` call** — it returns immediately. #1440 therefore changed
*disposition and wording only* (immediate pause + `ejectRedSingleton`'s wording, vs.
`ejectMember`'s counter-based pause + "isolated by halving bisection" wording); it added or
removed **zero** trial-counting calls.

The defect is exactly what the issue's own "Evidence it is not" argument states: the
unconditional counting had simply never been exercised by a real bisection-then-land
scenario until this run, blocked by #1527's budget exhaustion in the `on`-leg pre-release
gate. It is a long-latent bug in the original D8 design, not something introduced in
0.0.78.

## Correction to ADR-059 §D8

§D8's "Design" bullet states: *"`recordTrial(repoKey)` is called at the top of
`assembleAndValidate`... so every trial — initial batch, bisection sub-trials, and
one-at-a-time singletons — counts."* This is the as-designed contract that turned out to be
wrong — see Context above. This ADR does not edit ADR-059 in place (ADRs are historical
decision records, not as-built docs, per this repo's convention); `docs/state-machine.md`'s
"Counter:" paragraph is the as-built doc updated in the same PR to describe the corrected
behavior.

## Verification

Both acceptance tests naming this bug directly
(`TestMergeTrainBisectionEjectsPoisoner`, `TestMergeTrainRunawayGuardPausesBatch`) are
`//go:build e2e` and require a live GitHub bed not available in this environment. Verified
instead by:

- A new unit test, `TestRunawayGuard_BisectionExceedsThresholdWithoutTripping`
  (`engine/merge_train_test.go`), reproducing the reported bed's exact trial shape (3-member
  batch, single poisoner) against `MaxTrainTrialsPerWindow=5` — chosen so the raw trial count
  (6) exceeds the threshold while the guard-counted (non-green) count (3) does not.
- Non-vacuity: the counting fix was temporarily reverted and the new test re-run against the
  unfixed code, confirming it fails — the guard fires at trial 5 ("5 trial(s) with zero
  successful lands"), all 3 members get paused with a runaway-guard alert, #3 is never
  actually ejected as the isolated poisoner, and zero merges occur (full output recorded in
  the PR body).
- The full existing counter/guard unit suite
  (`TestRecordTrial_Increments`, `TestIsRunawayTripped_AtThreshold`, `TestResetTrialCounter`,
  `TestRunawayGuard_Fires`, `TestRunawayGuard_NormalBisectionNotTripped`,
  `TestMergeTrainRunawayGuard`) passes unmodified (R3/A2).

## Consequences

- A successful merge-train bisection no longer trips the runaway guard on its own
  survivor-validation trial. Any poisoned batch of 3+ members — previously guaranteed to
  stall under the bed's default threshold — now lands its clean survivors normally.
- The runaway guard still fires correctly on a genuinely spinning train: every existing
  regression test for that behavior is unmodified and passes, and the by-outcome exclusion
  is provably a no-op for an always-red validator by construction.
- `fireRunawayGuard`'s alert-comment and log wording changed from "trial branches" to
  "trial(s)" to stay accurate about what the count represents post-fix — a cosmetic,
  same-file change.
- Out of scope, unchanged: `TrainCIPending`/assembly-error counting, the per-episode bisect
  cost cap, and the by-origin alternative considered and rejected above.
