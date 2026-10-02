# tests/gate — the live e2e gate runner

The Go program behind `scripts/e2e/run.sh` and `scripts/e2e/reset.sh` (#1994,
[ADR-1994](../../adrs/1994-go-gate-runner.md)). Both scripts are now thin shims that
build `./tests/gate/cmd/gate` and `exec` it (`gate run …` / `gate reset …`).

It orchestrates the release gate: operational preconditions → the free pre-gate (sim +
wire-contract tests) → bed preflight → optional `--clean` reset → bed start → the auth × train
legs, each a bed restart (`TestSwitchTrainMode`) plus `go test -tags=e2e -json` over
`./tests/e2e/...`. What each step does and why is documented in
[`tests/e2e/README.md`](../e2e/README.md); this file is the porting reference.

- Compiles **without** the `e2e` build tag, so `go test -race ./...` builds and tests it on
  every PR. The live tests it drives stay tag-gated.
- Lives at `tests/gate`, **not** under `tests/e2e`: the live legs run `./tests/e2e/...`, and
  anything beneath that pattern would be swept into every leg and its outcome report.
- Not part of the `fabrik` binary. Stdlib plus `internal/sessionreap` (#1989) and
  `tests/e2e/registry` (#1933).
- Its tests need no bed, network or `gh` login: a fake `Commander`, recorded `go test -json`
  streams, real `git` on scratch repos (`skipIfNoGit`), and real child processes for the
  hang-protection cases.

## Where the follow-on chain plugs in (R5)

The seams below are all in use: #1972 (ledger), #1973 (INCONCLUSIVE), #1974 (probes),
#1975 (the sparse plan), #1976 (multi-bed) and #1977 (the phase seam) plug into them.

| Seam | Where | Used by |
|---|---|---|
| `Cell` (auth, train, parallel, args) and `PlanCells` (`PlanInput.Sparse` selects the sparse plan) | `schedule.go` | **#1975's sparse matrix** (below), #1976's bed assignment |
| `Scheduler` interface on `Gate` (`SerialScheduler` for one bed, `MultiBedScheduler` for two or more) | `schedule.go`, `multibed.go` | **#1976 multi-bed** (below) |
| `Phase`, `PlanPhases`, `retryPhases` — a cell's shared / default-base-train / exclusive `go test` invocations | `phases.go` | **#1977's two-phase leg** (below) |
| `LegResult` (cell, exit code, log path, decoded `[]Event`, budget before/after) delivered to `Gate.OnLeg` | `leg.go` | other observers. **#1972's ledger does not use it** — it records from `suiteWriter`'s event sink (`ledger_recorder.go`), because `OnLeg` never fires for a killed, RUN INVALID, watchdog or restart-failed leg |
| `Classification` (pass/fail/skip/**inconclusive**/running/never-started) | `events.go` | #1973 added INCONCLUSIVE — see "INCONCLUSIVE and bounded retry" below |
| `Ledger`, `Evaluator`, `Report` and `RequiredTests`/`ResumeCells` over `PlanCells` output | `ledger*.go`, `coverage.go`, `resume.go` | #1972; #1975's sparse plan changed the required set with no change here — `Report` only gained the `Matrix`/`FullMatrixPairs` fields for the summary |
| `[]Preflight` (`Gate.Preflights`, ordered, each returns an `*ExitError`) | `gate.go` | #1974's host-load probe (`host-load-probe`) |
| `[]Preflight` (`Gate.LivePreflights`, run **after** the pre-gate, before the bed is built) | `gate.go` | #1974's board-lag probe — a live write, so it cannot precede the pre-gate (ADR-1454) |
| `[]Preflight` again: `bed-topology` (static) and `bed-identity` (live) | `beds.go`, `identity.go` | #1976 |
| `Commander` (`Run`/`Start`), `Gate.Env`, `Sleep`, `Now`, `ProcCwd` | `exec.go`, `gate.go` | every test; #1976's per-bed views |
| `IdentityOps` (`ResolveLogin`, `Budget`, `MintAppToken`) on `Gate.Identity` | `identity.go` | #1976's identity scheduling and budget lines; `nil` (a hand-built `Gate`) disables both |

## The sparse plan and `E2E_MATRIX` (#1975, ADR-1975)

`PlanCells` has two modes. With `PlanInput.Sparse == nil` it is the full auth × train matrix
above, byte-for-byte. With `Sparse` set (`SparseInput{Live, Entries}` — the live set and each
test's registry entry) it plans four logical cells in run order, grouped by auth:

| Cell | Selected tests |
|---|---|
| `app/on` (baseline) | every live test (the caller's arguments pass through; the exclusive runaway-guard test is the last phase of this cell, #1977) |
| `app/off` | `train: sensitive` |
| `pat/on` | `auth: sensitive` |
| `pat/off` | sensitive on both axes |

A non-baseline cell drops the pairs a test's `skip_ok_legs` matches, selects the rest with one
anchored `-run ^(…)$` (`narrowArgs`, keeping other passthrough arguments and any caller subtest
filter), and is dropped
entirely — bed restart included — when nothing is left. A caller `-run` is intersected with each
cell's selection; `E2E_AUTH_MODE` / `E2E_TRAIN_MODE` filter the finished plan, so a `pat`-only or
`off`-only invocation omits the baseline and is a partial run. A forced train mode other than
`on`/`off` falls through to the full plan (the restart step rejects it). A live test with no
registry entry is planned in every cell.

`Config.Matrix` (`E2E_MATRIX`, `sparse` default, `full`, anything else an error → `ExitUsage`; the
zero value means full so hand-built test `Config`s keep the full shapes) decides whether `Gate`
passes `Sparse`. `Gate.buildPlanInput` is the one place a `PlanInput` is built, shared by `gate run`
and `gate coverage`, and the registry is loaded for the plan whether or not the ledger is enabled.
The required set is still `RequiredTests(PlanCells(…))` with no caller arguments — the plan and the
set cannot diverge. `gate run` prints `== E2E_MATRIX=<mode>: N cell(s): …` before the first leg, and
the coverage summary ends with `required set (E2E_MATRIX=<mode>): N (test, leg) pairs of M in the
full 2×2` (also on the release-notes line). `sparse_test.go` covers the plan, the filters, the
the gate-level agreement of plan and required set, and `--resume` over
a sparse plan, all with the fake `Commander`.

## `run.sh` function → Go

All 29 functions of the bash `run.sh`, plus its top-level code.

| `run.sh` | Go |
|---|---|
| top level: `TEST_BED`, `ENGINE_LOG`, `BED_TOKEN`, `PRUEFER_DIR`, exit-code constants, `TIMEOUT`/`PARALLEL`/`PARALLEL_ON`/`GH_API_TIMEOUT`/`POST_SUITE_*`/`STALL_*`/`BED_POLL_SECONDS` | `config.go`: `Config`, `LoadConfig`, `EnvFileValue`, the `Exit*` constants |
| `resolve_auth_modes` | `authmode.go`: `ResolveAuthModes` |
| `auth_modes_include` | `authmode.go`: `authModesInclude` |
| `env_file_value` | `config.go`: `envFileLastValue` (the last-wins, unquoting variant; `EnvFileValue` is the first-wins one `BED_TOKEN` used) |
| `auth_mode_problems` | `authmode.go`: `AuthModeProblems` |
| `check_auth_mode_preconditions` | `authmode.go`: `Gate.CheckAuthModePreconditions` |
| `run_reaped` | `exec.go`: `OSExec.Run` with `Cmd.Session` (session reap through `internal/sessionreap`) |
| `disarm_deadline_signal` | gone — `OSExec.Run` owns its deadline timer and tracker |
| `with_timeout` | `exec.go`: `Cmd.Timeout` / `Result.TimedOut`; the `with_timeout: command exceeded Ns, killed:` line is produced in `budget.go` |
| `_report_gh_probe_failure` | `budget.go`: `Gate.reportProbeFailure` |
| `resolve_pid_cwd` | `consumers.go`: `ResolvePIDCwd` (`/proc` on Linux, `lsof -Fn` elsewhere) |
| `discover_fabrik_process_dirs` | `consumers.go`: `Gate.DiscoverFabrikProcessDirs` |
| `find_competing_token_consumers` | `consumers.go`: `FindCompetingTokenConsumers` |
| `check_competing_token_consumers` | `consumers.go`: `Gate.CheckCompetingTokenConsumers` |
| `check_reviewer_reachable` | `consumers.go`: `Gate.CheckReviewerReachable` |
| `run_pregate` | `pregate.go`: `Gate.RunPregate` |
| `preflight_bed` | `bed.go`: `Gate.PreflightBed` |
| `stop_bed_instance` | `bed.go`: `Gate.StopBedInstance` |
| `write_isolated_bed_gitconfig` | `bed.go`: `Gate.WriteIsolatedBedGitconfig` |
| `preflight_bed_start` | `bed.go`: `Gate.StartBed` and `Gate.BedStartCmd` |
| `prepare_bed_and_reset` | `bed.go`: `Gate.PrepareBedAndReset` |
| `drain_output_consumer` | `exec.go`: `Cmd.WaitDelay` / `Result.PipeWedged`; the warning is printed in `leg.go` |
| `graphql_budget_remaining` | `budget.go`: `Gate.GraphQLBudgetRemaining` |
| `report_test_outcomes` | `events.go`: `Classify` + `Classification.Report` |
| `report_test_timings` | `events.go`: `Timings` + `FormatTimings` (header printed in `leg.go`) |
| `last_completed_test_name` | `events.go`: `LastCompletedTest` |
| `detect_rate_limit_backoff` | `backoff.go`: `DetectRateLimitBackoff` |
| `stop_post_suite_watchdog` | gone — the watchdog is a timer inside `RunLeg`, torn down by `defer` |
| `_post_suite_watchdog_signal` | `leg.go`: the `timer.C` branch of `RunLeg`'s `select` |
| `switch_and_run` | `leg.go`: `Gate.RunLeg`, `postSuiteTail`, `watchStall`, `suiteWriter` |
| `print_sim_parity_summary` | `parity.go`: `Gate.PrintSimParitySummary` (reads the registry through `tests/e2e/registry`) |
| dispatch guard: precondition order, `TRAIN_ISOLATED_RE`, `caller_has_run`, the auth × train loop | `gate.go`: `Gate.Run`; `schedule.go`: `PlanCells`, `SerialScheduler`; `args.go`: `ParseRunArgs`, `HasRunFlag`. `TRAIN_ISOLATED_RE` and its separate cell are gone (#1977): the runaway-guard test is `exclusive` in the registry and the last phase of its cell (`phases.go`) |
| `reset.sh` (`gh_`, `close_open_prs_in`, `close_open_issues_in`, `delete_fabrik_branches_in`, `resolve_project_id`, `drain_board`, `--worktrees`) | `reset.go`: `Gate.Reset` (`gate reset [--worktrees]`) |

## Bash test case → Go test

The nine `scripts/e2e/*_test.sh` files (about 80 assertions) are deleted; their CI steps are
gone, and the Go tests run in the existing `go test -race ./...` step. Cases that tested a bash
workaround rather than gate behaviour have no counterpart, and are listed with the reason.

| Bash test | Case | Go test |
|---|---|---|
| `auth_mode_check_test.sh` | `resolve_auth_modes` ×7 (unset, pat, app, APP, trimmed, internal whitespace, invalid) | `TestResolveAuthModes` |
| | `auth_mode_problems` ×8 (neutral, missing id/installation id, full identity, unreadable key, key irrelevant for pat-only, commented-out key, config.yaml key) | `TestAuthModeProblems` |
| | `check_competing_token_consumers` with no pat leg: warns, not refused | `TestCheckCompetingTokenConsumers/an app-only run…` |
| | …with a pat leg: refused, exit 7, names the pid | `TestCheckCompetingTokenConsumers/a competitor with a pat leg…` |
| `token_consumer_check_test.sh` | matching token; mismatched; the bed's own dir excluded; no `.env`; empty list; mixed | `TestFindCompetingTokenConsumers` (+ a symlink-to-the-bed case) |
| | `E2E_SKIP_TOKEN_CHECK`; empty `BED_TOKEN` degrades; orchestrator match → exit 7 naming pid and dir | `TestCheckCompetingTokenConsumers` |
| `reviewer_reachable_check_test.sh` | live pid; missing lock; dead pid; skip env; missing `PRUEFER_DIR` warns; `-run`; `--run=` | `TestCheckReviewerReachable` (+ garbled lock, `-run=`, `--run X`) |
| `pregate_test.sh` | skip; both layers pass; sim fails (exit 5, one call); matching SHA skips; mismatched runs full; matching SHA + dirty tree runs full; allowlisted dirt skips; non-allowlisted dirt runs full | `TestPregate` (+ wire-contract failure, RE2 against the exact `cut-release.sh` regex, bad-regex fail-closed) |
| | structural: `run_pregate` precedes `prepare_bed_and_reset` in the dispatch guard (a grep of the script) | `TestRunExitCode5PregateFailureSpendsNothing`, `TestRunOrderAndCleanHandling` (asserted by running it, not by grepping) |
| `parity_summary_test.sh` | counts; prints when the pre-gate is skipped; missing registry; bad registry; real registry well-formed; called before and outside the pre-gate | `TestPrintSimParitySummary`, `TestSimParityIsInformationalAndOutsideThePregate`, `TestRunOrderAndCleanHandling` |
| | **dropped:** no `jq` on `PATH` | Go needs no `jq` |
| `backoff_detection_test.sh` | no activity; the per-poll "is low" line alone; activation line; activation only after a mid-leg truncation (#1547); the byte-offset neutralisation twin | `TestDetectRateLimitBackoff` (+ `TestReaderContainsAcrossChunkBoundaries`) |
| `bed_gitconfig_isolation_test.sh` | credential kept, `insteadOf` and `user.*` dropped; no credentials gives an empty file | `TestWriteIsolatedBedGitconfig` |
| `preflight_bed_ref_test.sh` | single-branch refspec fixture; never-fetched branch; force-pushed branch; bogus ref → exit 4 + wrapped message; unset → `origin/main` | `TestPreflightBedRefResolution` (+ dirty tracked files, not-a-checkout) |
| `hang_hardening_test.sh` | `with_timeout` kills at its deadline; non-vacuous (no timeout keeps running); passes exit 0 and 1 through; output captured under a deadline; group-kills background children | `TestExecTimeoutKillsAHangingCommand`, `TestExecWithoutATimeoutKeepsRunning`, `TestExecPassesExitCodesThroughUnmodified`, `TestExecCapturesOutputUnderADeadline`, `TestExecTimeoutReapsBackgroundChildren` |
| | `last_completed_test_name` ×4 (none, realistic stream, subtests excluded, skip counts) | `TestLastCompletedTest` |
| | watchdog external-signal branch and fired branch both tear down the suite, consumer and watcher | `TestLegPostSuiteWatchdogFires`, `TestLegWatchdogTwinHangIsNotCutShortWithoutIt`, `TestExecCancelReapsTheSession`, `TestSignalReapsTheChildTree` (the real binary, INT and TERM) |
| | `_report_gh_probe_failure` ×2 (relays the timeout diagnostic; silent when empty) | `TestLegBudgetProbeFailuresAreWarningsNeverGates` |
| | bounded drain against a wedged consumer holding the pipe (#1694); healthy drain not slowed | `TestExecBoundsAWedgedOutputPipe` (success and failing exit), `TestExecWedgeReallyBlocksWithoutABound` (the non-vacuous premise), `TestExecHealthyDrainIsPromptAndNotWedged`, `TestLegWedgedPipeWarnsAndContinues` |
| | **dropped:** `$(with_timeout …)` command substitution does not enforce the deadline | a bash job-control pitfall; there is no equivalent in Go |
| | **dropped:** the deadline-watcher job pattern fires / does not fire | the watcher was a backgrounded `sleep`; replaced by a `time.Timer` (covered by the watchdog tests above) |
| `pregate_test.sh` | the PATH-shadowed fake `go` | replaced by the fake `Commander` in `helpers_test.go` |

New coverage with no bash counterpart: the leg executor's happy path, argv/env contract,
failure classification, timeout teardown and RUN INVALID (`leg_test.go`); every exit code end
to end (`gate_test.go`); the reset port and its pagination bounds (`reset_test.go`); the leg
shape table (`schedule_test.go`); the golden reports (`events_test.go`).

## Coverage ledger (#1972, ADR-1972)

The gate is coverage-based: outcomes are recorded per leg under the engine SHA and `--resume`
runs only what is missing. `tests/e2e/README.md`'s "Coverage ledger, `--resume`, and suspending
a run" is the operator's view; this is the map.

| File | What |
|---|---|
| `ledger.go` | `Ledger`: layout, append-only fsynced JSONL, tolerant reader (torn tail skipped, unknown version = corrupt leg), `void`, `invocations.jsonl`, `Snapshot.Covered` |
| `ledger_recorder.go` | `legRecorder`: terminal-event recording from the stream, skip message and `#N` extraction |
| `ledger_hash.go` | `HashTests`: per-test source hash over the same-package reference closure |
| `ledger_drift.go` | `Gate.DriftCheck` (R2) against the engine SHA |
| `ledger_skips.go` | `ClassifySkip` (known / structural / missing), `gh issue view` via the `Commander` |
| `ledger_wire.go` | coverage inputs, `openCoverage`, preflight tee, pre-gate record |
| `resume.go` | `SelectedTests` (Go `-run`/`-skip` rules), `RequiredTests`, `ResumeCells`, regex builder + size guard |
| `coverage.go`, `coverage_cmd.go` | `Evaluator`, `Report`, `gate coverage` (exit 0 / `ExitCoverageIncomplete` 8) |
| `archive.go`, `loadavg*.go` | per-leg archive, engine-log segment sampler, bed-config hash, load average, retention |
| `report.go` | `gate report` (#1992): measured per-test runtime from the archive's `go-test.json`, joined with the registry's `entry`/`traversal` fields; `--baseline` prints the before/after delta. Model quota is not recorded anywhere, so it reports a labelled proxy (pipeline stages not driven) |

Deltas this adds to the port: `ParseRunArgs` consumes `--clean` **and** `--resume` as leading flags
in either order; exit code 8 exists (only under `--resume` and `coverage`); the per-leg `-json` log
moves into the archive when the ledger is on (the `$TMPDIR` name remains when it is off; a multi-phase leg suffixes each phase, `go-test.shared.json`); and `RunLeg` starts an archive/sampler before the restart step.

## The two-phase leg (#1977, ADR-1977)

One cell is still one bed restart (`TestSwitchTrainMode`), but the `go test` after it is split by
the registry's isolation class (`exclusive`, `default_base_train`, else shared) into up to three
invocations run in this order: **shared** at the cell's `-parallel` (`E2E_PARALLEL` /
`E2E_PARALLEL_ON`, defaults 4 / 2 — unchanged from before #1977; 8 / 4 is the value to try once the bed's `max_concurrent` is 10 and a measured leg supports it; INCONCLUSIVE-retry spend is its own `retry` entry in `phases.json`), then **default-base-train** at `-parallel 1`, then **exclusive** at
`-parallel 1`. Exclusive last means a shared test never inherits what an exclusive one left, with no
extra bed restart; default-base-train follows shared because shared yolo tests also enqueue on
`RepoAlpha/main` under train `on`.

| Piece | Where | Notes |
|---|---|---|
| `PlanPhases(cell, live, classes)` | `phases.go` | pure; runs *after* every selection mechanism (caller `-run`/`-skip`, `--resume` rewrite, sparse narrowing) via `SelectedTests`, then one `narrowArgs` per phase (a caller subtest suffix survives). Excludes `TestSwitchTrainMode` (the leg's own restart step); an unknown test is exclusive; nil `classes` is the single undivided phase |
| `RunLeg` | `leg.go` | phases run in order into **one** `legRecorder` (ledger keys and the required set are unchanged; a killed leg keeps what finished). Every phase runs even after a red one, the first non-zero exit is the leg's; it stops early only on cancel, an incomplete phase (timeout kill) or the rate-limit backoff. RUN INVALID, the watchdog and the budget report stay once per leg in `postSuiteTail` |
| `retryPhases` | `phases.go` | a #1973 retry regroups the inconclusive set through the same phase builder, so a retried exclusive test is re-run serially and a retried shared test at the shared `-parallel`, in phase order |
| `Gate.liveTests`/`isolation` | `gate.go`, `ledger_wire.go` | set by `setPhaseInputs` from the registry the plan already loads; unavailable → loud warning and one undivided `go test` per leg |
| `PeakConcurrent`, `PhaseStat`, `LegSummary` | `events.go`, `metrics.go` | per-phase wall-clock, peak concurrent tests (from stream order: `run`/`cont` +1, `pause`/terminal −1), GraphQL spend (a budget probe at each phase boundary) and the host's peak load; printed after the suite and archived as `phases.json` |
| load sampler | `archive.go` | `legArchive` samples the load average on the archive cadence; `load.json` gains `peak_1m` and `samples` |
| `BedMaxConcurrent`, `noteBedConcurrency` | `bedconcurrency.go` | reads the bed's effective `max_concurrent` (`.env` `FABRIK_MAX_CONCURRENT`, then `config.yaml`, then 5), archives it as `bed-concurrency.json` and warns — never blocks — when it is below the widest `-parallel` |

Tests (`phases_test.go`, fake `Commander`, no bed or network): phase order and the "shared never
follows a serial phase" rule, selection by `-run` / `--resume` rewrite / subtest suffix, retry
classification, first-non-zero-exit, early stop on an incomplete phase, killed-leg recording, the
summary and peak-concurrent derivations, the load sampler and the `max_concurrent` precedence.

## INCONCLUSIVE and bounded retry (#1973, ADR-1973)

`tests/e2e/README.md`'s "The INCONCLUSIVE outcome and bounded retry" is the operator's view and
the guard audit; this is the map.

| File | What |
|---|---|
| `tests/e2e/inconclusive` (untagged) | the marker contract: `Marker`, `Message`, `IsMarked` (prefix match only), shared by the tagged live tests and this package |
| `events.go` | `Classification.Inconclusive`; `isInconclusiveSkip`, the **one** predicate shared by `Classify` and the recorder; the report line appears only when non-empty |
| `ledger_recorder.go` | a marked skip is recorded `OutcomeInconclusive` (reason in `SkipMsg`), never `SKIP` |
| `retry.go` | `retryLogPath`, `mergeAttempts` (last attempt wins per test), `summarizeRetries`, the per-leg summary and `#1974` warning |
| `leg.go` | `runSuiteAttempt` (one `go test -json` invocation: log, stall watcher, recorder), `retryInconclusive` (the bounded in-leg loop and its stop conditions), the tail's `anyFileContains` timeout scan over every attempt's log |
| `gate.go` | `leftInconclusive` / `withLeftInconclusive`: a clean run that left inconclusives exits `ExitCoverageIncomplete` (8) |
| `config.go` | `Matrix` (`E2E_MATRIX`: `sparse` default, `full`, else `ExitUsage`; zero value = full), `InconclusiveRetries` (`E2E_INCONCLUSIVE_RETRIES`, default 2, 0 disables), `InconclusiveWarn` (`E2E_INCONCLUSIVE_WARN`, default 3) |
| `pregate_signature.go`, `pregate.go` | `crashScanner`/`IsTSanForkCrash` (streamed, per-line, vetoed by any panic / `fatal error:` / `WARNING: DATA RACE` / goroutine dump) and `runPregateStep`'s one-shot retry |
| `ledger_wire.go` | `pregateRetryNote` → `pregate/<head>.retries.jsonl`; `retried`/`load_avg` on the pass record; the pre-gate line in the coverage summary |

Deltas this adds: the suite step of `RunLeg` is now `runSuiteAttempt` and may run more than once
per leg (only for the leg's own inconclusive tests, before the post-suite watchdog and the RUN
INVALID scan); a clean run can now exit `8` without `--resume`; `RunPregate` captures (scans, never
buffers) its steps' output. The `3/4/5/6/7` exit-code contract of `scripts/cut-release.sh` is
unchanged. `testdata/pregate/` holds **synthetic** crash logs (see its README) — add a real one if
a crashed gate run is ever captured.

## Environment probes (#1974, ADR-1974)

Two probes run before live budget is spent. `tests/e2e/README.md` has the knobs; this is the map.

| File | What |
|---|---|
| `probes.go` | `ProbeHostLoad`, `ProbeBoardLag`, the shared bounded `reprobe` loop, `parseOrphans`/`listOrphans`, `lagBoard` (draft-item add / listing / delete / leftover sweep), `ProbeReport` and `writeProbes` |
| `gate.go` | `DefaultPreflights` gains `host-load-probe` (host-only, **before** the pre-gate); `Gate.LivePreflights`/`DefaultLivePreflights` hold `board-lag-probe`, run in `run()` **after** `RunPregate` and before `PrepareBedAndReset` |
| `archive.go` | `beginArchive` writes `probes.json` beside `load.json` in every leg's archive directory |
| `config.go` | `LagProbeThreshold`, `LoadProbeFactor`, `LoadProbeThreshold`, `ProbeWaitMax`, `ProbeInterval`, `LagPollInterval`; `floatEnv` |
| `reset.go` | `resolveProjectNodeID`, extracted so `Reset` and the lag probe name the same board |

* **Load probe.** Reuses `Gate.loadAvg` (the reading the leg archive already takes — no second parser)
  against `E2E_LOAD_PROBE_THRESHOLD`, else `E2E_LOAD_PROBE_FACTOR` × CPU count. It also lists orphaned
  `sim.test` / `e2e.test` / `fabrik.test` processes (`ppid` 1, from `ps -axo pid=,ppid=,comm=`) with
  their working directory (`ResolvePIDCwd`; unresolvable → `unknown`, never an error), so it is clear
  which tree they came from. The list is informational and never gates; Linux orphans re-parented to a
  per-user subreaper, a truncated `comm` or a missing `lsof` can hide one.
* **Lag probe.** Adds a temporary **draft** project item (`addProjectV2DraftIssue`, title prefix
  `e2e-lag-probe-`) to the bed's own board, polls the same `projectV2.items` listing until it appears,
  and removes it again on success, timeout, error **and cancel** (on a context detached from the
  cancelled one). A draft creates no repository issue, so it cannot trip the bed's stale-Queued-member
  checks. A leftover from a SIGKILL mid-probe is swept by title prefix at the start of the next run.
  Every `gh` call is `Session`, `Timeout: GHAPITimeout`, scoped to the bed token (the `budget.go`
  routing shape).
* **Waiting.** A reading above its threshold pauses `E2E_PROBE_INTERVAL` and re-probes, at most
  `E2E_PROBE_WAIT_MAX` / interval times (counted in attempts, so the bound is deterministic). Still
  above at the bound → preflight fails with exit **4** naming the probe. A probe that cannot take a
  reading (no load source, no bed token, add/list failing) reports `unavailable` and never blocks.
* **Never kills.** There is no signalling code path: `TestProbeSourceHasNoSignallingCodePath` pins on
  `probes.go`'s AST that it imports no `syscall`/`os/signal`/`sessionreap` and names no `Kill`/`Signal`/
  `termPID`/`pkill`/`killall`, and the runner tests assert the `Commander` fake never sees a kill
  invocation. The operator decides what to do about an orphan.
* **Archive.** `probes.json` records, per probe: status (`ok`, `recovered`, `exceeded`, `unavailable`,
  `cancelled`, `skipped`), every reading, the threshold, attempts and seconds waited, and the orphan
  list / swept-leftover count. A zero threshold (a hand-built `Config`) disables a probe, so the
  existing runner tests are undisturbed.

## Multi-bed runs (#1976, ADR-1976)

`E2E_BEDS` is a comma-separated list of bed directories; the first is **bed A**. Unset — the
default — it is the single `FABRIK_TEST_DIR` bed and the run is the single-bed gate described
everywhere else in this file. With two or more beds the gate runs them **concurrently in one
process**, serialized only where they share a GitHub identity.

| File | What |
|---|---|
| `config.go` | `Config.BedDirs`, `parseBeds` (absolute paths; an empty entry, a duplicate by real path — symlinks included — or more than 26 beds is `ExitUsage`) |
| `beds.go` | `BedSpec`, `resolveBedSpec`, the per-bed views (`buildBedGates`/`newBedGate`), `bedEnv`, `prefixWriter`, `CheckBedTopology`, `prepareBeds` |
| `identity.go` | `Identity`, `IdentityOps`/`osIdentityOps`, `ResolveBedIdentities`, `identitySet`, `parseBedAppBanner`/`verifyBedAppIdentity`, `logIdentityBudget` |
| `multibed.go` | `MultiBedScheduler`: assignment, identity-set registry, D8 failure semantics, the closing summary |
| `consumers.go` | `FindCompetingTokenConsumersExcluding`; the #1684 check per bed |
| `reset.go`, `cmd/gate/main.go` | `ResetBeds`, `gate reset --bed <dir>` |
| `ledger.go`, `ledger_recorder.go`, `archive.go` | `Record.Bed`, `VoidBed`, `bed.json`, per-bed `NoteBedConfig` |

* **Per-bed views.** Every piece of bed state already derives from `Cfg.TestBed`, `Cfg.EngineLog`,
  `Cfg.BedToken` and `resetConfig()`: the lock, `bed-run.log`, the isolated gitconfig, build, start
  and stop, the archive's engine-log sampler and `bed-run.log` copy, the RUN INVALID scan, the
  board-lag probe and the reset. So a bed is a **view** of the invocation's `Gate` with its own
  values of those (`newBedGate` copies every field explicitly; a field added to `Gate` must be
  classified there). Shared mutable state is reached through `parent` (the INCONCLUSIVE list) or a
  pointer (`cov`, the one ledger). With one bed there are no views: `beds()` is the root `Gate`.
* **Per-bed resolution (D1).** A bed's `FABRIK_TEST_REPO_ALPHA`/`_BETA`/`FABRIK_TEST_PROJECT_OWNER`/
  `_NUMBER` come from its own `.env`, else the environment, else the defaults; its token is the
  `.env` `FABRIK_TOKEN`; its App installation is the `.env` `E2E_APP_INSTALLATION_ID`. Each leg's
  restart and suite invocations get `FABRIK_TEST_DIR` and those four (`bedEnv`, D2), and the live
  harness honours `FABRIK_TEST_PROJECT_NUMBER`.
* **Order (ADR-1454 kept).** Static preflights once per bed where bed-scoped — auth mode over every
  bed, then `bed-topology`, then the competing-consumer check per bed; the reviewer check, parity
  summary and host-load probe once. The pre-gate once (its record is repo-scoped, D10). Then the live
  preflights: `bed-identity` (each bed token's login, one `gh api user` per distinct token) and the
  board-lag probe on each bed's own board. Then `prepareBeds`: `PrepareBedAndReset` per bed,
  **serially**, followed by the engine-SHA check. Nothing live runs before the pre-gate.
* **Topology refusal (exit 7, before the pre-gate).** Two beds with the same App installation (when an
  app leg is planned), the same board, or a shared repo, or a bed with no `FABRIK_TOKEN`. Two beds
  sharing a harness login are **not** refused — they serialize.
* **Competing consumers (D5).** The #1684 check runs per bed against that bed's token with **every**
  configured bed's directory excluded, over one process listing, using all requested auth modes
  (the greedy fallback can send any cell to any bed). Another bed's engine is never a competitor; any
  other process on a bed's token — the dev daemon — still refuses.
* **Engine SHA (D11).** `E2E_BED_REF`, `E2E_BED_NO_BUILD` and `E2E_SKIP_PREP` apply to every bed;
  beds resolving to different engine SHAs (including `E2E_SKIP_PREP`'s bed-HEAD path) refuse with
  exit 7, because the ledger is per SHA.
* **Identities (D3/D4).** A cell charges a **set**: `app:<installation id>` on an app leg, plus
  `user:<login>` of the bed's `FABRIK_TOKEN` (the engine's PAT on a pat leg and the harness's token
  on every leg), deduplicated and sorted. An unresolvable login refuses (exit 7); with no resolver
  configured, the key falls back to `token:<hash>` so equal tokens still collide. After every app
  leg's restart, `verifyBedAppIdentity` reads the last startup in `bed-run.log` and refuses (exit 7)
  unless its `identity: GitHub App installation <N>` line names the `.env` installation;
  `TestBannerFormatsArePinned` pins the parser to the engine's format strings.
* **Scheduling (R3, D6).** Under the sparse matrix with the baseline in the plan, bed A runs the
  baseline cell (`app/on`) and nothing else; the other beds serve a shared
  queue in `sparseOrder`. Otherwise (`--resume`, a filtered or partial run, `E2E_MATRIX=full`) every
  bed serves the shared queue. A cell starts only when its **whole** identity set is free, acquired
  atomically under one mutex (no partial holds, so no hold-and-wait). A shared-queue bed takes the
  first cell whose set is free now and waits only if none is; bed A's own queue is strict. Beds start
  in order, each after the previous one has claimed a cell or registered a wait, so bed A gets first
  claim on a shared identity, and while bed A's next cell waits it **reserves** that cell's
  identities, so a shared-queue bed cannot keep re-acquiring them ahead of it. Each wait is
  logged once: `== waiting: <cell> on bed B needs <identity>, held by bed A (<cell>) ==` (or
  `…, reserved for bed A's next cell ==`).
* **Idle engines are stopped.** A running engine polls with its identity whether or not a leg is
  using it, so a bed's engine is stopped (`stopIdleEngine`, SIGTERM via `StopBedInstance`) while
  the bed waits for an identity and once it has no more cells; every leg's `TestSwitchTrainMode`
  restart starts it again. This is what keeps D5's exclusion of the beds' own engines from the
  #1684 check safe: no bed's engine spends an identity another bed's leg holds. (A single-bed run
  leaves its bed running after the last leg, as before.)
* **Failure (D8).** A failing leg lets the other beds' **running** legs finish but no new cell starts.
  RUN INVALID (exit 3) is narrower: that bed's records for the cell are voided (`VoidBed`), its
  engine identity is marked exhausted, cells charging it are not started, and everything else goes on.
  The exit code is the first failure's, by time; a cancel returns 130. The per-identity budget
  probes at a leg's end share one `GHAPITimeout` bound, so they can never trip the post-suite
  watchdog. `== multi-bed summary ==` lists
  every bed's cells with their outcome, every cell that never started with why, and any exhausted
  identity.
* **Budget lines (R5, also on one bed).** At leg start and end, one
  `== identity budget (leg: app/on, start): app:12345 fabrik-bed[bot] — 4210 remaining, resets … ==`
  line per identity in the cell's set (`rateLimit { remaining resetAt }`; App identities mint an
  installation token per probe). A failure warns and never gates; no token text reaches the output.
  The existing `budget_before`/`budget_after` harness-token lines are unchanged.
* **Ledger and archive (R4).** Every bed writes the one per-SHA ledger. Records carry `bed` (the
  directory, `omitempty`, so the format version is unchanged); a void line with a bed voids only
  that bed's records; each leg archive has `bed.json`; `bedconfig.json` is keyed
  `<invocation>@<bed>` so two beds' legitimately different configs are not drift (legacy
  invocation-only keys are ignored). `RequiredTests`, `ResumeCells`, the evaluator and
  `gate coverage` are unchanged: a pair covered on any bed counts. **One gate process must drive
  every bed**: `Ledger.mu` is process-local, so two gate processes on one ledger are unsupported.
* **Output (D12).** With two or more beds every bed-scoped line is prefixed `[bed A] ` / `[bed B] `,
  written a whole line at a time under a lock shared per stream, so beds never interleave mid-line.
  One bed: no prefix. With the ledger disabled the per-leg log name gains `-bed<X>`.
* **Reset (D9).** `gate reset` resets every configured bed, each with its own token, repos and board,
  serially, continuing past a failure and returning the first failure's code; `--bed <dir>` (or
  `--bed=<dir>`) resets just that directory.

Single-bed behaviour (R6) is unchanged by construction: no views, `SerialScheduler`, no prefix, no
bed variables in a leg's environment. The only additions are the identity budget lines, the
`bed-identity` live preflight (a warning, not a refusal, when the login cannot be resolved), the
banner cross-check on app legs, and the bed attribution in records and archives. The live two-bed
run, and standing up the second bed, are #1990's.

## Behaviour deltas and quirks

The port changes no behaviour on purpose. What follows is every place it differs, or where a
bash quirk was kept on purpose, so nothing is silent.

Deliberate differences, forced by the process model (all in ADR-1994):

1. **Kill escalation.** `with_timeout`/`run_reaped` sent SIGTERM to a process group. The runner
   reaps the whole **session** with `sessionreap.Escalate` (SIGTERM → grace → SIGKILL, grace
   `Config.KillGrace`, default 10s), which also reaches Bash-tool-style `setsid` command
   sessions (#1989) and has a SIGKILL backstop.
2. **Stall signal.** The stall detector watches the time of the last output write, not the log
   file's mtime. Same thing observed from inside; advisory only.
3. **Exit codes for a bed that cannot be prepared.** A failing `git rev-parse`, `git checkout`
   or bed `go build` inside preflight exited with git's/go's own code under `set -e` (128, 1).
   They now exit `4` (preflight failed), which is what `cut-release.sh` documents for "the suite
   never ran".
4. **An unusable `FABRIK_PREGATE_ALLOWED_DIRTY_REGEX`** failed open in bash (`grep -Ev … || true`
   yielded an empty `dirty`, vouching for any tree). A pattern that does not compile now counts
   every change as dirty, so the full pre-gate runs.
5. **`reset` aborts on a failed `gh pr list` / `gh issue list`** with a clear message and exit 1
   (it aborted silently under `set -e` before); the exit status is unchanged. With several beds
   (#1976) `gate reset` resets each in turn, continues past a failed bed, and takes `--bed <dir>`.
6. **A wedged output pipe is detected for a failing suite too.** `exec.Cmd.WaitDelay` reports it
   only for a successful exit, so the runner copies the pipe itself.

Quirks preserved on purpose:

- The per-leg log is `${TMPDIR:-/tmp}/fabrik-e2e-<auth>-<mode>-<runner pid>.json`, so the two
  "on" sub-legs of an auth mode overwrite each other. (#1972 owns the per-cell archive; a
  multi-bed run, #1976, appends `-bed<X>` so two beds never share a name.)
- The terminal echo of suite output is double-spaced (`jq -r` appended a newline to an `Output`
  that already ended in one).
- The timeout-teardown trigger is still a literal `panic: test timed out after` match against the
  whole log (the KNOWN LIMITATION comment in the old script applies unchanged).
- `cut-release.sh`'s `interpret_e2e_exit_code` has no case for exit `6`; it falls through to its
  generic branch, exactly as before. Not changed here.
- `--clean` is honoured only as the first argument; a `-run` anywhere in the arguments skips the
  reviewer check.
- The restart step (`TestSwitchTrainMode`) still has none of the classification/teardown
  machinery the suite step has (its documented KNOWN GAP).

## Live parity evidence

The acceptance check "one live leg through the shim produces the same outcome report as
`run.sh` did at the same SHA" needs a real bed, ~4,000 GraphQL points and an hour or more. It
is performed by an operator (or in Validate) and recorded on the PR; see the PR description.
The report formats are pinned meanwhile by the golden tests in `events_test.go`, whose
expected output was generated by the original `jq`/`column -t` code over a recorded stream.
