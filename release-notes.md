# Fabrik v0.0.46

## Features

- **Merge-conflict gate preempts CI-await spin on unmergeable PRs (#435).** Adds a third prong to catch-up Phase 1 — `checkMergeabilityGate` — sitting between review reinvoke and the CI gate. When GitHub reports the linked PR as `mergeable=false` (the base branch advanced during the CI-await window and now conflicts), the engine applies `fabrik:rebase-needed` and dispatches a rebase re-invocation. Claude — not the engine — runs the rebase, so semantic collisions (duplicate ADR numbers, migration slot reuse) can be renamed rather than silently dropped. `MaxRebaseCycles` defaults to 3 (lower than review/CI) because rebase either works in one shot or needs human judgment. Motivating case: develop issue where main moved forward with a conflicting ADR while Validate sat in CI-await, burning ~30 minutes of re-invokes on "CI still running" / "no changes" before the underlying blocker was ever detected. New ADR-028 and state-machine.md §6.5 capture the design.

- **`max_wall_time` stage field + inactivity kill + streaming marker detection.** New `max_wall_time: 45m` YAML field on stages caps each Claude invocation; a 15-minute inactivity watchdog separately kills stages that go silent. Kills are graceful (SIGTERM → 10s → SIGKILL to the whole process group). Critically, markers are now extracted from the in-flight NDJSON stream *before* the kill, so stages that emitted `FABRIK_STAGE_COMPLETE` or `FABRIK_BLOCKED_ON_INPUT` mid-run still advance correctly instead of being discarded with the kill. New ADR-029 documents the proactive-kill rationale.

## Fixes

- **Grandchild pipe-hold no longer sticks issues with `fabrik:locked` indefinitely (#429).** When Claude used `run_in_background` or the Monitor tool, grandchild processes (`tail -f`, polling loops) inherited the stdout pipe fd and kept it open after Claude exited. Go's `cmd.Wait()` blocks on pipe EOF, so the worker goroutine never returned. Fixed with `cmd.WaitDelay=30s` (configurable via `--claude-wait-delay` / `FABRIK_CLAUDE_WAIT_DELAY`), new process group (`Setpgid=true`, Unix-only), and `SIGKILL` to the group after the parent exits. Regression test confirms `InvokeClaude` returns within ~1s even when a background sleep holds the pipe. See ADR-005 for details.
- **`ErrWaitDelay` no longer masks engine shutdown.** When both `WaitDelay` fired and the context was cancelled, the previous code cleared the error unconditionally, bypassing the shutdown guard and letting a cancelled invocation advance the stage. Now guarded with `ctx.Err() == nil`.
- **`killProcGroup` surfaces unexpected errors** (e.g. `EPERM`) to stderr; only `ESRCH` (already gone) is silently suppressed. Previously all kill errors were swallowed.
- **`FABRIK_CLAUDE_WAIT_DELAY=0` accepted silently** as "use default" (matches flag semantics). Previously printed a spurious warning.
- **`Engine.New` now sets `claudeWaitDelay` unconditionally** — previous guard meant constructing a new engine with `ClaudeWaitDelay=0` would silently retain the previous engine's non-default value.
- **`cmd.Cancel` log message corrected** when `maxWallTime=0`: logs "context cancelled" instead of the misleading "exceeded max_wall_time" on engine shutdown.

## Internal

- New regression test `engine/grandchild_test.go` covering the pipe-hold scenario.
- New ADRs 028 (merge-conflict gate) and 029 (proactive kill design); ADR-005 cross-references 029.
- Expanded `docs/state-machine.md` with the new event (2.11), transition rows, and §7.6 invocation kill mechanics.
- `README.md`, `USER_GUIDE.md`, and `CLAUDE.md` updated for the new stage field, label, and flag.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
