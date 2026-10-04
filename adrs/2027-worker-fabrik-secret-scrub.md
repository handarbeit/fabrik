# ADR 2027: Worker environment — scrub the daemon's Fabrik credentials, and make the test suite hermetic against them

## Status

Accepted

## Context

A Fabrik stage worker's environment should carry exactly the GitHub access the worker is meant to have and nothing else of the daemon's. It carried the daemon's whole environment.

**Root cause: `.env` → process env → worker env.** `cmd/root.go` calls `config.LoadDotenv()` (`godotenv.Load(".env")`), so every `.env` entry lands in the daemon's own process environment. `buildClaudeEnv` builds the worker environment from `os.Environ()`. Anything an operator keeps in `.env` therefore reached every worker unless it was explicitly removed. Only the Anthropic auth namespace (ADR-1346) and the `FABRIK_*` invocation facts (ADR-1288) were. Observed on a live App-auth daemon, a worker's `claude` process carried `FABRIK_GITHUB_APP_ID`, `FABRIK_GITHUB_APP_INSTALLATION_ID`, `FABRIK_GITHUB_APP_PRIVATE_KEY_PATH`, `FABRIK_GITHUB_WEBHOOK_SECRET` and `FABRIK_TOKEN`: the key path lets a worker mint installation tokens indefinitely, the secret lets it forge deliveries, the PAT is a long-lived personal credential.

The second effect is on the tests. Workers run the repo's own suite, so every test that reads this environment behaves differently in a worker than in CI (which has none of it): `cmd` tests inherited the App config and hung on an unbounded `<-readyCh` (~11 minutes of one Implement run); `TestExecute_PATMode_StillRequiresUser` and `TestRunInit_GitHubApp_AdoptPairMismatch` failed; `TestAppGitCredentialHelper_ServesTokenFileOverAmbientHelper` saw the daemon's ADR-1846 `GIT_CONFIG_COUNT`/`KEY_n`/`VALUE_n` entries — command-scope config, which outranks the global file `GIT_CONFIG_GLOBAL` isolation points at — answer instead of the test's own helper.

## Decision

1. **An audited classification, default scrubbed** (`internal/workerenv`). Every `FABRIK_*` variable the engine reads is either *scrubbed* (daemon-only credential or config) or *forwarded*; a variable is forwarded only with a stated reason. The forwarded class is exactly the five worker facts the engine sets on purpose (`FABRIK_ISSUE`, `FABRIK_REPO`, `FABRIK_WORKTREE`, `FABRIK_ROOT`, `FABRIK_PR`). `NotEnv` lists output-marker literals (`FABRIK_STAGE_COMPLETE`, …) that are not environment variables. `TestEveryFabrikLiteralIsClassified` parses the non-test sources with `go/parser` and fails on any `FABRIK_*` string literal in none of the three lists, so a future variable cannot reach workers by omission (precedent: `tests/e2e/registry`). The package is stdlib-only so `engine`, `cmd` and every package's tests share it without an import cycle.

2. **Exact-name removal sentinels, never a wildcard.** `buildClaudeEnv` appends bare removal sentinels (the existing `mergeEnv` mechanism, as for `FABRIK_ANTHROPIC_API_KEY`) for the fixed list. A `FABRIK_*` wildcard would collide with the invocation facts emitted in the same slice. The list covers at least `FABRIK_TOKEN`, the three `FABRIK_GITHUB_APP_*` variables, `FABRIK_GITHUB_WEBHOOK_SECRET` and `FABRIK_REVIEWER_TOKEN` (read only by the live e2e tests, but it can sit in the same `.env`). There is no distinct upgrade-token variable: `releaseUpgradeToken` derives from `cfg.Token`.

3. **Operator-chosen Hookdeck names are resolved at run time.** `FABRIK_HOOKDECK_API_KEY_ENV` / `FABRIK_HOOKDECK_WEBHOOK_SECRET_ENV` hold the *names* of the variables carrying the secrets, so a fixed list cannot cover them. `Engine.New` resolves the effective names once (`workerenv.Resolve`) into `claudeWorkerEnv`, beside the other `claude*` globals; both the named variables and the defaults (`HOOKDECK_API_KEY`, `FABRIK_GITHUB_WEBHOOK_SECRET`) are scrubbed unconditionally, so a stale `.env` entry is removed whatever `event_source` is. A configured name that is `Protected` (`PATH`, `GH_TOKEN`, …) is refused with a startup warning, so a bad config value can never strip a sanctioned variable.

4. **The sanctioned GitHub access is protected by construction.** `GH_TOKEN`/`GITHUB_TOKEN` (the live installation token, ADR-1713, or the configured PAT in PAT mode), `GH_HOST` (ADR-1391) and the ADR-1846 `GIT_CONFIG_*` credential-helper entries are on no scrub list and are `Protected` against both the dynamic names and the opt-in. PAT mode is unchanged except that the `FABRIK_TOKEN` variable itself is scrubbed. Ambient `GH_TOKEN`/`GH_HOST` with nothing for the engine to inject is deliberately left alone (out of scope; in PAT mode an ambient `gh` login may be the operator's configured access).

5. **A separate, named opt-in: `FABRIK_WORKER_ENV_PASSTHROUGH`.** Comma-separated exact names from the scrub list. Implemented as "do not emit the sentinel", so the admitted name simply inherits from the base environment: it is never re-added as `KEY=VALUE`, so it cannot shadow an engine-computed override (the ordering hazard documented in `buildClaudeEnv`'s Anthropic passthrough loop) and cannot touch `GH_TOKEN`. Names not on the scrub list, `Protected` names, the opt-in itself and the ADR-1346 control pair are ignored with a warning. It may admit credentials (R1b says "a scrubbed key"), so a `[startup]` notice names every admitted variable. `FABRIK_ANTHROPIC_ENV_PASSTHROUGH` is not extended: its Anthropic-namespace restriction is deliberate (ADR-1346).

6. **Hermetic tests (`internal/testenv`).** `Isolate(t)` truly *unsets* (via `t.Setenv` for restore, then `os.Unsetenv`; `""` would differ for `LookupEnv` callers) every scrubbed Fabrik variable, `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST`, `GIT_CONFIG_COUNT`/`GIT_CONFIG_PARAMETERS` and every `GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` found by enumerating the environment (not a fixed 0–2). `IsolateGit` adds a test-local global git config and `GIT_CONFIG_NOSYSTEM`; it replaces `isolateGitConfig`/`isolateGitConfigEnv`. `ScrubProcess` does the same once for a `TestMain`; `cmd` gets one and `engine`'s calls it. A test that needs one of these variables sets it explicitly with `t.Setenv`.

7. **Bounded readiness waits.** Every receive on `testReadyCh` (and the `engine` equivalents) selects on `Execute()`/`Run()` returning and on a deadline, and fails with the returned error. A setup failure now fails fast instead of at the package timeout.

8. **The property is checked in CI.** `scripts/ci/worker-shaped-test.sh` exports dummy values for all of the above plus an ADR-1846-shaped `GIT_CONFIG_*` helper pointing at a dummy token file, and runs the same `go test -race ./...` shape as the main step. It is meant to run as a separate CI job (parallel, so wall time stays flat); the script is also the local command. **Wiring status:** Fabrik's GitHub App token has no `workflows` permission, so the edit to `.github/workflows/ci.yml` (add the job, drop `-skip TestExecute_ConfigYAMLApplied`) could not be pushed; it is recorded in `scripts/ci/worker-shaped-job.yml` for a maintainer to apply. (Applied afterwards by a maintainer: the `worker-shaped` job now runs in `.github/workflows/ci.yml`, and the `-skip TestExecute_ConfigYAMLApplied` is gone; the stub file was removed.)

## Consequences

- A worker no longer holds the App key path, the webhook secret, the PAT or any other daemon-only variable. It still fetches, pushes and uses `gh` with only the App-minted token.
- **Behaviour change:** a repo-side script that read `FABRIK_TOKEN` (or any other scrubbed variable) from inside a worker no longer sees it. `FABRIK_WORKER_ENV_PASSTHROUGH` is the escape hatch.
- Adding a `FABRIK_*` variable to the engine now requires classifying it, or `go test ./...` fails with a message saying how.
- Residual, by design: the installation token and the token file the git helper reads still reach the worker (R2). The ADR-1713 long-invocation expiry gap is unchanged.
- The `claude*` package globals keep the single-Engine-per-process assumption; tests that touch `claudeWorkerEnv` save and restore it.

## Related

ADR-1346 (Anthropic namespace scrub — the precedent), ADR-1713 (`claudeGHTokenOverrideFn`), ADR-1846 (git credential helper), ADR-1893 (App mode has no operator identity), ADR-1391 (`GH_HOST`), ADR-1288 (invocation facts).
