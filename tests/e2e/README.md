# Fabrik end-to-end tests

Scenario-driven integration tests for Fabrik. Each test drives a real Fabrik
instance (`~/dev/fabrik-test/`) against the real test repositories
(`handarbeit/fabrik-test-alpha`, `handarbeit/fabrik-test-beta`), files an
issue, and asserts on the resulting pipeline behaviour.

## What this is for

These tests catch regressions that escape `go test ./...` — bugs that only
manifest in real integration with GitHub, real Claude invocations, real
worktrees, and the real end-to-end stage pipeline. The category of bug:

- The `addBlockedBy` GraphQL mutation name (shipped broken in v0.0.66; fixed by #800)
- The pre-Implement spawn step failing on never-touched repos (#797, #803)
- The `Closes #N` getting absorbed into a nested code fence (#738)
- The CI gate `HeadSHA` resolution in poll-only mode (#779)

Every such regression that escapes a release earns a new scenario here.

## Where this lives in the release flow

```
[ go test ./... ]               unit tests; fast; run on every PR
        |
        v
[ tests/e2e/... ]               integration tests; slow; run before release
        |
        v
[ scripts/cut-release.sh ]      cuts a release
```

Suggested integration with `cut-release.sh` (not yet wired):

```bash
scripts/cut-release.sh v0.0.67                       # default — does NOT run e2e
scripts/cut-release.sh v0.0.67 --integration-check   # run e2e before tagging
scripts/cut-release.sh v0.0.67 --skip-integration    # explicit skip when iterating
```

We'll flip the default to `--integration-check` once the suite is stable.

## Test bed prerequisites

These tests assume:

1. **`~/dev/fabrik-test/` exists** with `.fabrik/config.yaml`, `.env`
   (containing `FABRIK_TOKEN` for `@arbeithand`), and a built `fabrik` binary.
2. **`handarbeit/fabrik-test-alpha`** and **`handarbeit/fabrik-test-beta`**
   are reachable with the token.
3. **`handarbeit/projects/2`** ("Fabrik Test") exists with stage columns
   (Backlog, Specify, Research, Plan, Implement, Review, Validate, Done).
4. **No other Fabrik instance is using the `@arbeithand` token's GraphQL
   budget concurrently** (or use `--max-concurrent 1` if you have to share).

See `~/fabrik-oss-launch-notes.md` (under "Files and where they live") for
the canonical setup.

## Running

```bash
# Run the whole suite (slow — minutes per scenario)
go test -tags=e2e -timeout 60m ./tests/e2e/...

# Run one scenario
go test -tags=e2e -timeout 30m -run TestSmokeSingleRepo ./tests/e2e/

# Verbose log streaming (recommended — these are long-running)
go test -tags=e2e -v -timeout 60m ./tests/e2e/...
```

The `e2e` build tag keeps these out of the default `go test ./...` run.

## Adding a scenario

1. Pick a name like `cross_repo_spawn_test.go`. Use `Test<DescriptiveName>` for the
   function so `-run` filtering is clean.
2. Use the helpers in `harness.go` to file the trigger issue and watch for
   expected events.
3. Always clean up at the end — close opened issues, remove the worktree from
   the test bed (`t.Cleanup` is your friend).
4. Document what regression the scenario protects against. Reference the
   originating issue or PR.

## Design notes

- Tests do **not** start or stop the Fabrik instance. The instance is expected
  to be already running. (Future enhancement: a `harness.StartFabrik(t)` that
  spawns/stops it per test session.)
- Assertions are on **observable outcomes**, not internal state. We check
  GitHub for label changes, comments, PR creation, etc. — not the engine's
  internal `worktreeManagers` map.
- Log-line assertions are deliberately last-resort. Prefer GitHub state. Logs
  are only useful when the observable outcome is "Fabrik logged something
  specific" (e.g., the spawn error from #797/#803).
- Scenarios should be **idempotent** — running twice in a row should produce
  the same result. If a scenario depends on starting state that prior runs
  modify, normalize it at the start of the test.

## Known limitations

- **Cost per run is non-trivial.** A full cross-repo scenario costs $1–3 in
  Claude tokens. The suite is not for casual local iteration.
- **CI integration is not wired yet.** Initially the suite is operator-only —
  run before cutting a release. Future work: a GitHub Actions runner that
  exercises the suite on a schedule.
- **GitHub rate-limit pressure.** Shared with `~/dev/fabrik/` (the dev
  instance) under the `@arbeithand` token. Stop the dev instance if running
  the full suite.
