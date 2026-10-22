# Live e2e wall-clock audit (#2417, ADR-2417)

Where the slowest live tests spend their time, what this change shortens, and what it
deliberately leaves alone. The machine-readable classification is
`tests/e2e/registry/wallclock.json` (joined into `gate report` as the `category`
column), so the next gate run prints this table with measured figures.

**Scope of the evidence.** The change was written by a pipeline worker with no bed and
no `.e2e-coverage/` archive. *Before* figures are the mean pass times quoted in the issue
(0.0.88 and 0.0.89 gate logs, 8 tests). Every other row is classified from the test source
and its README wall-clock row and marked *fill from `gate report`*. *After* figures are
projections with their arithmetic; the measured per-test and per-leg figures and the live
neutralisation runs are the maintainer's follow-up at the next release gate, not acceptance
for this issue.

Categories: **real work** (Claude workers, CI and merge-train Actions runs), **engine
timeout** (waited out on purpose), **negative window** (a polled "must not happen"),
**slack** (caps, margins, fixed sleeps that buy no evidence).

## Top ~20

| Test | Mean s (n) | Dominant | Where the time goes | Changed here |
|---|---|---|---|---|
| ExpectedReviewersDeclaredWaitsAndReprompts | 1952 (8) | engine timeout | Phase 1 at T, Phase 2 at a further T (T = review wait); ~2 min seed and detection | **T 15 → 10**; failure caps T+10 flat → T + poll margin |
| ConvergenceRace | 1883 (10) | real work | two overlapping 25-min `slow-gate` waits (defined in the test repos' `ci.yml`, outside this repo) | no |
| ConjunctiveCIReviewGate | 1872 (10) | real work | 25-min `slow-gate`; R1 window (2 min) runs inside it; R4 window (2 min) adds | unreachable timeout-fallback path and stale skip removed |
| ReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation | 1693 (10) | negative window | ~2 min activity, then a `T+10` min window armed *after* the reinvoke was seen | **re-anchored and ended at the escalation** (below) |
| CrossRepoSpawn | 1478 (16) | real work | parent + child full pipelines; Review waits on Pruefer | no |
| MergeTrainConflictBisectPrefixRerere | 1364 (3) | real work | ~11 trial CI cycles + one Claude conflict resolution | no |
| MergeTrainRunawayGuardPausesBatch | 1307 (3) | real work | six red trials to reach the runaway cap | no |
| MergeTrainBisectionEjectsPoisoner | 1018 (6) | real work | halving bisection, one CI cycle per step | no |
| CIFixReinvoke, CIFixReinvokeCycleLimit | fill | real work | forced-red CI, reinvoke worker, `MaxCiFixCycles` CI cycles; the 5/30-min waits are failure caps, not sleeps | no |
| CruiseFullPipeline, BaseBranchPipeline, SmokeSingleRepoFullPipeline | fill | real work | full pipelines; Review waits on Pruefer | no |
| QueuedMemberCommentEjection | fill | real work | slow-gate trial held open by the occupant member | no |
| LateCheckRunSuiteGate | fill | real work | late check run sleeps ~4 min by design | no |
| MergeTrainRestartSafety, MergeTrainQueuedDeeperThanBatchCap | fill | real work | trial cycles + exactly-once settle wait (already 3 bed polls) | no |
| AppSelfRecognitionBlockedCommentUpdatedInPlace | fill | engine timeout | waits out the dep-blocked cooldown (engine semantics) | no — separate issue if wanted |
| ExpectedReviewersFastAdvance | fill | negative window | 3-min window = 3 bed polls, armed at seed | no — already poll-derived |
| ReviewAuthorityYoloDoesNotBypassBlock | fill | negative window | 90 s window = 1.5 polls | no — already poll-derived |

The honest reading: most of the ~8 h per leg is real work. The idle, evidence-free time this
issue can reach is the review-timeout family, about two tests per leg.

## What changed and why it is still conclusive

### 1. Launch-injected bed timeouts (`tests/e2e/bedtimeout`)

Both launch sites (`tests/gate/bed.go` `BedStartCmd`, `tests/e2e/lifecycle.go`
`StartFabrikTestBed`) inject `FABRIK_REVIEW_WAIT_TIMEOUT=10` and
`FABRIK_CI_WAIT_TIMEOUT=30` from one definition; tests read `bedtimeout.ReviewWait()`.
**The launch value wins** over an operator `.env` (the process environment is never
overridden by `.env` loading), so no preflight-mismatch failure mode is added.

* **Review: 10 min** (engine default 15). Floor set by the slowest legitimate review wait on
  the bed: `TestReviewAuthorityCycleLimitPauses` (must hit the cycle-limit pause before the
  review timeout; the bed sets `FABRIK_MAX_REVIEW_CYCLES=3`), the Yolo test's 90 s window,
  and Pruefer's latency on the four full-pipeline tests (Phase 2 pauses at 2T = 20 min).
  This is the value to watch at the next gate; it is one constant to raise.
* **CI: pinned at 30, not shortened.** It is a liveness-stall dwell, not a bound on CI
  duration (ADR 1410), no live test waits it out, and a 25-min `slow-gate` with no
  transitions is exactly what it cannot tell from a stall. Shortening buys nothing and risks
  pausing the slow-gate items.

### 2. `ReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` negative window

* **Arming event:** the `fabrik:awaiting-review` application L, read from the issue events
  log (exact), not from the later reinvoke observation.
* **Property:** at the first gate evaluation after L+T the bug takes Phase 1 (`@mention` +
  `fabrik:bot-reprompted`); the fixed engine, whose `allBots` carries `&& authorityReason ==
  ""`, takes the human path at the *same* instant and logs `review wait timeout elapsed;
  pausing issue` (the item pauses).
* **Window:** ends at that positive observable (credited only once L+T has passed), so it
  proves a post-timeout evaluation ran without the bot path. A blind deadline could pass with
  zero post-timeout evaluations while the multi-minute review-reinvoke holds the worker.
  Failure ceiling: `L + T + 10 min reinvoke allowance + 3 × bed poll` (`bedtimeout.Window`).
  The 3 polls are: the engine reaching its first evaluation after T, the label/log being
  written, the harness observing it. It is reached only if the engine does nothing.
  `fabrik:bot-reprompted` fails the window immediately at any time.
* **If the observable never appears** the test **fails** at the ceiling: absence of
  `fabrik:bot-reprompted` with no proven post-timeout evaluation is not evidence. Only the
  engine's own `review wait timeout elapsed; pausing issue` log line is credited (a bare
  `fabrik:paused` could come from the review-cycle limit), and
  `bedtimeout.TestReviewTimeoutLogLineMatchesEngine` pins that text to `engine/reviews.go` so
  drift is caught in plain CI rather than at a gate.

### 3. Poll-derived caps elsewhere

`ExpectedReviewersDeclaredWaitsAndReprompts` phase caps
change from `T+10` flat to `T + bedWindowMargin()` (= 3 bed polls + 2 harness polls,
5 min at defaults). They are caps (`WaitForIssueLabel` returns on the label), so this
tightens failure latency rather than passing time.

### 4. Conjunctive: stale skip and unreachable fallback removed

The `reviewerToken != "" && T < 10` skip guarded Review's own gate; the item has been seeded
at Validate since #1992, so it never arises, and a skip counts against SC-006, so it is gone.
The no-token timeout-fallback branch needed T ≤ 5; with T fixed at 10 by the launch it could
never run (the test skipped instead), so the dead branch is removed and the test requires
`FABRIK_REVIEWER_TOKEN`. No coverage changes: that path already skipped at the former default 15.

## Poll seam (ADR-1978): evaluated, not adopted

The review-timeout gate is a wall-clock comparison (`time.Since(labelAppliedAt) < timeout`);
held polls cannot advance it, so the seam cannot shorten the review tests. The other negative
windows (FastAdvance 3 polls, Yolo 1.5 polls, Conjunctive R4 2 polls) are already
poll-derived and run in the parallel shared phase; making them `exclusive` would trade a few
minutes of window for serial scheduling. No registry classification changes.

## Projected before / after

| Test | Before | After (projected) | Arithmetic |
|---|---|---|---|
| ExpectedReviewersDeclaredWaitsAndReprompts | 1952 s | ~1350 s | 2×10 min + ~2.5 min = 22.5 min (was 2×15 + 2.5) |
| ReviewAuthorityDeclaredBot… | 1693 s | ~850 s | seed/reinvoke ~3 min after L, escalation at L+10+~1 min ≈ 14 min (was ~28) |
| ConjunctiveCIReviewGate | 1872 s | 1872 s | CI-bound; unchanged |
| **Per leg** | ~8 h | **−~24 min (≈5 %)** | −602 s −843 s per leg that runs both |

A full-matrix release runs these in each leg it selects (sparse matrix: baseline plus
sensitive cells), so the saving per release is the per-leg figure times the legs that include
them. Larger savings need real-work changes (`slow-gate` duration, trial CI) that are out of
scope and are recorded here as candidates for separate issues: the 25-min `slow-gate`
(`SLOW_CI_LONG_SECONDS` in the test repos' `ci.yml` and the mirrored `slowGateLongDuration`);
the dep-blocked cooldown wait.

## Non-vacuity

* Sim twin (property: the declared bot never defers the human escalation), run in this change:
  with the `&& authorityReason == ""` clause on `engine/reviews.go:328` removed,
  `go test ./tests/sim -run TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation` **fails**;
  restored, it and `TestExpectedReviewersDeclaredWaitsAndReprompts` pass.
* Window logic: `bedtimeout` unit tests drive a simulated bug (Phase 1 at the timeout → bug
  outcome), an early "escalation" (ignored before L+T), a silent engine (ends at the
  ceiling, not earlier) and an immediate bug.
* Launch contract: `TestBedStartCmdContracts` (gate half, including precedence over an
  operator export) and `tests/e2e/bedtimeout` source scans (harness half; no test may read
  the timeouts from the bed `.env`).

## Follow-up at the next release gate (maintainer)

1. Run the gate; fill the table above from `gate report` (category column is populated).
2. Live neutralisation of the declared-bot test (revert the `authorityReason` clause on a
   bed; the window must report the bug) and confirm the engine's escalation observable.
3. Watch the review-timeout-sensitive tests at T = 10: CycleLimitPauses, YoloDoesNotBypassBlock,
   and the four full-pipeline tests (Pruefer latency). Raise `bedtimeout.ReviewMinutes` if one loses.
