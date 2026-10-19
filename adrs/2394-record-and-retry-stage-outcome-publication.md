# ADR 2394: Record a finished stage outcome and retry its publication instead of re-running the stage

## Status

Accepted. Completes the half ADR 2393 left out ("recording a run that finished during the
outage"). Mentioned for context: #2388 (the report); this change does not close it.

## Context

`finalizeStageOutcome` made a chain of independent GitHub writes after a stage run finished —
issue-body update, stage comment, push, then the completion label in `handleStageComplete` — and
swallowed each failure into a log line (`could not add completion label`). Nothing durable said the
run had finished. GitHub and the Store both showed the stage as not complete, the in-memory
`LastAttemptAt` backoff expired, and the next poll ran Claude again on the same inputs, resuming a
session that had grown to ~13 MB. In #2388 that happened ~150 times in 21 hours, and the comment and
push were lost every time.

## Decisions

1. **A completed run is recorded before its first GitHub write.** `beginStageOutcome` writes a
   `stageOutcomeRecord` (stage, worktree head, nonce, raw output, the issue-body update and the body
   it replaces) to `.fabrik/state/stage-outcomes/<sha1(repo#n)>.json`: one file per item, mode 0600,
   atomic write, a sibling of `engine-decisions/` and `merge-train/` (the latter's loader quarantines
   every `*.json` it finds). Only a run that completed without error, without `FABRIK_NO_WORK_NEEDED`
   and without `FABRIK_BLOCKED_ON_INPUT` is recorded, and not an Implement coordinator with
   `fabrik:children-spawned` (which may turn out to be a no-work completion). Everything else keeps
   today's behaviour exactly (FR-014).
2. **Two phases.** The first write is phase `recorded`. Just before the comment is posted — after
   the PR-create marker, the Review/Validate spawn and any spec commit have run — `readyStageOutcome`
   fixes the comment payload and re-reads the head, and the phase becomes `ready`. Only a `ready`
   record is published. A record found in phase `recorded` (the process died between the first write
   and the checkpoint, a window that contains the PR-create and spawn network calls) is discarded: the
   spawn cannot be replayed safely (a Review/Validate spawn is non-resumable, ADR 1583) and a discard
   only costs the redo that happens today.
3. **While a record is live the stage is not dispatched.** `stageOutcomePending` returns false-for-work
   in `itemMayNeedWork` and `itemNeedsWorkRetryAt`, next to the `fabrik:awaiting-done` gate, and counts
   as "stage ran" for ADR 2220's routing. The gate is on the record, not on `LastAttemptAt`, which is
   what expired in #2388.
4. **Publication is retried by a per-poll settle scan.** `settleStageOutcomes` is board-sourced like
   `settleNoWorkNeededScan`, so it is independent of the admission it deliberately refuses. For each
   record, in order: remove it if the completion signal is already on the item; discard it (one
   `[outcome]` line naming the reason) if the item is closed or left the board, is paused, moved column,
   the record is not `ready`, or the worktree head cannot be read or differs from the recorded SHA (a
   missing worktree counts as a mismatch); skip the poll while a worker or comment pass is in flight or
   `forgeHeld()`; otherwise `publishStageOutcome`. The retry never calls `EnsureWorktree`, which can
   rewrite `HEAD`.
5. **Publication order and idempotency.** `finalizeStageOutcome` publishes inline exactly as before, but
   the completion signal now goes last: if the comment or the push failed, the record is kept and
   `handleStageComplete` is not called (so the label never lands ahead of the output it stands for).
   The scan then resumes at the missing step. Every step is idempotent: the comment carries a hidden
   `<!-- fabrik:outcome=<nonce>:pr|issue -->` marker and a retry reads the thread first (`findStageComment`
   matches on the prefix only, so the marker is compatible); the issue body is compared with the live body
   and a body a human edited during the outage is never overwritten; the push is `pushVerified`, which
   already skips when there is nothing to publish; `ensureDraftPR` finds an existing PR. Step flags in the
   record keep the common case free of extra reads.
6. **"Published" means the durable completion signal, not "the function returned".** `handleStageComplete`
   now returns an error: nil once the completion label (or `fabrik:awaiting-ci`, or a terminal path) is in
   place, the failing write's error otherwise. Whatever follows the signal (merge, advance) is already
   recoverable from the signal by the catch-up loop. An `attemptMergeOnValidate` failure keeps its
   historical effect (no label, Validate runs again) unless it is a transport error, which is an outage and
   is retried as a publication.
7. **The bound (R4).** Each failed retry is classified: `gh.IsTransportError` or `isTransientError` never
   counts; anything else increments `Attempts`, which is persisted so a restart cannot reset it. At
   `MaxRetries` the item is paused (`fabrik:paused` + `fabrik:awaiting-input`, one comment naming the step's
   error), the record is discarded and the comment says the stage re-runs on resume. A literal reading of R4
   ("only while the hold is engaged") would let three early transport failures exhaust the bound before the
   hold needs ≥ 5 failures over ≥ 2 minutes to engage; classifying the error is the stable reading. While
   `forgeHeld()` the scan skips records entirely.
8. **A pause discards the record.** R3, and spec assumption 3: publishing a stale outcome is worse than
   redoing the work. The escalation pauses too, so its comment says so.
9. **Comment-review audit (FR-011).** Two forms of the same loss were found.
   (a) A comment pass that completes the stage but whose `handleStageComplete` fails: the comments are
   already marked processed, so the next poll re-dispatches the whole stage — the same loss, reached via the
   comment path. `finalizeComments` now writes a `completion` record (stage and head only, no output) when
   `handleStageComplete` returns an error, and the scan retries only `handleStageComplete`. Sim twin
   `TestStageOutcome_Sim_CommentPassCompletionIsRecordedNotRerun`, with a neutralised twin proving the loss.
   (b) A failed 🚀 reaction followed by a restart: the in-memory watermark covers the live process, so the
   pass repeats only after a restart, and at most one capped pass. It is a reaction, not a stage outcome, and
   is **deliberately not fixed here**; it is a follow-up candidate.
10. **Neutralisation and tests.** `SetStageOutcomeRecordDisabledForTest` writes no record, restoring the
    pre-#2394 behaviour; `EnableStageOutcomesForTest(dir)` opts a `NewWithDeps` engine in (the store is nil
    there, so existing tests are unchanged); `tests/sim` `Env.StageOutcomeDir` is carried by `RestartEnv`.
    Sim twins: `tests/sim/stage_outcome_test.go`.

11. **A forced re-run discards the record (ADR 2378).** `fabrik:revalidate`, the merge train's auto-repair
    (`reenterValidate`) and the head-moved scan (`handleValidateSHAInvalidation`) strip
    `stage:Validate:complete` and add the durable `fabrik:validate-rerun` marker so that only a Validate
    *stage run* restores the label. A live record for Validate would replay `handleStageComplete` and
    complete Validate — clearing the marker — without the re-run. Three rules close that (found in Validate
    review): (a) `outcomeDiscardReason` discards a Validate record when `fabrik:revalidate` is on the item,
    or when `fabrik:validate-rerun` is on it and the record is not itself the re-run's
    (`stageOutcomeRecord.Rerun`, set from a live label read when the run finished — the poll-start
    snapshot can predate the marker, and the re-run's own record legitimately outlives it, since the marker
    is cleared by the very `handleStageComplete` the record retries). The `fabrik:revalidate` rule is
    load-bearing, not a backstop: a live record keeps the item out of the dispatch candidates, so
    `settleRevalidateScan` would never see the label otherwise. (b) `discardOutcomeForFreshStart` drops the
    item's record for the stage next to `markValidateRerun` in `reenterValidate` and
    `handleValidateSHAInvalidation`, so the order of the settle scan and the strip never matters (the train
    auto-repair calls `reenterValidate` directly). (c) The other fresh-start sites that clear
    `StageLastAttemptCleared` — `clearFailedStage` and `unblockAwaitingInput` — drop it too, since the stage
    is deliberately run again. Sim twins `TestStageOutcome_Sim_RevalidateReRunsInsteadOfCompletingFromTheRecord`
    and its neutralised twin (`SetOutcomeFreshStartDiscardDisabledForTest`); the explicit drops of (b)/(c) are
    covered by the unit test `TestDiscardOutcomeForFreshStart`, not by a separate sim path.
12. **A dropped record is never re-created by an in-flight attempt.** The settle scan works on a copy of the
    record and publishes without holding a lock, so a drop from another goroutine (a comment worker's
    `unblockAwaitingInput`/`clearFailedStage`, `reenterValidate`, a prune) can land mid-attempt. Only
    `createOutcome` (`beginStageOutcome`, `recordCompletionOutcome`) may insert a key; `persistOutcome` goes
    through `stageOutcomeStore.update`, which rewrites a record only while its key is still live, checked under
    the store lock. A failure note on a record that is no longer live escalates nothing. Otherwise the next
    persist would bring back a `ready` record whose head still matches, and a later scan would publish the stale
    outcome decision 11 exists to prevent. Unit test `TestPersistOutcome_DoesNotResurrectADroppedRecord`.

## Consequences

- A stage that completes while GitHub writes fail makes exactly one Claude invocation; its comment, push
  and completion label are published on a later poll, once, with no duplicates; a restart in between makes
  no difference.
- A live record suppresses all dispatch of the item at that stage, including a pending comment pass, until it
  is published or discarded. Every exit must remove it (discard rules, the deferred removal in
  `finalizeStageOutcome`, quarantine of unreadable files, removal when the item leaves the board); the
  opposite failure mode to today's — no re-run but no progress — is the one to watch, and each discard logs.
- The tiny window of decision 2 and the non-fix of 9(b) are known gaps.

## Out of scope

Capping re-dispatches in general, the forge-unreachable dispatch hold itself (ADR 2393), shrinking oversized
resumed sessions, and the reaction repeat of 9(b).
