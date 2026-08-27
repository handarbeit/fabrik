# ADR 1661: Merge-Train Job Row Emission Timing

**Date**: 2026-08-26
**Status**: Accepted
**Issue**: #1661 — give the merge train a TUI job row, emitted before setup rather than after

## Context

The merge train previously had no TUI representation at all: repo-level train activity
(`e.logf(0, "merge-train", ...)`) was routed to the single-line header status, which the very
next unrelated event (`[poll]`, `[cache]`, `[webhook]`) clobbered. During a multi-minute batch
run — trial assembly, CI polling, bisection, landing — the TUI showed nothing train-related,
answerable only by reading `.fabrik/fabrik.log` directly.

Fixing this means giving the train a job row, keyed by `(Repo, IssueNumber=0)`, driven by a
`JobStartedEvent`/`JobCompletedEvent` pair around `runMergeTrainWorker`'s batch lifecycle —
mirroring the existing `processItem`/`processComments` idiom.

ADR-040 established a load-bearing rule for that idiom: `JobStartedEvent` must be emitted from
inside the work function, **past all early-return guards** (lock acquired, all pre-work checks
passed) — never at goroutine-launch, to avoid ghost "In Progress" entries for work that never
actually started. `runMergeTrainWorker` calls `prepareTrainWorker` first, which does one-time
setup (semaphore acquisition, repo readiness, base-branch resolution, base-SHA pinning, member
fetch, restart-time reconstruction) and can itself fail immediately — e.g. context cancelled
before the semaphore is acquired, `ensureRepoReady` failing, no holding stage configured. Under
ADR-040's rule as written, `JobStartedEvent` would fire only after `prepareTrainWorker` returns
`ok=true`.

`prepareTrainWorker`'s own diagnostic log lines are exactly the "is it stuck?" answers this issue
exists to surface (issue's own Prior Art section cites a real "is the worker wedged?"
investigation answerable only via the log file). If `JobStartedEvent` fires only after
`prepareTrainWorker` succeeds, those setup-phase lines have no row to attach to — they would
still fall through to the header (correct per R6, but back to the original clobbering problem
for exactly the lines most likely to explain a train that never gets going).

## Decision

Emit `JobStartedEvent` at the very top of `runMergeTrainWorker`, **before** calling
`prepareTrainWorker` — not after it succeeds. This is a deliberate, narrow deviation from
ADR-040's default rule for this one call site.

The deferred `JobCompletedEvent{Skipped: true}` is registered immediately alongside it, so every
exit path — including every `prepareTrainWorker` failure — clears the row. `Title` is built once
from the dispatched `batch` parameter (member count + issue numbers via the new
`trainBatchTitle` helper), which is available before `prepareTrainWorker` runs and is never
recomputed as membership narrows during bisection.

No merge-train decision, condition, or control-flow logic changes as a result of this ADR
(#1661 R8) — the change is exclusively about when a TUI event fires, never about when
`runMergeTrainWorker`'s actual work begins or how it proceeds.

## Rationale

**Why not wait for `prepareTrainWorker` to succeed?** The whole value of this issue is
answering "what is the train doing / why is it stuck" without reading the log file.
`prepareTrainWorker`'s failure-path log lines (repo-not-ready, cannot determine base branch, no
holding stage configured, cannot pin base SHA) are precisely the lines that answer "why did the
train never start" — the single most useful diagnostic case a merge-train job row exists for.
Deferring `JobStartedEvent` until after this function returns successfully would mean those
lines are invisible in the TUI in exactly the scenario an operator most needs them.

**Why is this acceptable given ADR-040's rationale?** ADR-040's concern was *indefinite* ghost
entries — a goroutine launched and never did any real work, but its row lingered because nothing
ever cleared it. That failure mode does not recur here: the deferred `JobCompletedEvent` is
registered in the same breath as `JobStartedEvent`, so a `prepareTrainWorker` failure (e.g.
context cancelled before the semaphore is acquired) produces at most a brief flash-then-vanish
row, not an indefinite ghost. ADR-040 §Consequences.4 already accepts an analogous brief flicker
for `processItem`'s own lock-then-verify race ("~50ms... acceptable; an indefinite ghost is
not") — this is the same trade-off class, just triggered by a different early-exit condition.

**Why not add a guard to skip the flash for a fast failure?** That would cross from presentation
into logic — deciding *when `runMergeTrainWorker`'s real work begins* based on TUI cosmetics,
which R8 explicitly forbids. The fix belongs entirely in *when the TUI event fires*, which is
exactly what this ADR does and nothing more.

## Consequences

1. **This is the one call site where `JobStartedEvent` fires before ADR-040's "past all
   early-return guards" boundary.** Any future refactor of `runMergeTrainWorker` must preserve
   this ordering deliberately, not "fix" it back to ADR-040's default without re-reading this
   ADR.

2. **A `prepareTrainWorker` failure produces a rare flash-then-vanish row** in the TUI active
   pane instead of no row at all. This is expected and acceptable — see Rationale above — and
   is bounded by the same defer that would otherwise leave a ghost entry.

3. **`Title` is fixed at dispatch time from the raw `batch` parameter**, not the post-fetch
   `current []trainMember` `prepareTrainWorker` would have produced. Since `Title` is never
   recomputed later either (a separate, independent design choice — see the Plan-stage
   discussion in issue #1661), this has no follow-on effect: the row's evolving narrative comes
   entirely from `LastLine` via routed `merge-train` `LogEvent`s, not from `Title`.

4. **No merge-train decision logic changed.** Every diff introduced by #1661 is either a
   `logf`→`logfRepo` call-site rename (mechanical, ~78 sites) or a new `emitStructural` call;
   no conditional, threshold, or control-flow branch inside `runMergeTrainWorker` or any helper
   it calls was touched.

## References

- Issue #1661 (job row, this ADR's originating change)
- ADR-040 (`040-job-started-at-work-boundary.md`) — the general rule this ADR deviates from,
  and whose §Consequences.4 "brief flicker is acceptable, indefinite ghost is not" trade-off
  this ADR relies on directly
- ADR-067 (`067-merge-train-centralized-inflight-cleanup.md`) — `finishTrain` as the sole legal
  clear point for `mergeTrainInFlight`; this ADR's `JobCompletedEvent` defer sits alongside it
  without introducing a second, independently-reasoned cleanup point
- `docs/USER_GUIDE.md` §8 "TUI Dashboard" — user-facing description of the resulting job row
