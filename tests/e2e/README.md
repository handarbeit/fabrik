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

### Additional prerequisites for `TestCIFixReinvoke` and `TestCIFixReinvokeCycleLimit`

5. **`ci-fix-sentinel` enrolled as a required status check** on
   `handarbeit/fabrik-test-alpha/main`. Both tests skip gracefully (via
   `t.Skip`) if this check is not enrolled — safe to merge before the sibling
   sub-issue adds the sentinel CI job.
6. **`FABRIK_MAX_CI_FIX_CYCLES=2` in the test bed `.env`** (for
   `TestCIFixReinvokeCycleLimit` only). The test skips with an instructional
   message if the value is `> 3`. After editing `.env`, restart the test-bed
   Fabrik instance so the new value takes effect.
7. **`E2E_TIMEOUT=3h`** when running `TestCIFixReinvoke` in isolation — the
   inner waits alone total ~75–90 min, which exceeds the default `90m` budget
   once pipeline setup overhead is included:
   ```bash
   E2E_TIMEOUT=3h scripts/e2e/run.sh -run TestCIFixReinvoke
   ```

## Running

The recommended entrypoint is the runner script, which sets sensible defaults:

```bash
# Full suite (slow — multiple scenarios × minutes each)
scripts/e2e/run.sh

# Single scenario
scripts/e2e/run.sh -run TestSmokeSingleRepoDispatch

# Subset by name pattern
scripts/e2e/run.sh -run 'Smoke|NoWork'
```

Anything after the script name is passed through to `go test`. Override the
overall test timeout with `E2E_TIMEOUT` (default `90m`).

The `e2e` build tag keeps all of this out of the default `go test ./...` run.

### Reset between runs

```bash
scripts/e2e/reset.sh             # closes open issues in alpha + beta
scripts/e2e/reset.sh --worktrees # also wipes Fabrik's worktrees + bare clones (destructive)
```

Use the plain form between test runs to clean issues. The `--worktrees` form
is for when the test bed itself is wedged (stop Fabrik first, it will refuse
otherwise).

## Scenarios

| Test | What it verifies | Approx wall-clock | Cost |
|---|---|---|---|
| `TestSmokeSingleRepoDispatch` | Worker dispatches on a trivial issue; Specify completes | 3–5 min | $0.10–0.20 |
| `TestSmokeSingleRepoFullPipeline` | Full single-repo pipeline (Specify → … → Done with merged PR) | 20–40 min | $0.50–1.50 |
| `TestNoWorkNeeded` | `FABRIK_NO_WORK_NEEDED` short-circuit closes issue without PR | 10–15 min | $0.30–0.50 |
| `TestBlockedOnInput` | `FABRIK_BLOCKED_ON_INPUT` pause + comment-driven resume | 10–15 min | $0.30–0.50 |
| `TestCrossRepoSpawn` | Cross-repo decomposition (spawn child in beta, gate parent, resume on close) | 45–60 min | $1.00–2.00 |
| `TestYoloAutoMergeLabel` | `fabrik:yolo` auto-advance to Done via GitHub native auto-merge; timeline-verifies `fabrik:auto-merge-enabled` was applied | 20–40 min | $0.50–1.50 |
| `TestCruiseFullPipeline` | `fabrik:cruise` auto-advances to Validate-complete without auto-merge; PR merged by human closes issue | 30–50 min | $0.80–2.00 |
| `TestCIFixReinvoke` | CI-fix reinvoke positive path: sentinel fails on first push, Claude fixes, CI passes, issue closes | 75–90 min | $1.00–3.00 |
| `TestCIFixReinvokeCycleLimit` | CI-fix reinvoke negative path: unfixable sentinel exhausts MaxCiFixCycles, issue pauses | 30–60 min | $0.50–1.50 |

Approximate suite total: ~250 min wall-clock, $5–17 in Claude tokens (CI-fix tests should be run separately with `E2E_TIMEOUT=3h`).

### Regression coverage map

| Scenario | Issues / fixes it protects |
|---|---|
| `TestSmokeSingleRepoDispatch` | General pipeline breakage |
| `TestSmokeSingleRepoFullPipeline` | Full pipeline regression |
| `TestNoWorkNeeded` | #733 (marker), #742 (close-on-no-work) |
| `TestBlockedOnInput` | `FABRIK_BLOCKED_ON_INPUT` marker, ed46b7fc (awaiting-input label clear) |
| `TestCrossRepoSpawn` | #797 / #803 (on-demand spawn-target init), v0.0.66 spawn machinery, #800 (addBlockedBy mutation name) |
| `TestYoloAutoMergeLabel` | #829 (GitHub native auto-merge for yolo), #831/#835/#871 (convergence regression cascade) |
| `TestCruiseFullPipeline` | #898 (cruise/yolo gate at Validate, `engine/poll.go`); ensures cruise never triggers `checkAutoMergeConvergence` |
| `TestCIFixReinvoke` | #888 ADR-056 D1 (settling primitive reinterprets CI-gate signals); CI-fix reinvoke loop (engine/ci.go) |
| `TestCIFixReinvokeCycleLimit` | CI-fix cycle limit (`pauseForCIFixCycleLimit`), `MaxCiFixCycles` exhaustion path |

Every escape-from-release regression earns a new scenario in this table.

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
