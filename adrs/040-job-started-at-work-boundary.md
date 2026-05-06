# ADR-040: JobStartedEvent emitted at work boundary, not goroutine boundary

**Status:** Accepted  
**Issue:** #578  
**Date:** 2026-05-06

## Context

`tui.JobStartedEvent` was previously emitted by every dispatch goroutine immediately
on launch — before `processItem` had checked a single early-return guard. The matching
`tui.JobCompletedEvent` was emitted only by `InvocationObserver` when a Claude
invocation completed (`InvocationRecorded` applied to the store).

This asymmetry produced ghost "In Progress" entries in the TUI active pane: every
`processItem` early-return path (paused, blocked, stage-complete, locked-by-other,
cooldown, etc.) leaked a `JobStartedEvent` with no corresponding `JobCompletedEvent`.
The entry persisted and its timer counted up indefinitely.

Live evidence: issue #575 (blocked by #574) showed `#575 Backlog 18:15` frozen in the
active pane 19 minutes after the item was moved off the board, because the last
`JobStartedEvent` from a dep-blocked goroutine was never cleared.

Prior dispatch sites (all emitting `JobStartedEvent` at goroutine launch):
- `engine/poll.go` — main dispatch goroutine
- `engine/reviews.go` — `dispatchReviewReinvoke`
- `engine/ci.go` — `dispatchCIFixReinvoke`
- `engine/merge_gate.go` — `dispatchRebaseReinvoke`

## Decision

Move `JobStartedEvent` emission from goroutine-launch sites into the work functions
themselves (`processItem` and `processComments`), past all early-return guards:

1. **`engine/item.go`** (`processItem`): emit immediately after
   `defer store.Apply(WorkerExited{})` — the lock-acquired/WorkerExited-deferred
   boundary that marks "committed to real work." Use `workerStartedAt` (captured at
   lock acquisition) as `StartedAt`. `IsComment = false`.

2. **`engine/comments.go`** (`processComments`): emit at function entry (after the
   initial log line, before the 👀 reaction loop). Use `time.Now()` as `StartedAt`.
   `IsComment = true`. This also covers the three reinvoke dispatchers that call
   `processComments` directly.

Both sites immediately defer `JobCompletedEvent{Skipped: true}` as belt-and-suspenders
coverage for panic / context-cancel / worktree-failure paths where `InvocationObserver`
never fires. The active pane's `delete(a.active, key)` is idempotent, so the double
event on the success path (observer fires `Skipped:false`, defer fires `Skipped:true`)
is harmless.

`tui.JobCompletedEvent` gains a `Skipped bool` field. The zero value (`false`) is
correct for the authoritative success path emitted by `InvocationObserver`.

## Rationale

**Why emit in `processComments`, not in each reinvoke dispatcher?**

The three reinvoke dispatchers (`reviews.go`, `ci.go`, `merge_gate.go`) all call
`processComments` directly. Moving emission into `processComments` encodes the
semantics once, at the function that actually does the work, rather than at each
of the four callers.

**Why use `workerStartedAt` for `StartedAt` in `processItem`?**

`workerStartedAt` is captured at lock acquisition — the correct semantic for "when
this item started work." A fresh `time.Now()` at the emission line would be 50–100ms
later (after the lock-verify delay) and would not match the `LocalLockAcquired`
timestamp already recorded in the store.

**Why defer `JobCompletedEvent{Skipped: true}` at the emission site?**

`InvocationObserver` only fires when `InvocationRecorded` is applied — i.e., when
Claude returns successfully. Context cancellation, worktree setup failure, and
lock-verify loss all exit `processItem`/`processComments` without applying
`InvocationRecorded`. Without the deferred event, these paths would produce ghost
entries in the active pane (similar to the original bug, but shorter-lived).

**Why is this distinct from ADR-039's pre-dispatch gate pattern?**

ADR-039 (Fix B, issue #576) adds pre-dispatch gates to `itemNeedsWork` to prevent
goroutine launch entirely for labels like `fabrik:blocked`. Those gates prevent
`WorkerEntered`/`WorkerExited` from firing — eliminating the wake-loop. This ADR
fixes the TUI presentation layer independently: even if a goroutine launches and
early-returns (for any reason, gated or not), no `JobStartedEvent` fires unless real
work begins. The two fixes are complementary: ADR-039 eliminates the waste;
ADR-040 eliminates the ghost.

## Consequences

1. **`JobStartedEvent` must never be moved back to goroutine-launch sites.** The TUI
   "In Progress (N)" count is meaningful only if it reflects items where Claude is
   actively running. The `ActivePaneComponent` struct comment in `tui/active.go`
   documents this constraint explicitly.

2. **`processComments` callers do not emit `JobStartedEvent`.** The three reinvoke
   dispatchers (`reviews.go`, `ci.go`, `merge_gate.go`) and the three
   `return e.processComments(...)` paths inside `processItem` all delegate event
   emission to `processComments`. No call site above `processComments` should emit
   `JobStartedEvent` for comment processing.

3. **`processItem`'s three `return e.processComments(...)` paths never reach the
   `processItem` emission site.** All three occur before lock acquisition. Only
   `processComments` fires `JobStartedEvent` for those paths. There is no
   double-emission risk.

4. **Lock-then-verify ghost entries are short-lived (~50ms).** After `processItem`
   emits `JobStartedEvent`, the lock-then-verify path can still cause an early exit.
   The deferred `JobCompletedEvent{Skipped:true}` cleans up within milliseconds. A
   brief flicker in the TUI active pane is acceptable; an indefinite ghost is not.

5. **Double `JobCompletedEvent` on success path is harmless.** `InvocationObserver`
   fires `Skipped:false` on invocation completion; the deferred emit fires `Skipped:true`
   on function return. Both call `delete(a.active, key)`, which is idempotent in Go.

## References

- Issue #578 (root cause, live evidence, and consumer audit)
- ADR-038 (`038-dual-store-observer-wiring.md`) — established `InvocationObserver`
  as the authoritative `JobCompletedEvent` emitter; this ADR extends that work to
  `JobStartedEvent`
- ADR-039 (`039-cycleset-excludes-worker-lifecycle.md`) — complementary fix at the
  engine-state layer
- ADR-015 (`015-tea-execprocess-inline-subprocess.md`) — TUI event pipeline context
- `docs/state-machine.md` Appendix E — full comparison of goroutine-boundary vs.
  work-boundary lifecycle
