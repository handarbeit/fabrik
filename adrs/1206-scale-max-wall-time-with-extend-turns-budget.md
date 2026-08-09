# ADR 1206: Scale max_wall_time with the extend-turns turn budget

**Status:** Accepted
**Date:** 2026-08-09
**Issue:** [#1206](https://github.com/handarbeit/fabrik/issues/1206)

## Context

`fabrik:extend-turns` is the operator's escape hatch for a job whose slices keep hitting
the turn cap — used three times on 2026-07-27 (#816, #1114, #1183) to unstick work that
was progressing normally (see ADR-1199, which later gave this same signal its own,
independently-bounded slice counter). Its mechanism, established by ADR-030, pre-grants
the **first** invocation of a stage or comment-review run a 2x turn budget in a single
call:

```go
// engine/item.go, runInvocationWithExtension
firstBudget := stage.MaxTurns
totalMultiple = 1
if hadExtendTurnsLabel && stage.MaxTurns > 0 {
    firstBudget = 2 * stage.MaxTurns   // 200 turns, ONE invocation
    totalMultiple = 2
}
```

`engine/comments.go`'s `runCommentExtensionLoop` does the identical thing for comment
processing (`firstBudget = 2 * base`).

But `max_wall_time` — the hard per-invocation kill (`engine/claude.go`'s `runClaude`,
`context.WithTimeout` + `cmd.Cancel`'s `DeadlineExceeded` branch) — was not scaled to
match. That 200-turn invocation ran under the same `stage.MaxWallTime` as a normal
100-turn run. Measured on this repo, 100 turns takes 10–20 minutes, so 200 plausibly
takes 20–40 — a wall clock sized for the normal case cuts off a legitimately-progressing
extended run, and it fails in the least obvious way: the job looks like it failed again,
for a different reason, rather than looking like a timeout tuning problem.

This was worked around, not fixed, in PR #1201 by setting `max_wall_time: "60m"` on every
stage in this repo's own `.fabrik/stages/*.yaml` — sized for the rare 2x case rather than
the common 1x case, weakening the runaway bound for every ordinary invocation to
accommodate the rare extended one.

The progress-based extension loop (turn budget beyond the first pre-grant, up to the 3x
hard cap from ADR-030) is unaffected: each subsequent extension iteration resets
`currentBudget` back to `stage.MaxTurns` (1x, not cumulative) and re-invokes with
`--resume`, so it always gets a **fresh** `runClaude` call and thus its own correctly-sized
wall-clock window starting at that call's own spawn time. Only the label-gated
first-invocation pre-grant packs a multiplied turn budget into a single `runClaude` call,
and only that invocation runs the mismatched wall clock.

`engine/merge_train.go`'s `resolveConflictWithClaude` has the textually identical bug —
it independently computes `maxTurnsOverride := holdingStg.MaxTurns * 2` when any batch
member carries `fabrik:extend-turns`, and calls the same `InvokeForComments` /
`runClaude` machinery with an unscaled `stage.MaxWallTime`. It is out of this issue's
explicit scope (not a target of its test coverage or verification), but shares the same
plumbing.

## Decision

**Option A: scale the deadline with the budget, derived centrally inside
`InvokeClaude`/`InvokeClaudeForComments` rather than passed in by each caller.**

A new pure helper, `scaledWallTime` (`engine/claude.go`):

```go
func scaledWallTime(base time.Duration, effectiveBudget, baseBudget int) time.Duration {
    if base <= 0 || baseBudget <= 0 || effectiveBudget <= baseBudget {
        return base
    }
    return base * time.Duration(effectiveBudget) / time.Duration(baseBudget)
}
```

`InvokeClaude` and `InvokeClaudeForComments` each already compute an effective per-call
turn budget from `opts.MaxTurnsOverride` relative to a natural baseline — `effectiveBudget`
vs. `stage.MaxTurns` in `InvokeClaude`, `limit` vs. `commentMaxTurns(stage)` in
`InvokeClaudeForComments`. That pair is exactly the ratio the wall clock needs to scale
by: it evaluates to `1` (no scaling) for every ordinary invocation and every
progress-based extension iteration — both of which pass an unmultiplied budget — and to
`2` only for the single label-gated first-invocation pre-grant, where
`opts.MaxTurnsOverride` is `2 * stage.MaxTurns` (or `2 * commentMaxTurns(stage)`). No new
field, no new state threaded through `runInvocationWithExtension` or
`runCommentExtensionLoop` — the multiplier is already implicit in values those functions
already pass down via `opts.MaxTurnsOverride`. Both invocation functions call
`scaledWallTime` immediately before their existing `runClaude(...)` call and pass the
result in place of the raw `stage.MaxWallTime`:

```go
// InvokeClaude (engine/claude.go)
wallTime := scaledWallTime(stage.MaxWallTime, effectiveBudget, stage.MaxTurns)
output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number,
    stage.Name, sessFilePath, ld, extraEnv, wallTime, effectiveBudget, ...)

// InvokeClaudeForComments (engine/claude.go)
wallTime := scaledWallTime(stage.MaxWallTime, limit, commentMaxTurns(stage))
output, completed, usage, err := runClaude(ctx, args, prompt, workDir, issue.Number,
    stage.Name+"-comment-review", sessFilePath, ld, extraEnv, wallTime, limit, ...)
```

The three guard conditions in `scaledWallTime` are each load-bearing:

- `base <= 0` — `max_wall_time` is unset (no cap); nothing to scale.
- `baseBudget <= 0` — `stage.MaxTurns == 0` means "unlimited turns." In that case
  `runInvocationWithExtension` never sets `MaxTurnsOverride` from the label (guarded by
  `stage.MaxTurns > 0` in ADR-030's original logic), so `effectiveBudget` also resolves
  to `0`. Dividing by zero must not happen; returning `base` unscaled is correct here
  since no budget was ever inflated.
- `effectiveBudget <= baseBudget` — no extension is in effect for this particular call
  (ordinary invocation, or a progress-based extension iteration that already reset to the
  base budget). Returning `base` unscaled preserves the tight bound for the common case.

The scale factor is the exact rational multiple already governing the turn budget — not a
second, independently-tunable knob. This keeps the "proportionate, not unlimited"
guarantee mechanically tied to the turn multiplier: it cannot drift out of sync with it,
because it is derived from the same numbers on every call.

## Why not Option B

Option B (split the pre-grant across two `--resume` invocations, so the label-gated path
behaves exactly like the progress-based path) would remove the special case entirely,
leaving one extension mechanism instead of two with different wall-clock semantics. It
was rejected for this issue because it is materially more invasive: it changes *when* the
first-tranche completion/progress check happens relative to today's single-call behavior,
and requires reasoning through the no-signal-stage interaction (Research, Specify, Plan,
and Done have no progress signal per `docs/state-machine.md`'s stall-detection section —
under Option B those stages would need to unconditionally proceed to the second tranche
without a progress check, mirroring today's "no progress check needed for the first
turn-limit hit" pre-grant semantics, to avoid silently *reducing* available budget for
label-gated no-signal stages). Option A is strictly additive: it changes only the
wall-clock deadline computation at two call sites, with zero change to turn-budget or
resume semantics, and needs no new reasoning about stage-type interactions.

## `engine/merge_train.go` coverage

Because the scale factor is derived *inside* `InvokeClaudeForComments` from values it
already computes (`limit`/`commentMaxTurns(stage)`), rather than being computed and
opted-into by each caller, `resolveConflictWithClaude`'s identical `maxTurnsOverride`
pre-grant funnels through the same `InvokeOptions.MaxTurnsOverride` →
`InvokeClaudeForComments` → `runClaude` path with no changes needed in `merge_train.go`
itself to get scaling applied at all. Had the multiplier instead been a field each caller
was responsible for setting, this site would have needed its own explicit follow-up.

However, `handarbeit-pruefer[bot]` (reviewing PR #1467) found that the *multiplier* it
initially inherited was wrong, not just unverified: `resolveConflictWithClaude` computed
its pre-grant as `holdingStg.MaxTurns * 2` — scaled off `stage.MaxTurns`, not
`commentMaxTurns(holdingStg)`, the base `scaledWallTime` actually divides by inside
`InvokeClaudeForComments` (`limit` vs. `commentMaxTurns(stage)`). Whenever a stage's
`comment_max_turns` differs from its `max_turns`, the two bases disagreed and the
multiplier came out to 4x (`30m` → `120m`) instead of the intended 2x. This mismatch was
real and worth fixing regardless of whether any repo's config currently trips it — the
numerator and denominator were genuinely different bases — so it was folded into this
issue rather than left solely to the follow-up: `resolveConflictWithClaude` now computes
its pre-grant via `mergeTrainMaxTurnsOverride(holdingStg, extendTurns)`
(`engine/merge_train.go`), which bases it on `commentMaxTurns(holdingStg) * 2` — the same
base `scaledWallTime` divides by — with direct unit coverage
(`TestMergeTrainMaxTurnsOverride_UsesCommentMaxTurnsBase`, `engine/merge_train_test.go`)
and real-subprocess wall-time coverage against a non-degenerate holding-stage fixture
(`TestInvokeClaudeForComments_MergeTrainOverrideScalesWallTime`,
`engine/grandchild_test.go`, added by [#1472](https://github.com/handarbeit/fabrik/issues/1472)).

**Important correction, made after this ADR first landed:** the *bug* described above was
never live on this repo, and the "`max_turns: 100`/`comment_max_turns: 50`, true of every
stage YAML in this repo" framing this section originally used was wrong in a way that
mattered. `resolveConflictWithClaude` reads the *holding stage* passed in by
`assembleTrialBranch` (`e.holdingStg`), not a pipeline stage — on this repo that is
`Queued`, and `.fabrik/stages/queued.yaml` is three lines: `name`, `order`,
`holding_stage: true`. It sets none of `max_turns`, `comment_max_turns`, or
`max_wall_time`. With `holdingStg.MaxTurns == 0`, the pre-fix guard
(`holdingStg.MaxTurns > 0`) was false, so no turn-budget pre-grant was ever applied on
this path; and with `stage.MaxWallTime == 0`, `scaledWallTime`'s `base <= 0` guard
short-circuited before any scaling happened. So the ratio was never actually 4x here:
this code path was inert both before and after this issue, and merge-train conflict
resolution ran uncapped (not at a live 4x deadline) the whole time — nothing regressed
in production as a result of the original mismatch. The mismatch was still real and
worth fixing (any holding-stage config that *does* set these three fields, with the two
turn fields differing, would trip it), which is why the fix above was kept; it just never
fired against this repo's own config. See #1472's discussion thread for the fuller
account of this correction.

## Consequences

- An extend-turns first-invocation pre-grant now gets a wall-clock deadline scaled by the
  same factor as its turn-budget multiple (2x turns → 2x wall time), instead of running
  under a deadline sized for the un-extended case.
- Ordinary invocations, and every progress-based extension iteration, are unaffected —
  they already pass an unmultiplied budget, so `scaledWallTime` is a no-op for them.
- This repo's own `.fabrik/stages/*.yaml` `max_wall_time` values, bumped to `"60m"` by PR
  #1201 specifically to survive the un-scaled 2x case, are restored to `"30m"` — the
  pre-#1201 1x-sized value — now that the 2x case scales independently. See
  `git log` for `d8f784d8` (the `60m` bump) and `bdfe56d0` (the original `30m` + 100-turn
  budget).
- `engine/merge_train.go`'s conflict-resolution pre-grant now scales correctly too: its
  turn-budget override is sized off the same `commentMaxTurns(holdingStg)` base
  `scaledWallTime` divides by, via the new `mergeTrainMaxTurnsOverride` helper — see above.
- A future caller adding a third site that sets `opts.MaxTurnsOverride` inherits correct
  wall-time scaling automatically, as long as it routes through `InvokeClaude` or
  `InvokeClaudeForComments` — it does not need to compute or thread a multiplier itself.

## References

- [ADR-030: Progress-Based Turn Extension](030-progress-based-turn-extension.md) —
  established the label pre-grant and the progress-based extension loop this ADR is
  additive to.
- [ADR-1199: Separate the slice-budget counter from the failure-retry counter](1199-slice-budget-separate-from-failure-counter.md) —
  the same three 2026-07-27 incidents (#816, #1114, #1183) that motivated
  `fabrik:extend-turns`'s real-world use also motivated this issue.
- `docs/state-machine.md` — updated in this issue's PR to describe the scaling behavior
  for both the stage and comment-processing paths.
- PR #1201 — the `60m` workaround this issue's config change reverts.
- [#1472](https://github.com/handarbeit/fabrik/issues/1472) — originally filed to fix
  `engine/merge_train.go`'s turn-budget/wall-time scaling multiplier; the correctness fix
  landed in this issue instead (see above). #1472 added the real-subprocess wall-time
  coverage for this path (`TestInvokeClaudeForComments_MergeTrainOverrideScalesWallTime`,
  `engine/grandchild_test.go`) and corrected this section's original "weakened the
  runaway bound in production" framing to the latent/inert-on-this-repo account above.
  Found by `handarbeit-pruefer[bot]` reviewing PR #1467.
