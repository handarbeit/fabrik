# ADR 1199: Separate the slice-budget counter from the failure-retry counter

**Status:** Accepted
**Date:** 2026-08-08
**Issue:** [#1199](https://github.com/handarbeit/fabrik/issues/1199)

## Context

`finalizeStageOutcome` (`engine/item.go`) is the single funnel every Claude invocation's
outcome passes through. Before this change, its final `else` branch — reached whenever a
stage did not complete and was not blocked-on-input — unconditionally incremented one
counter, `Attempts`, and paused the issue with `stage:<name>:failed` once `MaxRetries` was
exceeded:

```go
if claudeRan && e.cfg.MaxRetries > 0 {
    e.store.Apply(itemstate.StageRetryIncremented{...})
}
```

Three structurally distinct causes reached this branch:

1. **Turn-cap preemption** — the CLI's own `subtype: "error_max_turns"` /
   `terminal_reason: "max_turns"` (captured since #1178, surfaced as
   `*claudeTurnLimitError`, `turnLimited` in code). The session ran out of turns and will
   resume via `--resume` on the next dispatch; the worktree persists and work continues
   from where it stopped.
2. **Genuine error exit** — something broke.
3. **Clean run, no `FABRIK_STAGE_COMPLETE`** — Claude finished without signalling
   completion.

All three counted identically against `MaxRetries` (default 3), and `--max-retries` is
documented as bounding failed *attempts* — "Max failed stage attempts before pausing the
issue" (`docs/USER_GUIDE.md`). A turn-cap preemption is not an attempt that failed; it is
a time-slice of work still in progress. Conflating the two meant a job legitimately
needing more than 3 slices was paused with `stage:<X>:failed` and a "failed to complete
after 3 attempt(s)" comment while progressing normally — observed on the same day across
three issues (#816: 3 slices before pausing, later completed at 41/100 turns; #1114: 5
slices; #1183: 3 slices, with invocation cost visibly rising slice-over-slice — exactly
what accumulating progress looks like). Each pause required a human to notice and
manually unpause. `fabrik:extend-turns` "fixed" this only by making slices large enough
to finish inside the retry budget — treating the symptom while re-introducing the
unbounded-work risk the turn cap exists to prevent. Reported independently by @bdueck in
#1191 as the undocumented `max_retries × max_turns` ceiling.

The fix is not simply "stop counting preemptions" — that would remove the only bound on
slicing and let a non-converging job resume forever. The counter needs to be split:
one bound for "this keeps genuinely breaking, stop" and a separate, wider bound for "this
job is too big for its slice budget, stop."

**The routing decision for cause 3** (clean run, no marker) was explicitly deferred by
the issue to this design stage: it is not obviously a failure or a slice. `turnLimited` —
the CLI's own structural signal — is the only *verified* classification available at this
call site; a clean run with no marker has no equivalent structural signal distinguishing
it from "stopped for an unknown reason." Treating an unverified case as a slice would
mean inferring rather than detecting structurally, which the issue explicitly rules out
("distinguish structurally, not by inference"). Cause 3 therefore stays on the failure
counter, unchanged from today.

## Decision

Split the single counter into two, both living on `itemstate.StageState`:

| Counter | Field | Config | Default | Counts |
|---------|-------|--------|---------|--------|
| Failure counter (unchanged) | `Attempts` | `MaxRetries` / `--max-retries` / `FABRIK_MAX_RETRIES` | 3 (0 = unlimited) | Genuine errors, degenerate output, PR-creation failures, clean run with no completion marker |
| Slice counter (new) | `SliceRetries` | `MaxSliceRetries` / `--max-slice-retries` / `FABRIK_MAX_SLICE_RETRIES` | 10 | Turn-cap preemptions only (`turnLimited == true`) |

**Routing** is a single branch on the already-computed `turnLimited` boolean, inside the
existing `claudeRan` guard in `finalizeStageOutcome`'s `else` branch — no new detection
work, no change to the surrounding WIP-commit/push/comment-marking flow (ADR-1178
requires that machinery keep running unconditionally for a turn-cap exit; unlike the
usage-limit exemption below, a turn-capped invocation *did* run and made progress, so it
cannot early-return the way `handleUsageLimitExit` does):

```go
if claudeRan {
    if turnLimited {
        // increment SliceRetries; escalate via pauseForSliceLimit at MaxSliceRetries
    } else if e.cfg.MaxRetries > 0 {
        // unchanged: increment Attempts; escalate via escalateFailedStage at MaxRetries
    }
}
```

**Escalation** on exceeding `MaxSliceRetries` is modeled directly on the merge-gate's
`pauseForRebaseCycleLimit` (`engine/merge_gate.go`) — a live, tested, in-production
precedent for exactly this shape: a second, independently-bounded counter with its own
non-failure message. `pauseForSliceLimit` posts a comment stating explicitly that this is
not a failure, names `--max-slice-retries`/`FABRIK_MAX_SLICE_RETRIES` as the override, and
suggests `fabrik:extend-turns` (wider per-invocation turn budget → fewer slices needed) or
splitting the issue. It applies `fabrik:paused` + `fabrik:awaiting-input` — **never**
`stage:<name>:failed`, since the stage has not failed.

**`SliceRetries` is bound higher than `Attempts` by default (10 vs. 3)** and is
independently configurable (flag + env, mirroring `MaxRebaseCycles`'s wiring tier — no
`config.yaml` field, the established tier for a "second, narrower" counter in this
codebase) rather than a fixed multiplier of `MaxRetries`. A multiplier was rejected: it
would silently change if `MaxRetries` is retuned, re-creating the exact "accidental
ceiling" problem #1191 reported. 10 is large enough that all three cited real-world cases
(3, 5, and 3 slices respectively) would have completed without pausing, while still
bounding a genuinely non-converging job — the issue's explicit requirement that the bound
on consecutive slices must still exist.

**`SliceRetries` resets exactly when `Attempts` resets** — folded into the existing
`StageRetryCleared` mutation (fired on normal stage completion and by `clearFailedStage`
on manual unpause) rather than a new mutation type. Both counters represent "how many
attempts/slices this run-to-completion took" and share the same natural reset point.

## Consequences

**A job needing more slices than `max_retries` now completes without human
intervention**, bounded instead by `MaxSliceRetries` — the issue's headline acceptance
criterion. `Attempts` is untouched by any number of turn-cap preemptions.

**`pauseForSliceLimit` does not reset `SliceRetries` on pause**, mirroring
`pauseForRebaseCycleLimit` exactly (which likewise does not apply `itemstate.EnginePaused`
or a failure-style label). A manual `fabrik:paused` + `fabrik:awaiting-input` removal
alone does not reset the counter — it is already at the limit, so the very next turn-cap
exit re-escalates immediately unless the underlying condition changes (wider turn budget
via `fabrik:extend-turns`, or splitting the issue). This is a deliberate, precedented gap:
the identical shape exists today for `RebaseCycles`/`MaxRebaseCycles`, and the pause
comment's suggested remedies are the intended path to actual progress rather than a bare
unpause.

**`--max-retries` now means exactly what it is documented to mean** — failed attempts —
closing the gap #1191 reported between the documented and actual behavior of
`max_retries`. The `max_retries × max_turns` ceiling that #1191 named is now either
irrelevant (a large job no longer needs to fit inside it) or, if the operator still hits
`MaxSliceRetries`, explicit and documented rather than an accidental product of two
unrelated numbers.

**`stage:<name>:failed`'s meaning narrows** to "failure budget exhausted," now that a
second, non-failure budget exists with its own escalation shape. `LABELS.md`'s entry for
the label is updated accordingly.

**No change to `#1146`'s stall detection** (declining-turns heuristic, `detectAndArmStallHint`).
It shares the same call site and `clean`/`turnLimited` inputs as the new routing logic but
is a materially different concern — bounding non-converging *retry strategy*, not slice
*count* — and remains independent of both `MaxRetries` and `MaxSliceRetries`.

**Degenerate-output detection stays scoped to the failure-counter branch only.** A
turn-limited exit could theoretically also produce degenerate output, but this is a
vanishingly rare edge case not raised by the issue or its cited incidents, and adding a
parallel check to the slice branch would be speculative engineering against an untriggered
scenario.

## Alternatives Considered

**A fixed multiplier of `MaxRetries`** (e.g. `3 × MaxRetries`, no new flag). Rejected: it
reintroduces exactly the "accidental ceiling" shape #1191 reported — the slice bound would
silently change whenever an operator retunes the unrelated failure bound, rather than
being an explicit, independently-documented number.

**Cause 3 (clean run, no marker) routed to the slice counter.** Considered because it is
"closer to a slice" in spirit — the stage ran fine and simply is not finished. Rejected:
`turnLimited` is the only structurally verified signal at this call site; there is no
equivalent structural marker for "clean run, no marker, but definitely not a failure."
Routing it to the slice counter would be an inference, which the issue's own requirement
("distinguish structurally, not by inference") rules out. It stays on the failure counter,
unchanged from before this issue.

**Reusing `escalateFailedStage`'s shape but skipping only `addFailedLabel`.** Considered
as a smaller diff. Rejected in favor of mirroring `pauseForRebaseCycleLimit` end to end:
the rebase-cycle path is a proven, tested precedent for the entire "second bounded
counter, non-failure message, `awaiting-input` instead of `failed`" shape, not just the
label choice — reusing it end to end means no new pause plumbing was invented for this
issue.

**A dedicated `SliceRetryCleared` mutation, independent of `StageRetryCleared`.**
Considered so the two counters could diverge if a future need arose. Rejected: no case
was found where "slices completed for this run" and "failures for this run" should reset
at different times — both represent the same run-to-completion, and adding a second
mutation type for a divergence that has no current use case is unnecessary surface area.

## References

- [ADR-1178: Turn-limit classification](1178-turn-limit-classification.md) — established
  `claudeTurnLimitError`/`turnLimited` and explicitly scoped retry/escalation semantics as
  out of this ADR's change — the follow-up this issue completes.
- [ADR-1119: Claude usage-limit detection](1119-claude-usage-limit-detection.md) and
  ADR-1120/1183 — precedent for excluding a structurally-detected cause from
  `StageRetryIncremented` (`handleUsageLimitExit`). The precedent reused here is the
  *counter-exclusion* pattern, not the *early-return* control flow: unlike a usage-limit
  exit (stage never ran, zero cost), a turn-capped invocation did run and made progress,
  so `StageAttempted`/`commitWIP`/the branch push must still run exactly as before.
- `engine/merge_gate.go`'s `pauseForRebaseCycleLimit` — the structural model for
  `pauseForSliceLimit`; a second, independently-bounded, non-failure-labeled pause path,
  live in production for `RebaseCycles`/`MaxRebaseCycles`.
- #816, #1114, #1183 — three same-day production instances of the original defect.
- #1191 (@bdueck) — independent operator-facing report of the same undocumented ceiling.
- #1146 — the related, explicitly out-of-scope declining-turns stall heuristic.
- #1178, #1200, #1201 — sibling issues in the same turn-cap-classification cluster.
- `docs/state-machine.md` §7.12 — as-built description of the shipped two-counter
  behavior.
