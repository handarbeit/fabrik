# ADR 1500: Merge-Train Conflict Resolution Gets a Wall-Time Fallback When the Holding Stage Has None

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1500 — merge-train conflict resolution has no wall-clock cap (holding stage carries no
max_wall_time)

## Context

`resolveConflictWithClaude` (`engine/merge_train.go`) invokes Claude to resolve a merge-train
trial-branch conflict, dispatching through `e.claude.InvokeForComments` with whichever stage
`holdingStage(e.cfg)` resolves to — `Queued` on this repo. That stage's YAML
(`.fabrik/stages/queued.yaml`) set no `max_turns`, `comment_max_turns`, or `max_wall_time`.

`scaledWallTime` (`engine/claude.go`, ADR-1206) returns `base` unchanged whenever `base <= 0` —
this is intentional and unrelated to this issue: ADR-1206's problem was scaling an
already-nonzero deadline under `fabrik:extend-turns`, never supplying one where none exists. But
the consequence is that `stage.MaxWallTime` unset means `base` is `0`, `scaledWallTime` stays a
no-op, and `runClaude` never wraps the invocation in `context.WithTimeout` at all. The only
remaining bound was the hardcoded 15-minute *inactivity* timeout (`claudeInactivityTimeout`),
which only fires when Claude stops producing output entirely — a Claude session that keeps
emitting output while making no real progress on the conflict could run unbounded, holding the
merge-train worker's semaphore slot and burning Claude usage indefinitely.

This gap is independent of two adjacent, already-fixed issues in the same function:
- #1206/ADR-1206's `scaledWallTime` scaling work — a scaling multiplier can never turn `0` into a
  real deadline, so that fix doesn't touch this gap.
- #1472's `mergeTrainMaxTurnsOverride` base-mismatch bug (the `fabrik:extend-turns` pre-grant
  computed off the wrong base) — a multiplier applied to a zero deadline is still zero, so this
  gap exists regardless of whether `extend-turns` is set.

## Decision

### Code-level fallback, not a config-only fix

`conflictResolutionStage(holdingStg *stages.Stage) *stages.Stage` (`engine/merge_train.go`)
substitutes a fallback `MaxWallTime` whenever the holding stage's own value is unset, and
`resolveConflictWithClaude` routes its `InvokeForComments` call through it instead of passing
`holdingStg` directly:

```go
func conflictResolutionStage(holdingStg *stages.Stage) *stages.Stage {
	if holdingStg == nil || holdingStg.MaxWallTime > 0 {
		return holdingStg
	}
	cp := *holdingStg
	cp.MaxWallTime = mergeTrainConflictWallTimeFallback
	return &cp
}
```

A config-only fix (setting `max_wall_time` on this repo's own `queued.yaml`) does not satisfy the
issue's requirement that the fix "hold for *any* configured holding stage" — `holding_stage: true`
is a general mechanism (see `CLAUDE.md`'s Stage Config Options), and another Fabrik deployment's
holding-stage YAML is out of this repo's control. The code-level fallback is unconditional on
which YAML a given deployment uses, so it satisfies that requirement universally. `queued.yaml`
was still given an explicit `max_wall_time: "30m"` anyway — cheap, non-load-bearing
self-documentation matching this repo's own convention for every other real stage (`specify.yaml`,
`research.yaml`, `plan.yaml`, `implement.yaml`, `review.yaml`, `validate.yaml`).

### Local defensive copy — never mutate the shared holding-stage pointer

`holdingStage(cfg)` (`engine/stages.go`) returns the actual `*stages.Stage` pointer stored in
`cfg.Stages`, not a copy — a single, process-wide object. Since ADR-1648, several base-partition
merge-train workers can run concurrently for the same repo, and every one of them calls
`holdingStage(e.cfg)` and gets back the identical pointer; other code (e.g. `advanceToNextStage`)
reads the same pointer too. Mutating `MaxWallTime` on it in place would be a data race across
concurrently running workers, and would leak the fallback value into every other reader of the
same object.

`conflictResolutionStage` therefore either returns `holdingStg` itself unchanged (when it already
has a `MaxWallTime`) or a freshly allocated shallow copy with the fallback substituted — it never
writes through the original pointer. A shallow copy is sufficient here: `stages.Stage`'s
slice/pointer fields (`AllowedTools`, `ExpectedReviewers`, etc.) are never touched by this
substitution.

### No change to `scaledWallTime`, `InvokeOptions`, or `engine/claude.go`

The fallback value is threaded entirely through the existing `stage.MaxWallTime` field on the
`*stages.Stage` already passed to `InvokeForComments` — `scaledWallTime`'s existing
`fabrik:extend-turns` proportional scaling (`mergeTrainMaxTurnsOverride`) applies to the fallback
exactly as it would to an explicit YAML value, with zero extra wiring. This keeps the fix fully
contained in `engine/merge_train.go`, matching the issue's stated scope.

### 30-minute fallback default

`mergeTrainConflictWallTimeFallback` (a package-level `var`, not a `const` — mirroring
`claudeWaitDelay`/`claudeKillGraceSigInt`/`claudeKillGraceSigTerm`'s established pattern of a
test-overridable timing knob) defaults to 30 minutes, matching every other real stage's
`max_wall_time` in this repo's own config. No in-repo telemetry exists on historical
conflict-resolution durations to size this more precisely; this is the best available anchor, and
the trade-off (too short ejects a legitimately-slow-but-progressing resolution; too long doesn't
meaningfully bound the runaway risk) is an accepted judgment call.

## Consequences

- A merge-train conflict-resolution invocation is now always subject to a nonzero wall-clock
  deadline, regardless of whether the configured holding stage sets `max_wall_time` — closing the
  gap this issue exists to close.
- A holding stage that *does* set `max_wall_time` explicitly is unaffected — `conflictResolutionStage`
  returns it unchanged, including under `fabrik:extend-turns` scaling exactly as before.
- The `fabrik:paused`-when-still-unresolved-after-fallback-kill behavior is identical to the
  existing wall-time-cap mechanism used elsewhere: output collected before the kill is still
  scanned for `FABRIK_STAGE_COMPLETE` by the shared post-kill path in `runClaude`.
- A future contributor investigating why a merge-train Claude invocation was killed despite its
  holding stage's YAML showing no `max_wall_time` will not discover the fallback without reading
  `resolveConflictWithClaude`/`conflictResolutionStage` directly, or this ADR. The
  no-in-place-mutation invariant recorded here is a direct consequence of ADR-1648's concurrent
  per-partition workers and needs to be preserved by any future change to this function.
- See `docs/state-machine.md` §7.7 and `CLAUDE.md`'s `max_wall_time` documentation for the
  user-visible caveat this introduces to previously-unconditional "absent or zero = no cap"
  language.
