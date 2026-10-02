# ADR 1978: A bed-only poll hold/trigger seam for state-window live tests

## Status

Accepted (#1978). Builds on ADR-1449 (the inert-by-default, test-only engine seam precedent), ADR-1977 (the registry `exclusive` class and the serial, last exclusive phase this relies on), ADR-1974 (`awaitVisible`, which the test still uses for lag), ADR-1973 (the `Inconclusive` outcome that masked the race) and ADR-1833 (the batch-cap behaviour the converted test proves). **ADR-1454 is unchanged:** every live scenario still runs live; nothing here moves coverage to the sim.

## Context

Some live tests need the engine to take one decision in one specific poll, because their subject is a *state window*: `TestMergeTrainQueuedDeeperThanBatchCap` asserts on the engine seeing exactly 7 Queued members at once. It builds that window by removing `fabrik:paused` from 7 members — a multi-step write about one API round-trip wide. A free-running poll landing inside it sees only some of the members and the test reports "poll boundary straddled the unpause". #1973 downgraded this from a failure to `Inconclusive` with a bounded retry; each occurrence still cost a ~30–60 minute re-run.

Waiting for GitHub to catch up (#1974) cannot close it: the problem is not that a write is invisible, it is that the engine may look *between* two writes. Only controlling when the engine polls can.

## Decision

### 1. An env-enabled seam that gates the real `Run()` loop

`FABRIK_TEST_POLL_CONTROL` (`Config.PollControlFile`, read in `cmd/root.go`) names a control file. It is env-only — no flag, no YAML key, absent from the CLI help and from `docs/USER_GUIDE.md` — and the code and the `docs/state-machine.md` §7.8a seam note mark it test-only. Unset, `Engine.pollSeam` is nil, every hook is a nil check, and the engine starts no goroutine, touches no file and logs nothing extra.

Unlike the sim seam (`Engine.PollOnce`, ADR-1449), which bypasses `Run()` and so skips its preamble, this seam gates the real loop: the live test is *about* the real loop.

### 2. Hold means the whole poll, through two hooks

`Run()`'s `doPollCycle` closure is split into `pollCycle` (the old body, now returning the `PollBackoffResult`) and a wrapper that routes through `pollSeam.ordinary` when the seam exists. The startup poll, the ticker and the `wakeCh` wake all call `doPollCycle`, so one hook covers them — and the GraphQL-recovery self-wake, which only ever sends on `wakeCh`. No `select` is restructured, which is what keeps the unset path byte-identical (`Run()` has two duplicated `select` shapes and a startup poll outside the loop; touching them was the risk). `reconcileLoop`'s ticker body is extracted unchanged into `reconcileTick` and wrapped by `pollSeam.runUnlessHeld`, because a reconcile mutates the cache and a state window must not move.

A held cycle is **dropped, not queued**. Already-dispatched item and merge-train workers keep running: hold gates new cycles, not work already started. A hold never blocks `ctx.Done()` — the held path returns immediately and `Run()`'s `select` is untouched, so the drain is not delayed.

### 3. A trigger is the ordinary poll, with an honest result

A trigger runs `Run()`'s own `pollCycle` — `PollWithBackoff` and the ticker reset — so backoff and rate-limit bookkeeping are real, not a reduced path. `PollBackoffResult` gains `Ran`, true only when `poll()` executed. A call that hit the 500 ms minimum-poll-interval floor is retried a bounded number of times; anything else that ran no poll (the REST hard gate, a persistent floor) is reported `blocked` and a `poll()` error `error`. The harness maps `blocked` to `Inconclusive` and `error` to `Fatalf`: a test must never assert on a non-poll.

### 4. Two single-writer files, atomic rename

`internal/pollctl` (shared by engine and harness, because the engine must not import `tests/` and the wire format must not be duplicated) defines a request file written by the harness — `hold`, `hold_until`, `hold_gen`, `trigger_seq` — and an ack file written by the engine — `held`, applied `hold_gen`, `done_seq`, `outcome`, `detail`. Each has exactly one writer and is replaced by rename, so there is no read-modify-write race and a reader never sees a torn file; a missing or unparseable file reads as the zero value. Generations and sequences increase monotonically, so a waiter can tell its own request was applied rather than a stale ack. Rejected: one shared file (a harness/engine write race), signals (not portable to the gate's launch, not observable from the harness), and a `PollOnce`-style bypass (that is the sim seam).

A watcher goroutine, started only when the seam is enabled, polls the request file every 200 ms. A stat on every poll tick would give a held ticker no natural check cadence and a trigger no prompt start.

### 5. Serialization is structural

`pollMu` is taken by an ordinary poll, a triggered poll and the *application of a hold*. Two polls therefore never overlap; a trigger during a running poll waits for it and never starts a concurrent one; and the hold ack is only written once no poll is running and none will start, which is the quiescence guarantee `HoldPolls` hands the test. `reconMu` does the same for reconcile ticks (always taken after `pollMu` when both are needed).

### 6. Lifecycle: start released, bound the hold

The engine deletes any stale request at startup and writes a fresh released ack. The ack's existence is the harness's "the seam is live on this bed" signal (`HoldPolls` fails with a clear message on a bed started without it), and it also fixes a startup race found while writing the engine tests: the startup board check fetches *before* the seam starts, so a request written then would otherwise be deleted by the clear. A hold therefore never survives a restart, and the harness does not assume it does.

`hold_until` bounds a hold: the harness stamps now + `pollctl.MaxHold` (10 minutes — typically far longer than the unpause, wait and trigger window that is the only part held, but *not* a guaranteed upper bound: BatchCap's visibility waits are each allowed `awaitSeedTimeout`, also 10 minutes), and past it the engine releases and logs once. Because the request file still says `hold` after that self-release, `Trigger` does not trust it: it reads the engine's ack (`held`) and the `hold_until` deadline and, if the hold has lapsed, ends the test `Inconclusive` instead of triggering — free-running polls may have seen part of the window, so a poll then would not observe it whole. `t.Cleanup` cannot run in a hard-killed harness; the engine-side bound is what stops the next run's preflight hanging on a held bed.

### 7. Enabled at both launch sites

The gate's `BedStartCmd` (`tests/gate/bed.go`) and the harness's `StartFabrikTestBed` (`tests/e2e/lifecycle.go`) both append `pollctl.Env(bedDir)`. They are independent launches (the runner is untagged and cannot import the tagged package) and a restart through the harness path — `TestSwitchTrainMode`, restart-safety, cold-cache — would otherwise silently drop the seam. `TestBedStartCmdContracts` pins the gate half and a structural test in `tests/e2e/pollhold` pins that both sources call `pollctl.Env`. Enabled but released, the bed is a normal free-running bed, so the shared and exclusive phases still share one bed start.

### 8. The harness helpers and who may use them

`HoldPolls`, `TriggerPoll` and `ReleasePolls` (`tests/e2e/poll_control.go`) are thin wrappers over `tests/e2e/pollhold`, an untagged package so its protocol, exclusivity rule and bounded waits run in plain `go test ./...` (the `awaitvisible`/`inconclusive`/`registry` precedent: CI only compiles the tagged harness). Holding polls stops dispatch, catch-up, settle scans and reconcile for the whole bed, so exclusivity is enforced three ways: a runtime registry lookup in `HoldPolls` (a subtest resolves to its top-level name), a static scan (`ScanPollSeamCallers`/`CheckPollSeamCallers`, modelled on `CheckBedLifecycleCallers`) in plain `go test`, and the existing exclusive ↔ `t.Parallel()` consistency check. `HoldPolls` registers `ReleasePolls` with `t.Cleanup` *before* waiting for the ack, so a hold the engine never acknowledges is still released; release reports with `Errorf`, never `Fatalf`, so it is safe from a cleanup. `E2E_POLL_SEAM=off` turns all three into logged no-ops so the pre-seam behaviour — the straddle, as `Inconclusive` — can be reproduced; the gate never sets it.

### 9. Use only for state windows; hold after seeding

Live e2e is the one layer that runs with real timing, and free-running polls are part of what it tests, so pipeline, convergence and gate tests stay free-running. The only user is `TestMergeTrainQueuedDeeperThanBatchCap`, now registry class `exclusive` (replacing `default_base_train`; the two are mutually exclusive) with an `exclusive_reason`. Its flow: seed the 7 members *paused and free-running*, `HoldPolls`, unpause all 7, wait until the removal is visible on the REST issue read (`AwaitLabelGone`, added to the #1974 family) and every board item is listed, `TriggerPoll`, assert A1 on that poll's batch snapshot, `ReleasePolls`, then run A2/A3 free-running as before.

This deliberately differs from "hold, seed and unpause". Paused members are inert to the batch and the engine must poll and hydrate them anyway; holding across the slow seed (up to 10 minutes per `awaitVisible`) would starve the bed for no benefit. Only the unpause is the window.

### 10. The `Inconclusive` guard stays

The seam removes the poll-boundary straddle only. In poll mode the engine's cache catches up only inside `poll()` (probe, then deep-fetch), and the harness cannot read that cache; "visible" can only mean visible on the GitHub surfaces the probe reads. The triggered poll can therefore still see fewer than 7 if the probe's `EffectiveUpdatedAt` lags a label change. The guard becomes a defensive check that now covers residual read lag only, and still ends `Inconclusive`. With the seam neutralised (`E2E_POLL_SEAM=off`) the old straddle is reproduced as `Inconclusive`.

## Consequences

- A held bed starves everything else (dispatch, settle scans, reconcile). Mitigated by exclusive-only enforcement, `t.Cleanup` release, a short hold window (unpause, wait, trigger) and the engine-side `hold_until` bound.
- A webhook-enabled bed works too: the cache can update during a hold through the webhook manager, which is not held, and a wake sent into a held loop is simply dropped.
- Moving BatchCap from `default_base_train` to `exclusive` runs it in the last phase. The ledger is keyed by per-test source hash, so the changed body gets a fresh key; the exclusive phase is serial and no other exclusive test enqueues on `RepoAlpha/main` (runaway uses RepoBeta, the two-base test uses throwaway bases).
- Another state-window test can adopt the seam by being registered `exclusive`; nothing else needs to change. Candidates found later are follow-ups, not part of this ADR.
- The seam is a second, parallel control surface on the poll loop. Keeping it inert when unset rests on the nil-check structure and on the seam-unset unit test (`TestRun_PollSeamUnset_LoopUnchanged`), which drives the real `Run()`.
