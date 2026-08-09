# ADR 1410: CI Gate — Liveness, Not Elapsed Time

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1410 — `CIWaitTimeout` paused a healthy, progressing CI suite purely because it ran
longer than a fixed constant

## Context

`CIWaitTimeout` (30-minute default) was a single wall clock applied uniformly across four
structurally different CI states in `engine/ci.go`:

| condition | site | is a clock the right instrument? |
|---|---|---|
| check runs exist, pending, transitioning | `classifyCIFromCheckRuns` | **No** — CI is demonstrably alive and reporting |
| check runs report failure | same guard, fires ahead of classification | **No** — a verdict arrived; terminal input to the CI-fix loop, not a wait |
| `OPEN+BLOCKED`, no check runs ever (R3) | `classifyCIFromMergeableState` | **Yes** — a required check is configured but never triggers; no event is coming |
| `mergeable_state` blocks, no check runs visible | `classifyCIFromMergeableState` | **Yes** — external signal that may never arrive |

Only the bottom two are genuine liveness problems — there is no event Fabrik could wait for,
because none is coming without human intervention. The top two are healthy or already-resolved CI
being charged against an arbitrary constant that has no relationship to how long CI actually takes.

### The field incident

`verveguy/liminis-context-graph#342` (2026-08-04): `fabrik:awaiting-ci` was anchored at
12:51:45Z. A rebase reinvocation consumed 15m46s of the 30-minute budget and pushed a new head at
13:07:31Z. The fresh CI run started at 13:07:40Z with 14m05s of budget left, and the suite takes
18m04s. Fabrik paused at 13:21:57Z. CI completed **fully green** at 13:25:44Z on a PR that was
`CLEAN`, `APPROVED`, and `MERGEABLE`. Nothing was wrong except that the clock was shorter than the
suite — and the rebase interaction made it worse by consuming budget before CI even started, so
the effective wait for the fresh run was under 14 minutes against an 18-minute suite.

### The failure-as-timeout defect (R3)

`classifyCIFromCheckRuns`'s `CIWaitTimeout` guard fired *ahead* of classifying pending vs. failed,
and its own doc comment said it covered "both 'still pending' and 'failed' states." A CI failure
occurring after the 30-minute mark was therefore reported as a **timeout** — bypassing
`MaxCiFixCycles` and the CI-fix reinvocation path entirely, and posting a message claiming Fabrik
"timed out waiting for checks to pass" when checks had in fact reported a definite verdict.
`classifyCIFromRequiredContexts` had the identical shape for required-status-context failures
(ADR-933).

### Architectural discovery during implementation: the pending branch is unreachable in production

While the redesign below is correct on its own terms, this discovery changes which mechanism
actually fixes #342 in production. `checkMergeabilityGate` (`engine/merge_gate.go`) unconditionally
claims any `PRMergeUnsettled` classification — and `settlePRMergeState` (`engine/pr_settle.go`)
**always** returns `PRMergeUnsettled` for `CheckRunsPending`, with no other path to that state. Since
`handleMergeAndCIGates` (`engine/catch_up_handlers.go`) returns immediately when the merge gate
claims the item, `checkCIGate` — and therefore `classifyCIFromCheckRuns`'s pending branch — is never
reached for a merely-pending CI state via the async settle pipeline. This was equally true of the
*old* `CIWaitTimeout` guard before this issue: the pending-branch clock was already dead code from
the pipeline's perspective, reachable only via a direct `checkCIGate` call (as the unit tests in
`engine/ci_test.go` do). `settleAwaitingCIScan`'s unconditional backstop (ADR-1270, #1303) — not
`classifyCIFromCheckRuns`'s inner guard — was and is the mechanism that actually escalated #342 in
the field. This is confirmed non-hypothetically: `TestSettleAwaitingCIScan_342Repro_SlowButHealthyCI_DoesNotPause`
reproduces the incident end-to-end through `settleAwaitingCIScan`, and its "unmodified-engine" sub-test
proves a 30-minute-equivalent backstop *does* fire at #342's own 34-elapsed-minute timeline.

This does not make the pending-branch liveness dwell (below) pointless — it is still correct
behavior for a direct `checkCIGate` caller, remains what the acceptance criteria's unit-level tests
exercise, and is defensive against a future change to the merge-gate claim priority — but the
**backstop resize (R5) is what actually resolves #342 for real traffic**, not the liveness dwell.
This is recorded here rather than left implicit, since it inverts which of this ADR's two
mechanisms carries the practical fix.

## Decision

Split `CIWaitTimeout`'s single clock into two independent instruments, by what each condition
actually needs.

### 1. Verdicts never wait (R3)

`classifyCIFromCheckRuns` now classifies via `gh.ClassifyCheckRuns` first. On `CheckRunsFailed` it
returns `ciFailure=true` unconditionally — no clock, regardless of how long the gate has been open.
`classifyCIFromRequiredContexts` drops its `CIWaitTimeout`/`labelAppliedAt` guard entirely and
always returns `ciFailure=true` on `RequiredContextsFailed`. Both route to the existing CI-fix
reinvocation path (`MaxCiFixCycles`) exactly as a fresh failure always has.

### 2. Genuine liveness dwells stay, one generalized

`classifyCIFromMergeableState`'s two cases — R3's `OPEN+BLOCKED`-never-checked, and a blocking
`mergeable_state` with no check runs visible — have no observable signal to track progress on at
all (no event is coming without human intervention), so they keep their original
`labelAppliedAt`-anchored elapsed dwell, unchanged. `classifyCIFromCheckRuns`'s **pending** branch
gains an analogous dwell, but anchored on observed progress rather than elapsed time: a new
`ciProgressStalledSince` helper reads `LinkedPRState.LastCIProgressAt` from the `itemstate` store,
and escalates only once `time.Since(LastCIProgressAt) >= ciWaitTimeout()` **and** the timestamp is
non-zero. Both dwells reuse the repurposed `CIWaitTimeout` config value (see "Configuration
compatibility" below) — a single value covers both "never checked" and "checks frozen," since
nothing in the acceptance criteria requires distinguishing them and splitting later is a small
additive change if field experience shows otherwise.

`LastCIProgressAt` is set in `internal/itemstate/store.go`:

- `applyCheckRunCompleted`, when `upsertCheckRunByID` reports actual content change (a new
  check-run ID appeared, or an existing one's `Status`/`Conclusion` transitioned) — not on an
  identical duplicate observation, which is the negative case the stall dwell depends on.
- `PRHeadSHAUpdated`'s existing SHA-change branch, and its pending-check-run drain block when the
  drain actually adds runs — a fresh push resets CI entirely, which is progress in its own right,
  not merely a prerequisite for future progress. This is what removes #342's dependence on *when*
  the wait started: a rebase-triggered head change needs no special handling, because it already
  counts as progress.

All three check-run producers (`settleAwaitingCIScan`'s `RefreshCheckRunsLive`, `FetchCheckRuns`'s
cache-miss path, and `check_run` webhook deltas) already funnel through `applyCheckRunCompleted`,
so this one edit point covers all of them.

**Cold-start default**: the itemstate store is entirely in-memory with no persistence across a
restart, and GitHub exposes no change-history equivalent to backfill it from (unlike
`labelAppliedAt`'s REST fallback). A zero `LastCIProgressAt` — never observed since this process
started — never escalates; it only re-observes. This composes safely with `settleAwaitingCIScan`'s
backstop: a cold restart does not mass-pause every `fabrik:awaiting-ci` item (the pending-branch
dwell can't fire blind), and does not permanently suppress the backstop either (the backstop's own
guard is independently anchored on `labelAppliedAt`, not on this progress signal).

### 3. A separate, much larger absolute backstop bounds per-poll cost (R5)

New `CIBackstopTimeout` config (240-minute / 4-hour default), consumed only by
`settleAwaitingCIScan`'s unconditional guard (`engine/ci_settle.go`) and merge-train's blocking
polls (below) — never by `checkCIGate`'s classifiers. It exists purely to cap how long
`fabrik:awaiting-ci` can hold a deep-fetch plus a live `RefreshCheckRunsLive` call every poll,
independent of any suite's expected duration — the justification R5 explicitly asks be kept
separate from the liveness dwell's own justification (waiting out a slow-but-alive suite). Given
this ADR's "architectural discovery" above, this is the setting that actually governs #342-shaped
incidents for pending CI in production; its size is what makes an 18-minute suite (or any suite
under 4 hours) unaffected.

The backstop remains anchored on `labelAppliedAt` (total elapsed time since `fabrik:awaiting-ci`
was applied), not on `LastCIProgressAt` — deliberately: its purpose is bounding per-poll cost
regardless of what CI is doing, which is an elapsed-time question, unlike the liveness dwells above.

### 4. Merge-train's blocking loops keep an elapsed bound, repointed (R6)

`pollForMergeable` and `pollTrainCI` (`engine/merge_train.go`) are synchronous, blocking polls
inside a single worker goroutine, not re-entrant poll-driven state — adopting liveness semantics
would hold the goroutine open for a suite's full duration, a cost the async gate doesn't pay. They
already degrade gracefully on timeout (retry next merge-train cycle, no pause, no escalation)
rather than reproducing #342's destructive pause, so the urgency to redesign them is lower than for
the async gate. Both now derive their deadline from `CIBackstopTimeout` instead of the
now-much-shorter, repurposed `CIWaitTimeout` — using the liveness dwell here would force a wasted
trial-branch rebuild roughly every 30 minutes for a healthy-but-slow suite; the backstop fixes that
real remaining cost without a redesign. `pollTrainCI`'s check-run-failure branch already returned
`TrainCIRed` immediately and unconditionally before this change (R3 was never a problem here), so
no equivalent verdict/wait split was needed on that path.

`pauseForMergeGroupStall`/`checkAutoMergeConvergence` (`engine/merge_gate.go`, ADR-058 D5) needed
**no code change**: it already fires only when merge-group CI has never reported at all —
structurally identical to R3, already anchored on `fabrik:auto-merge-enabled`'s `labelAppliedAt`
rather than the CI-await window's own clock. It keeps reading `e.cfg.CIWaitTimeout` directly, which
under the repurposing below is exactly the right value (the liveness dwell, not the backstop).

### 5. No new pause function or message (R7)

The check-run-stall escalation reuses `pauseForCITimeout` verbatim. After this change, every caller
of that function represents a genuine liveness-dwell-exceeded condition — never a plain "still
pending" state, and never a confirmed failure — so its existing message ("timed out waiting for
checks to pass") stays truthful. Recoverability is inherited from #1408's episode-matching model
(`hasCIGatePauseComment`/`reapplyCIGatePauseLabels`) for free, with no new code path to compose.

## Configuration compatibility (R8)

`FABRIK_CI_WAIT_TIMEOUT`/`--ci-wait-timeout` keep their name, flag, and 30-minute default — an
operator who never touched this setting sees the identical resolved value
(`TestCiWaitTimeout_DefaultUnchanged` pins this). What changes is **meaning**: from "cap on total
CI wait" to "liveness-stall dwell." This is stated explicitly in three places so it is never a
silent change:

1. This ADR.
2. `docs/USER_GUIDE.md`'s `--ci-wait-timeout` entry and the dedicated CIWaitTimeout section.
3. A one-time informational startup log line (`logCIWaitTimeoutSemantics`, `engine/poll.go`),
   unconditional — unlike the Anthropic-auth-namespace notices it sits alongside, which only fire
   on an active opt-in, this fires for every operator regardless of whether they've customized the
   value, since the setting's meaning changed under them either way.

`CIBackstopTimeout`/`--ci-backstop-timeout`/`FABRIK_CI_BACKSTOP_TIMEOUT` is new (R5), following the
identical `resolveInt`/`explicitFlags` pattern every other `*_WAIT_TIMEOUT` setting in
`cmd/root.go` uses, with its own 240-minute default.

## Rationale

### Why repurpose `CIWaitTimeout` rather than add a third setting

R1–R3 remove two of the four conditions `CIWaitTimeout` used to gate (pending-verdict, failed) from
clock-governed behavior entirely. What's left — R3's never-checked case, the blocked-mergeable-state
case, and the new check-run-frozen case — are all genuinely the same shape of dwell, and keeping the
existing ~30-minute default matches what R3's own dwell was already tuned for. A brand-new setting
here would mean three-plus CI-timeout knobs for an operator to reason about for no behavioral gain.
R5's backstop gets its own new setting instead, satisfying R5's explicit requirement to justify its
value separately from the liveness dwell.

### Why not add `StartedAt`/`CompletedAt` to `gh.CheckRun`

`gh.CheckRun` already flows through one choke point (`upsertCheckRunByID`'s `reflect.DeepEqual`)
that detects a new check-run ID appearing or an existing one's status/conclusion transitioning —
sufficient to satisfy R2's "define concretely from data Fabrik already fetches" and the acceptance
criteria. Adding `started_at`/`completed_at` would require new REST parsing (`github/prs.go`), new
webhook-payload fields (`boardcache/delta.go`), and plumbing through a shared type with ~35+
existing call sites, for signal that would not actually close the one gap that remains open (below).

### Accepted limitation: a single long-running check with no interim transitions

A suite with one check that runs for three hours reporting only `queued`→`in_progress`→`completed`
— no sibling checks, no reruns — produces no observable state change for its entire duration under
ID/Status/Conclusion-only change detection. `started_at`/`completed_at` would not close this gap
either, since GitHub exposes no mid-run heartbeat regardless of which fields are parsed; neither
value advances without a GitHub-side state change. This is structurally indistinguishable from a
genuine stall given only data Fabrik already fetches, and is accepted as a documented,
non-closable-without-new-signal limitation rather than solved. In practice, this scenario is bounded
by `CIBackstopTimeout` (4h default) rather than the liveness dwell, so a suite in exactly this shape
still completes normally unless it exceeds the backstop too. If field experience shows this is a
real problem, the fix is a per-repo override or a longer default dwell, not more plumbing.

## Consequences

**Positive:**

- A CI suite of any duration under `CIBackstopTimeout` completes normally regardless of how long it
  takes, closing the #342 failure mode (R1). Verified end-to-end via
  `TestSettleAwaitingCIScan_342Repro_SlowButHealthyCI_DoesNotPause`.
- A CI failure is always classified as a failure and routed to CI-fix reinvocation, never
  misreported as a timeout that bypasses `MaxCiFixCycles`, regardless of elapsed time (R3).
- R3-class and mergeable-state-blocked liveness coverage is preserved with zero code change to
  `classifyCIFromMergeableState` and zero loss of existing test coverage.
- The absolute backstop still bounds worst-case per-poll cost and merge-train worker-goroutine
  lifetime, sized independently of and much larger than the liveness dwell (R5, R6).
- `CIWaitTimeout`'s semantic change is never silent — an ADR, a doc section, and a startup log line
  all state it explicitly (R8).
- #1408's resume/re-escalation model composes without any new code: every `pauseForCITimeout`
  caller after this change represents a genuine stall, so the existing episode-matching logic
  applies unchanged (R7).

**Negative / Trade-offs:**

- `classifyCIFromCheckRuns`'s pending-branch liveness dwell is unreachable via the real async settle
  pipeline today (`checkMergeabilityGate` claims `PRMergeUnsettled` first) — see "Architectural
  discovery" above. It remains correct, tested, and defensive code, but the practical #342 fix for
  pending CI is `CIBackstopTimeout`'s resize, not this dwell. A future change that lets
  `PRMergeUnsettled`-with-pending-checks reach `checkCIGate` would activate it for free; closing this
  gap deliberately (if ever needed) is out of scope for this issue (see Non-goals: no change to the
  conjunctive CI/review gate's semantics, #895).
- A resumed item (#1408) whose original pause was a genuine liveness stall, and which is *still*
  stalled on resume with no confirmed verdict, is not automatically re-escalated by the async gate —
  the same unreachability above means the handler chain can't re-derive "still stalled" for a
  pending-CI item the way it can for a still-failing one. It falls back to being silently
  re-blocked by the merge gate, re-evaluated every poll, until CI produces a verdict or the backstop
  (bounded by `labelAppliedAt`, independent of the resume) fires again. This is a pre-existing
  property of the claim-priority architecture, not a regression introduced here — the old
  elapsed-time guard had the identical unreachability — and is out of this issue's scope to close.
- A single long-running check with no interim transitions is indistinguishable from a genuine stall
  under ID/Status/Conclusion-only liveness detection (documented above, bounded by the backstop).

## Sibling Audit

This does not add a new instance of the "dedicated `board.Items`-sourced settle scan" pattern
(ADR-060/061/062/1097/1270/1387) — `settleAwaitingCIScan` remains the sole owner of
`fabrik:awaiting-ci`, unchanged by this issue except for which config value its existing backstop
guard compares against. It does not add a new pause function, comment format, or escalation-episode
model — #1408's existing machinery is reused verbatim. It generalizes ADR-058 D5's
"dwell anchored on signal-never-arrived, not the await window's own clock" pattern rather than
contradicting it; `checkAutoMergeConvergence`'s own stall detector needed no change as a result.

**References:** [ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md) (the backstop
this issue resizes and repoints), [ADR-933: Required Status Context
Config](933-required-status-context-config.md) (`classifyCIFromRequiredContexts`, the second verdict
site fixed here), [ADR-058: Merge Queue Integration](058-merge-queue-integration.md) D5 (the
dwell-anchored-on-never-arrived precedent this issue generalizes), [ADR-1408: CI-Gate Pause Comment
Reuse on Resume](1408-ci-gate-pause-comment-reuse-on-resume.md) (the recoverability model R7
composes with), `docs/state-machine.md` (the as-built gate lifecycle, updated alongside this ADR),
`verveguy/liminis-context-graph#342` / PR #344 (field evidence: anchor 2026-08-04T12:51:45Z, head
`4b0a7ac4` pushed 13:07:31Z, CI green 13:25:44Z, spurious pause 13:21:57Z).
