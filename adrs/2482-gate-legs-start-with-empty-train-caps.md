# ADR 2482: Gate legs start with empty train caps

## Status

Accepted. Builds on ADR 2349 (which it leaves unchanged) and the bed-engine ownership rules of ADR 2245.

## Context

Since #2349 (ADR 2349) the engine persists its merge-train caps — the runaway guard's trial window, per-member ejection counts and auto-repair attempts — to `.fabrik/state/engine-decisions/train-caps.json`. A gate leg switch (`TestSwitchTrainMode`) is a restart of the same bed, so the next leg's engine reloads the previous leg's caps.

On the 0.0.90 gate (engine `d6866128`, invocation `20261026T045615Z-e31de8`), `pat/on`'s `TestMergeTrainRunawayGuardPausesBatch` recorded 6 trials on `fabrik-test-beta`, which trips the guard on purpose. The bed then switched to `app/on/webhook`, where `TestCrossRepoSpawn`'s child reached Queued within the 60-minute window and the engine logged `runaway guard already tripped … pausing 1 Queued member(s)`. The child never landed and the test timed out after 3121 s. Whether this happens depends on leg order. It is a gate-isolation defect, not an engine defect.

## Decision

1. **The engine is unchanged.** Persistence across a real restart is the intended production behaviour (ADR 2349). A missing train-caps file is "absent" to the engine, so the gate removes the file and the next engine starts empty.
2. **Only `train-caps.json` is removed**, by `tests/e2e/bedstate.ClearTrainCaps` (`os.Remove`, never `RemoveAll`: a missing file is success, any other failure — a directory in its place, a permission error — is returned and fails the leg loudly). `usage-limit.json` is account state, not test state, and stays; `.corrupt` quarantine files are never loaded and are left alone. `bedstate` is an untagged, stdlib-only package (the `bedtimeout`/`bedevents` precedent) because `tests/gate` cannot import the `e2e`-tagged harness. A drift test pins both file names to `engine/decision_state.go`.
3. **The gate stops the bed, then clears, then runs the switch.** The file must go while no engine runs: a running engine rewrites it from memory on the next trial or ejection. `restartBed` therefore stops the bed's engine (`stopBedEngine`: the engine's own clean-stop escalation of ADR 1393 — a first SIGINT, a wait of `StopWait`, a second SIGINT as the force-quit, a wait of `KillGrace` — through the `Procs` seam), removes the file, and only then runs the unchanged `TestSwitchTrainMode` command, whose `StopFabrikTestBed` is a no-op on a stopped bed. The alternative of removing only inside `TestSwitchTrainMode` has the harness's robust stop but no `tests/gate` fake-commander test can observe the harness's Stop/Start, so the acceptance test could not be written literally. A PID that does not hold the bed's instance lock is a recycled one and is never signalled; an engine that survives both signals fails the leg (an `*ExitError`) and the switch is not run. `retryInconclusive` restarts through `restartBed`, so every restart starts clean. This changes who stops the bed on a leg switch, but the harness always stopped it at that same point.
4. **The harness also clears, as defence in depth.** `TestSwitchTrainMode` calls `ClearTrainCaps` between its Stop and Start, covering a direct run of the switch test; after the gate's clean it is a no-op.
5. **`gate reset` clears the file when no live engine holds the bed.** With a live engine the removal would be futile, and `reset` does not become a bed stopper (and refusing would break its GitHub cleanup), so it logs one warning and leaves the file. The preflight path always has a stopped bed. `ResetBeds --bed` and multi-bed runs act on each bed's own directory only.
6. **`TestMergeTrainRunawayGuardPausesBatch` cleans up after itself.** The four paused members never land, so nothing resets the engine's in-memory trial window; a harness-side `t.Cleanup` that stops the bed, removes the file and starts the bed clears memory and disk without an engine change. It is registered after the prerequisite checks (a skipped test does not restart the bed) and leaves the bed running with `.env`, auth and mode unchanged. The one extra restart is acceptable for an `exclusive` test, which already runs last in its leg.
7. **Only the guard test gets the per-test cleanup.** Ejection counts are keyed `owner/repo#N` and live tests create fresh issues, so a count cannot leak to another test; red-trial tests add a few trials, well under the cap, and had that same in-leg exposure before #2349. The leg-boundary clean covers them across legs. `bedstate` makes extending the cleanup a one-line change.
8. **Non-vacuity.** `Gate.trainCapsClearDisabledForTest` (per instance, copied to per-bed views by `newBedGate`) neutralises the leg-switch and reset clean; the test runs one assertion helper with it off (passes) and on (must fail).

## Consequences

- Every leg starts with empty trial windows, ejection counts and auto-repair attempts, whatever an earlier leg did. A resumed invocation (`--resume`) starts each leg it runs clean too.
- The usage-limit hold survives leg switches, as before.
- SC-001 and SC-005 (`pat/on` then `app/on/webhook` completing `TestCrossRepoSpawn`) can only be confirmed on a real gate run; CI proves the file removal, the ordering and the non-vacuity.
