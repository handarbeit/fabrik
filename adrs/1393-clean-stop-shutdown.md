# ADR 1393: Clean-Stop Shutdown — Durable Pause, Bounded Drain, WIP Preservation

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1393 — clean-stop shutdown: pause in-flight issues durably and bound the worker drain

## Context

Before this change, stopping the daemon was a kill, not a pause. `Engine.Run()`'s SIGINT/SIGTERM
handler (`engine/poll.go`) cancelled the root context and called `cleanupLockedIssues()` — which
removes `fabrik:locked:<user>` and nothing else — then waited on `e.wg.Wait()` unbounded, with the
only escape hatch being a second Ctrl-C that force-exits via `os.Exit(1)` with no cleanup at all.
This left three gaps:

1. **No durable record that we stopped.** An issue mid-Implement when the daemon stopped looked, on
   the board, identical to one mid-Implement right now — `fabrik:paused` was never applied, no audit
   comment was posted.
2. **`stage:<Name>:in_progress` removal was incidental, not guaranteed.** It was only cleared if the
   worker goroutine reached `finalizeStageOutcome`'s cancellation branch and its `release()` closure
   before the process actually exited — a race against the (formerly unbounded) drain, never run at
   all on force-quit or crash. The two existing startup-recovery passes (`runStartupCleanup`,
   lock-gated; `runStartupOrphanedInProgressScan`, terminal-sibling-gated) both miss the resulting
   "bare `in_progress`, no lock, no `complete`/`failed` sibling" shape.
3. **In-flight worktree changes were silently discarded.** `commitWIP` and the branch push both sat
   *after* the cancellation early-out in `finalizeStageOutcome`, so a shutdown mid-stage left the
   worktree dirty and unpushed.

ADR-054 already covers *how* Fabrik kills a Claude subprocess (the SIGINT→SIGTERM→SIGKILL escalation
and kill-reason propagation) — but nothing about *whether to drain* or *what to write* before the
process actually exits. `handleStopRequest` (the TUI's per-issue stop) was the closest prior art —
cancel-with-reason, `fabrik:paused` + `fabrik:awaiting-input`, an audit comment — but Research found it
shared gaps 2 and 3 above: it never cleared `in_progress` itself, and had no idempotency guard against
a duplicate call re-posting its comment.

## Decision

### R3 — Reuse, not duplicate: `pauseInterruptedIssue`

`handleStopRequest` (TUI single-issue stop) and the new daemon-wide clean-stop pause
(`runShutdownPause`/`pauseIssueForDaemonShutdown`, `engine/shutdown.go`) both route through one new
shared primitive, `pauseInterruptedIssue(item gh.ProjectItem, comment string)`
(`engine/mutate.go`), which wraps the codebase's existing generalized "pause + comment" primitive,
`pauseIssue`/`pauseOpts`. This fixes the gaps in the cited prior art as a side effect of unification
rather than as a separate patch: both callers now clear `stage:<Name>:in_progress` directly before
pausing, and both share one idempotency guard.

**Idempotency is "skip if the store snapshot already shows `fabrik:paused`," not
`hasPauseComment`.** `hasPauseComment` needs a populated `item.Comments`, which neither caller builds
cheaply (both construct a skeletal `gh.ProjectItem{Number, Repo}`). The store's live label snapshot is
already available and cheap, and directly satisfies AC2: whichever caller (or re-entry) runs second
sees the label already applied and skips both the label writes and the comment post — using
`reapplyPauseLabels` (idempotent) instead.

Comment text and kill-reason string remain per-caller — `"stopped from TUI by <user>"` /
`"user_stop"` vs. `"paused by a daemon clean stop"` / `"daemon_shutdown"`. That is *provenance*, not
divergence: the label pair, the write ordering, and the idempotency guard are now identical between
the two "an issue got interrupted mid-stage" mechanisms, which is what R3 requires.

### R2 — direct, synchronous `in_progress` clear

`pauseIssueForDaemonShutdown` (and the refactored `handleStopRequest`) call `removeInProgressLabel`
themselves, using the stage name from the store's `Worker().StageName` — never delegated to the
cancelled worker goroutine's own `release()`, which is a race, not a guarantee. This is safe against a
concurrent `release()` call for the same label by construction: `applyLabelRemove` already treats
`gh.ErrNotFound` as success, so a second removal of an already-removed label is a no-op.

**A failed `fabrik:paused` write gets no new retry/settle-scan machinery.** The `fabrik:awaiting-*`
settle-scan family (ADR-1422, ADR-1097, ADR-061, ADR-1097) exists to retry across *future poll
cycles* — but the process is exiting immediately after this write, so there is no future poll cycle
within this run to retry in. The accepted fallback is R7's `runStartupBareInProgressScan`: it still
clears the stale `in_progress` label on next start regardless of whether `fabrik:paused` landed, so
the issue simply becomes re-dispatchable — indistinguishable from ordinary crash recovery. The
failure is still logged via `applyLabelAdd`'s existing warning path (R2's "reportable, not silent").
No new label is introduced (R6 intact).

**The in-flight snapshot itself is taken before `cancel()`, not after (validate-comment review
finding).** The first implementation had `runShutdownPause` enumerate `e.store.All()` for
`Worker() != nil` itself, from inside its own freshly launched goroutine — which only runs *after*
`beginShutdownPause` calls `cancel()`. A worker cancelled before or while starting its Claude
subprocess (no long kill-escalation wait to lose the race against) can run its own cancellation
branch — commit, push, `releaseLock()` — and its goroutine-level deferred `WorkerExited` in a
handful of milliseconds, fast enough to clear its `Worker()` entry before the newly scheduled
`runShutdownPause` goroutine gets to its own `e.store.All()` read. That issue would silently vanish
from the pause set: no `fabrik:paused`, no audit comment, and since the worker's own `release()`
already removed `in_progress` and the lock, R7's startup scan can't catch it either — indistinguishable
on the board from an issue that was never dispatched. Fixed by moving the enumeration
(`inFlightSnapshot()`) into `beginShutdownPause`, synchronously, strictly before `cancel()` is called:
at that point no worker has had any chance to react to the shutdown this call is about to trigger, so
the captured list is authoritative. `runShutdownPause` now takes the snapshot as a parameter instead
of computing its own. Regression-tested by
`TestBeginShutdownPause_SnapshotSurvivesRaceWithWorkerExit`, which clears the seeded issue's
`Worker()` from inside a wrapped `cancel()` — the earliest a real worker could react — and confirmed to
fail against the pre-fix enumeration-inside-the-goroutine shape.

### R4 — 30s default drain deadline, computed from the same `e.wg` workers use

The pause-write phase (`runShutdownPause`) is tracked on the *same* `sync.WaitGroup` (`e.wg`) that
already tracks in-flight workers: `e.wg.Add(1)` before launching it as a goroutine, `e.wg.Done()` on
completion. A single new primitive, `waitGroupTimeout(wg *sync.WaitGroup, timeout time.Duration)
bool` (`engine/shutdown.go`), bounds both concerns with one deadline — the standard Go idiom (a
goroutine closes a channel after `wg.Wait()`; the caller selects that channel against
`time.After(timeout)`), which had no existing equivalent anywhere in the codebase.

Default: **30 seconds**, configurable via `--drain-deadline`/`FABRIK_DRAIN_DEADLINE` (mirroring
`--kill-grace-sigint`'s shape exactly — `cmd/root.go`'s `drainDeadline(s string) time.Duration`
helper). This exceeds the 20-second worst case of the existing kill escalation
(`KillGraceSigInt` + `KillGraceSigTerm`, 10s + 10s by default) by a 10-second margin, budgeted for the
R1/R2 GitHub API writes (up to 2 label ops + 1 comment per in-flight issue), which are parallelized
one goroutine per issue rather than serialized. A startup warning,
`warnDrainDeadlineOrdering` (mirroring the existing `warnCIBackstopTimeoutOrdering` check), fires
when the configured drain deadline does not exceed `KillGraceSigInt + KillGraceSigTerm` — advisory
only, since both remain valid, positive durations and only their relative ordering is suboptimal.

**`DrainDeadline` has no "0 = unbounded" sentinel**, unlike `kill_grace`'s "0s = skip this step."
convention. An unbounded drain is the exact defect this issue fixes, so any non-positive value (after
parsing) falls back to the 30s default with a warning, rather than being honored as "wait forever."
This is a deliberate, documented divergence from the `kill_grace` convention, not an oversight.

The write-phase budget is not separately bounded from the overall drain deadline — `waitGroupTimeout`
covers both the worker drain and the pause-write phase together as one deadline, rather than two
nested ones. This is simpler and sufficient: N in-flight issues' GitHub writes run in parallel
(bounded, in practice, by how many workers were actually in flight, which is itself bounded by
`MaxConcurrent`), so the write phase's realistic wall-clock cost is a handful of REST round-trips, not
N sequential ones.

### R5 — force-quit unchanged, plus a genuine pre-existing bug fix

The second-signal (force-quit) path acquires no new obligations: it still calls `cleanupHook` and
`os.Exit(1)` immediately, completely independent of `e.wg`/the pause-write phase. Verifying this
uncovered a **pre-existing defect**, unrelated to this issue's own changes but blocking it (R5, AC4):
the second-signal listener's `select` raced against `ctx.Done()` instead of a dedicated "Run() is
actually returning" signal. Since `ctx.Done()` closes the instant the *first* signal's `cancel()`
fires — synchronously, in the same goroutine, before the second `select` is even reached — that
`select` almost always took the already-ready `ctx.Done()` case immediately and returned, meaning a
human physically could not press Ctrl-C a second time fast enough for force-quit to ever fire. This
was confirmed via a subprocess reproduction (`TestForceQuit_DuringCleanStop_AC4`) against the
pre-#1393 code shape: the helper process ignored two SIGINTs and had to be SIGKILLed.

The fix: a new `drainComplete` channel, `defer`-closed only when `Run()` is actually about to return
(for any reason), replaces `ctx.Done()` as the second select's "give up waiting for another signal"
case. The listener now stays parked on `sigCh` for the entire drain and only exits once `Run()` is
genuinely finishing on its own — restoring the two-signal semantics the pre-existing code intended but
never actually delivered.

### R6 — unchanged label semantics

Shutdown becomes a new *writer* of `fabrik:paused`, not a new state. It suppresses dispatch and
deep-fetch (#1379) exactly as before; resuming is still the operator removing the label.

### R7 — a third, narrow startup pass

`runStartupBareInProgressScan` (`engine/worker_liveness.go`) targets exactly: `stage:X:in_progress`
present, no `fabrik:locked:<any-user>` label present, no `stage:X:complete`/`failed` sibling present.
This is deliberately disjoint from the two existing passes' predicates — lock-gated
(`runStartupCleanup`) and terminal-sibling-gated (`runStartupOrphanedInProgressScan`) — rather than
broadening either of them, so all three remain individually self-justifying instead of one silently
absorbing another's responsibility. It defensively skips any item with an active `Worker()` in the
store (in practice always nil this early in startup, since it runs immediately after the first poll
cycle populates the store, before `startWorkerDetector` or any dispatch goroutine runs), mirroring
`runStartupCleanup`'s own guard shape.

This closes the gap R2 narrows but cannot eliminate: R2 only guards the SIGINT/SIGTERM clean-stop
path. A crash, a force-quit, or a `fabrik:paused` write that itself fails, all still leave a
worker's `in_progress` label unresolved with no in-process code path left to clear it — recovery on
next start is the sound, independent backstop this pass provides.

### R8 — commit-and-push before the cancellation early-out, for any cancellation reason

The smallest-diff option: `finalizeStageOutcome`'s `claudeRan` computation is hoisted above the
`ctx.Err() != nil` early-out (removing a now-duplicate later computation), and the early-out branch
now calls `commitWIP` + `pushBranchUnlessQueued` before `releaseLock()`, gated identically to the
ordinary (non-cancelled) path's `claudeRan && !completed && !stage.ReadOnly` — `completed` is always
false on this path (the invocation never reached a `FABRIK_STAGE_COMPLETE` marker), and a read-only
stage has nothing to commit (its stash was already restored earlier in the same function).

This applies to **every** cancellation reason — `daemon_shutdown`, `user_stop`, and any future
reason — not just daemon shutdown specifically. This is deliberate: diverging worktree-preservation
behavior between the TUI single-stop and the daemon-wide clean stop is exactly what R3 warns against,
and it also happens to fix the TUI stop path's identical, previously-unaddressed gap.

**What a resumed issue's worktree looks like (AC8):** the branch carries a
`chore: partial <Stage> stage progress (incomplete)` commit (the existing `commitWIP` convention,
excluding `.fabrik-context/`) and has been pushed to `origin` (best-effort — a push failure is logged
as a warning, non-fatal, matching every other push-after-stage call site). The board shows
`fabrik:paused` + `fabrik:awaiting-input` and an audit comment; `stage:<Name>:in_progress` is absent.
Resuming is unchanged: the operator removes `fabrik:paused`, and the stage re-dispatches, resuming
from the pushed partial-progress commit exactly as any other interrupted-and-resumed stage does today.

The audit comment states this as *system policy* ("in-progress worktree changes are committed and
pushed automatically") rather than a live per-run confirmation: the commit runs on the worker's own
goroutine, asynchronously from the pause-write phase that posts the comment, with no ordering
guarantee between the two — so the comment describes behavior, not an observed fact about this
specific invocation.

### SIGHUP inherits the bounded wait, not the pause — structurally, not by a flag

The pause-write phase (`runShutdownPause`) is only ever launched from the SIGINT/SIGTERM goroutine
(`e.wg.Add(1)` + `go e.runShutdownPause()`, immediately after `cancel()`) — never from
`registerSighupHandler`, which still calls `cancel()` directly with no `issueCtxs`/pause interaction,
exactly as before. This means SIGHUP's exclusion from the new pause behavior falls out of *where the
code is launched from*, not a new coordination flag that could drift out of sync.

Collapsing the four previously-identical inline drain copies in `poll.go` into one shared
`e.drainAndExit` helper means SIGHUP's `e.wg.Wait()` becomes **bounded** by the same
`drainDeadline()` as a side effect. This is an accepted, in-scope, and net-positive behavior change,
explicitly recorded rather than left ambiguous per the issue's Out-of-Scope note: today, an unbounded
SIGHUP drain can hang forever on a stuck worker with no operator recourse short of a second SIGHUP;
after this change, a SIGHUP restart-in-place always completes within `drainDeadline()`, exactly like
SIGINT/SIGTERM, without ever pausing any in-flight issue.

## Consequences

**Positive:**
- The board now durably records "we stopped this issue and why" for every daemon clean-stop, closing
  gap 1. `stage:<Name>:in_progress` is cleared directly rather than raced, closing gap 2 for the
  SIGINT/SIGTERM path; R7's new startup pass closes the residual crash/force-quit window
  independently.
- In-progress worktree changes are committed and pushed rather than silently discarded, for both the
  daemon clean-stop and the pre-existing TUI single-stop path, closing gap 3 for both mechanisms at
  once.
- `handleStopRequest` gained an idempotency guard and an `in_progress` clear it never had before, as a
  side effect of unifying onto `pauseInterruptedIssue` rather than as separate follow-up work.
- A genuine, previously-unnoticed force-quit defect (the `ctx.Done()`-vs-second-signal race) is fixed,
  independently confirmed via subprocess reproduction before the fix and after.
- The drain is now bounded by a stated, configurable deadline instead of relying solely on a second
  Ctrl-C, for both SIGINT/SIGTERM and (as an accepted side effect) SIGHUP.

**Negative / Trade-offs:**
- On `waitGroupTimeout`'s timeout branch, the internal goroutine blocked on `wg.Wait()` (and any
  still-in-flight per-issue pause writes) is abandoned — accepted, not fixed, since R5 requires
  force-quit-equivalent promptness from the drain deadline itself; this mirrors the pre-existing
  force-quit path's own goroutine-abandonment behavior.
- Per-issue pause writes are parallelized (one goroutine per in-flight issue), which could pressure
  the REST rate limit at high `MaxConcurrent` right as the process exits. No rate-limit-aware
  throttling is added — the existing `shouldPauseForRESTRateLimit` gate only runs inside the poll
  loop, not the shutdown path, and adding it here is out of scope. Documented as a known limitation.
- A failed `fabrik:paused` write during shutdown has no retry within the same run (see R2 above) —
  only R7's next-startup healing of the `in_progress` label, not a guaranteed `fabrik:paused`. The
  issue becomes ordinary crash-recovery-shaped rather than clean-stop-shaped in that narrow case.

## Sibling Audit

ADR-054 remains the authority on *how* Fabrik kills a Claude subprocess (the SIGINT→SIGTERM→SIGKILL
escalation and kill-reason propagation) — this ADR does not change that mechanism, only what
surrounds it: whether to drain, what to write, and how long to wait. The `fabrik:awaiting-*`
settle-scan family (ADR-1422, ADR-1097, ADR-061, ADR-060) is deliberately **not** extended to cover a
failed shutdown-time `fabrik:paused` write — see R2's rationale above for why that family's premise
(retry across future poll cycles) does not apply to a process that is exiting immediately.

**References:** [ADR-054: SIGINT→SIGTERM→SIGKILL Escalation and Kill-Reason Propagation](054-sigint-kill-escalation-and-reason-propagation.md), [ADR-1422: Terminal Advance Escalation and Settle Scan](1422-terminal-advance-escalation-and-settle-scan.md), [ADR-1097: Non-Default-Base Explicit Close Retry](1097-non-default-base-close-retry.md), ADR-002 (GitHub as state store), ADR-036 (reactive-cache single-owner store), #1379 (`fabrik:paused` suppresses deep-fetch)
