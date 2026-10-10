# ADR 2046: Merge train holds a worker slot only around its Claude calls

## Status

Accepted. Amends the "semaphore acquired in setup" wording of
[ADR 1661](1661-merge-train-job-row-emission-timing.md) (that ADR's text is left
unedited).

## Context

The merge train shared `Engine.sem` (`max_concurrent`) with every stage worker and
held one slot for its whole lifecycle: `prepareTrainWorker` acquired it and
`runMergeTrainWorker` released it on return. That covered assembly, the entire
trial-CI wait (`pollTrainCI`, 10–25 minutes), every bisection step and landing.
On a busy board landing queued behind long Implement sessions, and a slot sat idle
for most of every trial while the train only polled GitHub (report #2037).

The train's actual Claude use is one call site: `resolveConflictWithClaude`
(`InvokeForComments`), reached from trial assembly (the re-form loop, `bisect`,
`landOneAtATime`, `landGreenBatch`'s rebase loop) and from the singleton catch-up
merge (#2044). Queued comment and review-finding handling ejects the member to the
ordinary reinvoke path, which takes its own slot.

## Decision

1. **The slot brackets the Claude invocation only.** `resolveConflictWithClaude`
   calls `acquireTrainSlot` immediately before `InvokeForComments` and releases as
   soon as it returns, before `finalizeConflictResolution`. One site covers every
   caller; rerere replay and regenerate-only branches stay slot-free.
   `prepareTrainWorker` and `runMergeTrainWorker` acquire nothing; the only cleanup
   an early return owes is `finishTrain` (ADR 067 stays the sole marker-clear
   point). Per-(repo, base) exclusivity is the `mergeTrainInFlight` marker, which
   never depended on `e.sem`.
2. **Waits are visible.** A non-blocking acquire is tried first; only a real wait
   logs `waiting for a free worker slot for conflict resolution on #N` through
   `logfRepo`, so the repo's job row shows it (the mechanism ADR 1661 relies on).
   The old start-of-train wait message is gone.
3. **Cancellation is not a verdict.** A context cancellation while waiting (or a
   shutdown killing the invocation) returns the same non-nil "not attempted" error
   a usage-limit exit does (ADR 1120) and is recognised by `trainCancelled`
   (`ctx.Err()` or a wrapped context error). It ejects no member, pauses nothing,
   is not counted by the runaway guard (`assembleAndValidate` skips `recordTrial`),
   does not start `bisect`'s one-at-a-time fallback, stops `landOneAtATime`, does
   not dissolve a batch in `landGreenBatch`'s rebase loop, and defers the catch-up
   uncharged. Usage-limit behaviour is unchanged.
4. **Suspension is re-checked after the wait**, so a long wait cannot spend a slot
   on an invocation the account-wide suspension would refuse.
5. **No `merge_train_slots` key.** After (1) the train's slot use is bounded to one
   Claude call and the wait is cancellable, and the train no longer counts against
   `max_concurrent` while polling CI. A dedicated pool would add a second knob for
   no demonstrated need; if saturation delaying conflict resolution proves to be a
   problem in practice, file it separately.

## Consequences

- Up to `max_concurrent` stage workers can run alongside a train for the whole CI
  wait. `docs/USER_GUIDE.md` documents that the train is counted against
  `max_concurrent` only during conflict-resolution calls.
- Conflict resolution on a saturated board waits for a slot, adding latency to that
  train. The wait is not bounded by the invocation's 30-minute wall-time cap
  (ADR 1500), which starts inside `runClaude`. The trial worktree and pinned base
  SHA stay live meanwhile; nothing in assembly measures time across the call.
- While waiting the train holds the in-flight marker but no slot, so stage workers
  always progress and there is no slot-for-slot deadlock.
- Out of scope: persisting trial state across polls/restarts (the engine-held
  trial-CI redesign from #2037's follow-up).
