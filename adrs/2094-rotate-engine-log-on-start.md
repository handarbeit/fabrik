# ADR 2094: Rotate the engine log on start

## Status

Accepted

## Context

`Run()` opened `.fabrik/fabrik.log` with `O_TRUNC`, so every start (manual restart, SIGHUP exec, `--auto-upgrade` re-exec) destroyed the previous run's log: exactly the before/after-upgrade boundary operators and incident reviews (#1980) need. The file also carried no version, PID or start time. The live gate relies on the file holding **one run only**: `tests/gate/backoff.go` scans the whole file and `tests/gate/archive.go` detects a restart from the file shrinking or its head bytes changing. The request came from #2028.

## Decision

1. **Rotate at start, once per process.** After the instance lock is held and before the log is opened, `rotateEngineLog` shifts `fabrik.log.(k)` → `.(k+1)` and `fabrik.log` → `.1`, dropping the oldest. Nothing holds the log at that point, so there is no handle swapping across `e.logFile`, the unlocked `pollLogFile` global and `MultiWriter` holders. The file stays one run per file.
2. **Size cap (R4) is declined.** A mid-run rotation would hide the start of a run from `DetectRateLimitBackoff`, make the archiver read the shrink as an engine restart, invalidate every `LogOffset` the e2e harness holds, and contradict one-file-per-run. A single run's file therefore still grows unbounded, as before.
3. **Retention is an unexported constant (5)**, like Pruefer's. A config key would need a flag, env var, YAML key, `Config`, `workerenv` classification and docs for a knob nobody asked for.
4. **Failure never blocks startup.** Missing slots are skipped; other errors are warned on stderr and written into the new log after the banner. If the live log itself cannot be moved aside, it is opened with `O_TRUNC`, i.e. the previous behaviour, which keeps the gate's one-run-per-file invariant.
5. **Banner.** The first line is `<ts> [startup] fabrik <version> pid=<n> reason=<reason>`; the line's own timestamp is the start time. `Config.Version` already carries `dev(<sha>)`/`+dirty`. PID is unchanged by exec, so the start time distinguishes runs, and it also makes the archiver's head-byte restart check stronger.
6. **The start reason is captured in `cmd`** (`reexecStartReason`, before `handleReexecPluginRefresh` unsets the markers) and passed as `Config.StartReason`, so it never travels through the environment into workers. The dev-build self-upgrade set no marker, so it gained `DevBuildConfig.ExtraEnv` and a dedicated `FABRIK_DEV_REEXEC=1` (classified `GroupReexec`). It deliberately does not reuse `FABRIK_AUTO_UPGRADED`, which triggers a plugin refresh the dev `PostBuildHook` already performed.

## Consequences

- Up to six engine log files exist in `.fabrik/`; `.fabrik/fabrik.log.*` is git-ignored.
- The previous banner/version are not recorded in the new banner (the old version is not known to the new process); the previous run is `fabrik.log.1`.
- Backups left from a previously larger retention are not purged beyond normal shifting.
