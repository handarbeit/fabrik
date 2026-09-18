# ADR 1779: Sentinel-Probe Verification for PID-Unknown Worker Liveness

**Date**: 2026-09-18
**Status**: Accepted
**Issue**: #1779 — worker-liveness clears an unverifiable worker without probing its sentinel,
allowing two workers on one worktree

## Context

`runWorkerDetectorScan` (`engine/worker_liveness.go`) has always had two liveness-verification
tiers for a `WorkerHandle`, and they are not equally strong:

1. **Signal-0** (`PID > 0`): requires both a stale heartbeat *and* a failed `kill -0`. Confirmed
   dead only — a live-but-unresponsive process is left alone indefinitely.
2. **Timeout-only** (`PID <= 0`, added by #1303): pure elapsed time against `Worker.StartedAt`,
   with no verification at all, because signal-0 has nothing to target — a worker that never
   reached `onPIDReady` has no PID on record.

#1303 introduced tier 2 deliberately: an unconditional skip on `PID <= 0` lets a dispatch goroutine
that hangs *before* `onPIDReady` fires (stuck in `ensureRepoReady`, before the child process is even
started) outlive its own `WorkerEntered` marker indefinitely, permanently wedging dispatch behind
it. But its own code comment was candid that the fix traded one failure mode for another: "this
can't be confirmed dead via signal-0, so it is a timeout-based clear, not a confirmed-dead clear."

#1749 (a community report, 8 clears across 5 issues in 3 days, every one firing at the default
5-minute threshold, not a race) showed the cost of that trade concretely: a real `claude` subprocess
(PID 66965) was cleared at the timeout, then ran for a further 1h36m and authored five files on the
branch, while a second worker (PID 83369) was dispatched onto the same worktree for the same
(issue, stage) at 07:00:24Z. Both held `Write`/`Edit` grants; their output interleaved in one branch
with no engine record of which wrote what. The reporter's own follow-up correction is important
context: the item was not permanently wedged (it self-recovered ~20 minutes later, the orphan's
output was collected normally), so the actual harm is **duplicate concurrent writers and lost
provenance**, not a stall — the fix must not become a wedge-recovery mechanism.

The missing piece already existed and was unused: every worker invocation carries
`--name fabrik:<owner>/<repo>#<issue>:<stage>` (`sessionNameSentinel`, `engine/claude.go`), a
deterministic tag built from data the scan already has (`repo`, `issue number`, `stage name`) —
independent of whether the engine's own `Worker.PID` field was ever successfully updated. Its doc
comment stated it was "purely observational — nothing in the engine parses it back or branches on
it." The reporter located both orphaned processes with exactly a `ps | grep` against this sentinel.
A worker with no recorded PID is still positively identifiable; the engine simply never looked.

This mirrors a recurring principle already established in this codebase by ADR-1222 and ADR-1410:
verify liveness structurally where a verification signal exists, rather than inferring death from
elapsed time alone. Tier 2 was the one remaining place inferring death from elapsed time alone with
no fallback structural signal at all.

## Decision

### A third, intermediate liveness tier: sentinel-probe

Before tier 2's timeout clear actually fires, probe for a live process carrying the worker's
sentinel. This does not replace tier 2 — it gates entry into it:

- **Sentinel found live** → treat the worker like tier 1's "PID alive, heartbeat stale" case: log
  and wait, do not clear. Where the probe also yields the OS PID (the unix implementation always
  provides it), adopt it into the handle via the pre-existing `WorkerPIDSet` mutation and apply a
  fresh `WorkerHeartbeat` — so the *next* scan cycle finds `PID > 0` and routes through tier 1
  (ordinary signal-0 liveness) from then on. No new "half-verified" state is introduced; adoption
  reuses machinery that already exists for the normal `onPIDReady` callback.
- **Sentinel affirmatively not found** → clear exactly as tier 2 always has. This is the #1303
  regression guard: a worker that genuinely never started a real process (hung in `ensureRepoReady`)
  still gets cleared on timeout, unconditionally, preserving #1303's entire rationale.
- **The probe itself cannot run** (unsupported platform, `ps` error, non-zero exit, timeout) → a new,
  fourth outcome tier 2 never had to handle: neither clear immediately nor defer forever. A bounded
  consecutive-failure counter allows a small number of scan cycles of grace before falling back to
  tier 2's plain clear, distinctly logged as unverified.

### One probe implementation, two call sites

`probeSentinelLive` is called from both `runWorkerDetectorScan` (the tier above) and independently
from `dispatchCandidates` (`engine/poll.go`), immediately after the existing `snap.Worker() != nil`
in-flight guard. The dispatch-site check is a deliberate belt-and-suspenders addition, not a
consequence of the scan-site fix being insufficient on its own: the scan-site fix is meant to make
"dispatch a second worker while the first's sentinel is still live" unreachable, but the issue's own
R5 requirement says to assert this independently rather than trust that inference. Sharing one
implementation between both call sites (behind a package-level `sentinelProbeFn` function-var seam,
mirroring `claudeNameFlagSupported`'s existing save/restore test convention) means a portability or
correctness fix to the probe cannot silently apply to only one of the two guards.

The dispatch-site check **fails open** on a probe error (allows dispatch), unlike the scan-site's
tier-4 behavior (which eventually clears after a bound). This asymmetry is deliberate: by the time
`dispatchCandidates` reaches this check, `Worker() == nil` already — either R1/R2 above kept the
worker alive and never got here, or a clear already happened (via R3 or R4's bound) and this is the
*second* worker attempt. Treating a probe error as "block dispatch" here would let a systematically
broken `ps` wedge the *entire* dispatch path for every item on every poll — a strictly worse and
far more visible failure mode than the rare duplicate-writer this check exists to catch. The
scan-site's own tier-4 bound already accepts a small, capped amount of wedge risk in exchange for
not clearing blind; doubling that risk at the dispatch site for no additional protection was
rejected.

### `ps`-based process scan, not `/proc`

The probe shells out to `ps -eo pid=,args= -w -w` via `exec.CommandContext` (no shell, 3s timeout)
and matches the sentinel as an exact whole argv token (`strings.Fields` + `==`), never a substring —
the issue's own worked example (`#2966` must not match a decoy `#29660`) makes clear that a
naive `strings.Contains` would produce exactly the kind of false-positive liveness this mechanism
exists to prevent. `-w -w` (BSD `ps`'s `-ww` equivalent) defeats macOS `ps`'s command-column
truncation, which would otherwise silently drop a long invocation's `--name` value near the end of
its argv and produce a false "not found" — the opposite failure direction from a false positive, but
just as damaging (it would reproduce the exact bug this issue fixes).

A `/proc/<pid>/cmdline`-based reader was considered and rejected: it is Linux-only and would need an
entirely separate backend for macOS (no `/proc`), doubling the implementation surface to serve
platforms that already work fine with one `ps`-based path. Windows gets a build-tag stub that always
reports itself unsupported — routing every Windows worker through tier 4's bounded-unverifiable
path, the same posture `isProcessAlive`'s existing Windows stub already takes for tier 1's signal-0
check (never confirms a PID dead on Windows).

### Gated on `claudeNameFlagSupported`, both call sites

`--name` is itself gated on a one-time, process-lifetime capability probe of the installed `claude`
binary (`claudeNameFlagSupported`, pre-existing). If that probe failed, no worker in this Fabrik
process could ever carry a sentinel — every probe call would legitimately report "not found," which
is indistinguishable in outcome from tier 2's plain clear but costs a subprocess spawn on every
scan cycle and every dispatch pass for no benefit. Both call sites check this flag first and, when
false, skip the probe entirely and fall straight through to the pre-#1779 behavior — byte-identical
to today's behavior for any fleet running an older `claude` CLI.

### The bounded-unverifiable counter is engine-local, not durable `itemstate`

The consecutive-probe-failure count lives in `Engine.sentinelProbeFailures map[string]int`
(mutex-guarded, keyed `"owner/repo#N"`), not as a new field on `itemstate.WorkerHandle` with an
accompanying mutation type. This mirrors the existing shape of `mergeTrainRunawayAlerted`/
`mergeTrainTrials` in `engine/merge_train.go` rather than the `StageRetryIncremented`-style bounded
counters that do live in `itemstate`. The distinction: `itemstate.Store` mutations exist for state
other subsystems need to observe or react to via the store's change-notification machinery. Nothing
outside this one scan needs to see the probe-failure count — it is pure scan-goroutine bookkeeping,
scoped to the lifetime of a single unverifiable episode, and is safely non-durable: a `Worker` with
`PID <= 0` can only exist for a worker *this Fabrik process* dispatched (the store itself is
in-memory and always nil on restart), so there is no cross-restart durability requirement to give up
by keeping it engine-local.

The counter is cleared whenever a worker leaves the unverifiable state through any exit: sentinel
found live (adopting a PID `#1303`-tier-1-side clears it too, defensively, in case the real
`onPIDReady` callback wins the race independently of adoption), sentinel confirmed not found, or
cleared at the bound. `cleanupStaleWorker` — the single function underlying every clear path in this
scan — clears it unconditionally, so no clear path can leave a stale non-zero count behind for a
future, unrelated worker at the same (repo, issue) key.

### The bound: 3 consecutive scan cycles, not configurable

At the 60-second scan interval, 3 cycles is ~3 minutes of additional grace on top of the existing
5-minute `WorkerStaleTimeout` default — enough to absorb a single transient `ps` hiccup (a brief
resource contention, a signal interruption) without meaningfully widening #1303's original wedge
window. This matches this codebase's existing default for bounded-retry counters elsewhere
(`MaxToolsDeniedRetries` defaults to 3). It is deliberately not exposed as a flag: the issue itself
frames the exact number as an implementation choice, not a product requirement, and a knob invites
tuning against no real operational data. If field experience later shows 3 is wrong in either
direction — too eager (still producing occasional duplicate writers from `ps` flakiness) or too
conservative (widening the wedge window somewhere `ps` is more than transiently unreliable) — that
is a follow-up issue grounded in evidence, not a speculative parameter added now.

## Consequences

- A genuinely orphaned-but-still-running worker (the reported #1749 shape) is no longer cleared out
  from under itself — its sentinel is found live, its PID is adopted, and it proceeds to completion
  normally under tier 1's ordinary signal-0 liveness, eliminating the duplicate-writer risk for the
  large majority of cases the reporter characterized (6 of 8 observed clears "recovered fine," i.e.
  were never actually dead).
- #1303's wedge protection is fully preserved: a worker that truly never started a process is still
  cleared, unconditionally, once the sentinel probe affirmatively reports "not found." The existing
  `TestDetectorClearsPIDNeverSetAfterStartedAtTimeout` regression test passes unmodified.
  `claudeNameFlagSupported == false` bypasses the probe with `sentinelProbeFn` never invoked at all,
  reducible via the identical clear that predates this ADR.
  Additional coverage: `TestDetectorSentinelNotFound_ClearsWorker` (the same clear, exercised through
  an engaged probe returning `Live: false`).
- The wedge window (#1303's risk) can widen by up to ~3 minutes in the specific case where `ps`
  itself is broken for the full duration — a strictly bounded, logged-as-unverified degradation, not
  an unbounded one.
- Two new subprocess-spawn call sites are introduced (`ps`, once per stale `PID<=0` worker per scan
  cycle, and once per dispatch-eligible item per poll pass with `Worker() == nil`) — both bounded by
  a 3-second timeout and gated on `claudeNameFlagSupported`, so the additional cost is zero on a
  fleet running an older `claude` CLI and small otherwise (proportional to items actually being
  dispatched or actively timed-out, never full board size).
- `docs/USER_GUIDE.md`/`docs/stage-lifecycle.md`'s prior claim that the `--name` sentinel is
  "observability-only" and that "nothing in the engine reads, parses, or branches on it" is now
  false and has been corrected in the same change (see Scope in the issue).
- The reinvoke dispatchers (`reinvoke.go`, `reviews.go`, `ci.go`, `merge_gate.go`) — which share the
  same `snap.Worker() != nil` guard shape as `dispatchCandidates` — do *not* get the R5 sentinel
  check in this change. The issue's evidence and acceptance criteria are scoped to the main
  stage-dispatch path; if the same duplicate-writer risk is later shown to be reachable through a
  reinvoke path, that is a follow-up issue, not a silent gap left unexamined here.
