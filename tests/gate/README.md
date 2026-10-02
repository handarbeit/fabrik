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

Structure only — none of the features exist yet.

| Seam | Where | Used by |
|---|---|---|
| `Cell` (auth, train, parallel, args, isolated) and `PlanCells` | `schedule.go` | #1975 sparse matrix, #1976 multi-bed |
| `Scheduler` interface (`SerialScheduler` today) on `Gate` | `schedule.go` | #1975, #1976, #1977 two-phase legs |
| `LegResult` (cell, exit code, log path, decoded `[]Event`, budget before/after) delivered to `Gate.OnLeg` | `leg.go` | other observers. **#1972's ledger does not use it** — it records from `suiteWriter`'s event sink (`ledger_recorder.go`), because `OnLeg` never fires for a killed, RUN INVALID, watchdog or restart-failed leg |
| `Classification` (pass/fail/skip/running/never-started) | `events.go` | #1973 adds INCONCLUSIVE (the ledger's `Outcome` is already an open enum with INCONCLUSIVE reserved) |
| `Ledger`, `Evaluator`, `Report` and `RequiredTests`/`ResumeCells` over `PlanCells` output | `ledger*.go`, `coverage.go`, `resume.go` | #1972; #1975's sparse plan changes the required set with no change here |
| `[]Preflight` (`Gate.Preflights`, ordered, each returns an `*ExitError`) | `gate.go` | #1974 environment probes |
| `Commander` (`Run`/`Start`), `Gate.Env`, `Sleep`, `Now`, `ProcCwd` | `exec.go`, `gate.go` | every test; #1976's per-bed environments |

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
| dispatch guard: precondition order, `TRAIN_ISOLATED_RE`, `caller_has_run`, the auth × train loop | `gate.go`: `Gate.Run`; `schedule.go`: `TrainIsolatedRE`, `PlanCells`, `SerialScheduler`; `args.go`: `ParseRunArgs`, `HasRunFlag` |
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

Deltas this adds to the port: `ParseRunArgs` consumes `--clean` **and** `--resume` as leading flags
in either order; exit code 8 exists (only under `--resume` and `coverage`); the per-leg `-json` log
moves into the archive when the ledger is on (the `$TMPDIR` name, and its main/isolated overwrite
quirk, remain when it is off); and `RunLeg` starts an archive/sampler before the restart step.

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
   (it aborted silently under `set -e` before); the exit status is unchanged.
6. **A wedged output pipe is detected for a failing suite too.** `exec.Cmd.WaitDelay` reports it
   only for a successful exit, so the runner copies the pipe itself.

Quirks preserved on purpose:

- The per-leg log is `${TMPDIR:-/tmp}/fabrik-e2e-<auth>-<mode>-<runner pid>.json`, so the two
  "on" sub-legs of an auth mode overwrite each other. (#1972 owns the per-cell archive.)
- The terminal echo of suite output is double-spaced (`jq -r` appended a newline to an `Output`
  that already ended in one).
- The timeout-teardown trigger is still a literal `panic: test timed out after` match against the
  whole log (the KNOWN LIMITATION comment in the old script applies unchanged).
- `cut-release.sh`'s `interpret_e2e_exit_code` has no case for exit `6`; it falls through to its
  generic branch, exactly as before. Not changed here.
- `--clean` is honoured only as the first argument; a `-run` anywhere in the arguments skips the
  reviewer check and the forced isolated leg.
- The restart step (`TestSwitchTrainMode`) still has none of the classification/teardown
  machinery the suite step has (its documented KNOWN GAP).

## Live parity evidence

The acceptance check "one live leg through the shim produces the same outcome report as
`run.sh` did at the same SHA" needs a real bed, ~4,000 GraphQL points and an hour or more. It
is performed by an operator (or in Validate) and recorded on the PR; see the PR description.
The report formats are pinned meanwhile by the golden tests in `events_test.go`, whose
expected output was generated by the original `jq`/`column -t` code over a recorded stream.
