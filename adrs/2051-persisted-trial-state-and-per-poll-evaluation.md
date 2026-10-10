# ADR 2051: Merge-train runs are persisted state, evaluated per poll

## Status

Accepted. Supersedes the worker model of [ADR 059](059-internal-merge-train.md) D3/D5
(a goroutine that runs a whole episode: assemble, block on CI, bisect, land) and the
"recursive `bisect`" form of D4 (its halving order and cost cap are unchanged — the
recursion becomes data). Amends, without editing their texts:

- [ADR 067](067-merge-train-centralized-inflight-cleanup.md) — the in-flight *claim*
  now lives as long as the **run**, not as long as a goroutine;
- [ADR 1222](1222-consolidate-merge-train-worker-liveness.md) — the Store liveness marker now
  means "a step goroutine is executing", not "an episode exists";
- [ADR 1208](1208-queued-review-finding-ejection.md) — unchanged *because* the claim
  persists (see Decision 5);
- [ADR 1835](1835-merge-train-trial-prefix-reuse.md) — the prefix cache belongs to the
  run;
- [ADR 2044](2044-singleton-catch-up.md) — the catch-up's in-worker CI wait stays;
- [ADR 2046](2046-merge-train-slot-only-around-claude-calls.md) — extended: a waiting
  trial now holds neither a slot nor a goroutine;
- [ADR 2050](2050-train-phase-hook-and-episode-outcomes.md) — the episode spans the
  run, and the phase has a durable source.

## Context

A merge-train worker was one goroutine for a whole episode. That meant (1) a daemon
restart could not resume a trial's CI wait or any bisection progress —
`reconstructTrainState` only finishes a merged landing, resumes a *single* open trial
(dissolving on anything but green) and sweeps remnants; (2) a wedged trial was visible
only as a goroutine that never returns; (3) the train phase was readable only from
inside that goroutine. Stage CI waits (`fabrik:awaiting-ci`) already work the other
way: the worker finishes and later polls evaluate. Report: #2037.

## Decision

1. **Trial state is a persisted record, one per (repo, base) partition.** A JSON file
   `.fabrik/state/merge-train/<hash(trainKey)>.json`, written atomically (temp file +
   rename, the `workers.json` pattern), schema `version` 1. It holds the pinned base
   SHA, the trial-name sequence, the step, the members in play, the one open trial
   (name, head SHA, draft CI PR, members, opened-at, absolute deadline, and the #2052
   startup-watch / failed-job re-run memory), the bisection state, the one-at-a-time
   cursor and a write-ahead poisoner mark. A file that does not parse or has another
   version is renamed `.corrupt` and treated as absent.

   *Rejected: a marker comment on the trial PR* — bisection position changes at every
   transition (a comment edit and a read per poll each), and sub-trial PRs are closed
   and recreated. *Rejected: deriving everything from the trial PR body and branch
   name* — bisection position, spent budget, pinned base and the one-at-a-time cursor
   are not recoverable from a trial PR. *Cost accepted:* the file is local to one
   machine; a moved or lost file degrades to today's reconstruction.

2. **GitHub validates, the file never overrules it.** A record is adopted only after
   the board snapshot and GitHub agree with it (trial PR open at the recorded head SHA;
   every live member still Queued, open, unpaused, same PR head, live status not moved
   — ADR-1871; pinned base still in the clone). Otherwise it is discarded and the
   partition forms fresh through the unchanged `reconstructTrainState` Routes 1–3. A
   merged trial PR is deliberately left to Route 1. A base that moved is *not* a
   failure: `landGreenBatch`'s main-moved cycle handles it at landing.

3. **Bisection is data.** The old `bisect`'s only recursive call was a tail call, so
   its state is `{Origin, Red, NextHalf, Used, Cap, RedDiag}` (`bisectState`, pure).
   Halving order (bors-ng), the cap check *before* each half, `Used` spent before the
   cancel/error/runaway/infra checks, recursion into the *survivors* of a red half, and
   "pending is not red" are reproduced exactly and pinned by a randomized property test
   against a verbatim copy of the recursion and by a parity oracle on the real worker.
   The cap is read once at episode start and persisted, so it holds across a restart.

4. **One state machine, two drivers.** `advance(run, verdict)` holds everything that
   used to sit between the blocking CI waits; it opens a trial, persists, and returns
   *awaiting*. The **synchronous driver** (no run store — every `NewWithDeps` engine)
   blocks for each verdict (`trainValidateFn` / `pollTrainCI`), so the pre-existing
   merge-train tests run unmodified against the state machine. The **asynchronous
   driver** (`New()`) lets the worker goroutine exit once the trial is open; the
   per-poll scan `settleTrainRuns` evaluates it. `runMergeTrainWorker`'s signature is
   unchanged; `landOneAtATime` remains as a detached synchronous wrapper.

5. **Ownership: the claim persists, liveness means "a step is running".** The
   `mergeTrainInFlight` claim (and its batch numbers) lives for the run, so the
   dispatch guard, ADR-1208's direct-eject-vs-pending-eject routing and
   `invalidateConflictingQueued`'s `!inFlight` gate need no change — an open trial's
   members cannot be ejected out from under it. Pending-eject signals are consumed at
   the verdict step, the same checkpoint as before. The Store marker is set when a step
   goroutine is launched and cleared by `leaveStep`; an awaiting-CI trial therefore does
   not block the auto-upgrade idle guard (a restart resumes it) while a Claude step or
   a landing does. `mergeTrainWorkerActiveForRepo` also reports an open run. A
   per-run `stepping` compare-and-swap guards FR-011: a poll and a step, or two polls,
   never advance one run twice.

6. **Per-poll evaluation shares the decision code.** `pollTrainCI`'s loop body became
   `evalTrialCI(ctx, …, *trialCIState) (result, diag, decided)`; `pollTrainCI` is a
   sleep loop around it and the scan calls it with the memory loaded from, and saved
   to, the record. CI classification, `ciSuiteHold`, the #2052 retrigger and the single
   failed-job re-run are therefore identical. No slot is taken.

7. **Deadline.** `Deadline = e.now() + ciBackstopTimeout()` is persisted at open and
   compared with `e.now()` on each poll; the trial's CI is read first, and expiry with no
   verdict synthesises `TrainCIPending` through the existing pending handling. Trial PRs have no `LastCIProgressAt` (their check runs are
   never attributed to a board item) and inventing a liveness signal is out of scope.

8. **Scope of "per poll".** Only *trial* CI waits (main, bisection half, one-at-a-time
   singleton). Waits inside a landing path (`landGreenBatch`'s rebase revalidation,
   `pollForMergeable`, the ADR-2044 catch-up `waitMemberCI`) stay in a short step
   goroutine: landing paths are out of scope and the catch-up needs the pinned base.
   A restart during `landing` (set before each green main or one-at-a-time landing) drops the record; the durable Route 1/2 reconstruction
   finishes the landing idempotently.

9. **Write-ahead.** `advance` persists the post-verdict state before the side effect it
   licenses; a verdict is recomputable from GitHub, so a crash between verdict and
   persist loses nothing. A pending ejection persists `Poisoner = N` before
   `ejectMember`; on adoption a poisoner already off Queued is dropped, one still Queued
   is ejected once.

10. **Phase source.** `Engine.TrainRunPhases()` reads phase, position, phase start and
    open-trial PR from the record. Transition sites keep calling the single
    `noteTrainPhase` hook; `syncRunPhase` feeds it from the record on adoption
    (`noteTrainPhaseAt` restores the phase start).

11. **Stays in memory** (reset on restart, unchanged): runaway-guard timestamps,
    ejection counts, auto-repair and catch-up caps, the overlap cache, policy and
    infra-cooldown memos. The bisection spend, the cap and the trial sequence *are*
    persisted. After a restart the TUI episode's outcome facts cover the post-restart
    part of the run.

## Consequences

- A restart mid-trial resumes the same trial PR and lands it with no second trial; a
  restart mid-bisection opens the same next half. A trial with no CI progress is
  surfaced within one poll of its deadline. Covered by unit tests (restart at every
  step of four scenarios, race-clean) and the sim scenarios of
  `tests/sim/mergetrain_resume_test.go`.
- An episode now takes more polls than before (one verdict per poll). The sim scenarios
  that predate this ADR and assert an episode "within one poll" keep the synchronous
  driver (`mergeTrainEnvOptions.AsyncTrain` is opt-in); both drivers execute the same
  state machine. The first live release-gate run on the asynchronous driver should be
  watched.
- The state file is machine-local; losing it costs only the rebuild that was the
  behaviour before.
- No new configuration key, flag, environment variable or label.
