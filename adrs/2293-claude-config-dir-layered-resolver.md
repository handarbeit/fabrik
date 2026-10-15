# ADR 2293: Resolve CLAUDE_CONFIG_DIR Through One Layered Resolver

**Date**: 2026-10-14
**Status**: Accepted
**Issue**: #2293 — config: resolve CLAUDE_CONFIG_DIR through one layered resolver

## Context

`CLAUDE_CONFIG_DIR` decides which Claude account every stage worker bills to, but Fabrik
never resolved it: it took whatever the daemon's process environment held. `.env` loading
ends in `godotenv.Load`, which never overwrites a variable that is already set, so an
inherited value (for example one a tmux server had exported) silently beat the project's
`.env` line. Three projects ran for about three days on the wrong subscription (report
#1807). There was no `config.yaml` key, flag or `FABRIK_` variable, three places read the
raw variable, and nothing warned when two sources disagreed.

A second constraint: both re-exec paths (the SIGHUP restart and the self-upgrade exec)
pass the current `os.Environ()` to the new process. Anything the first process put into
its environment — including what `godotenv.Load` added — is indistinguishable from an
inherited value in the next one.

## Decision

1. **One pure resolver** in the existing stdlib-only package `internal/claudeprofile`
   (`Resolve(Inputs) Resolution`), so Pruefer can adopt it unchanged (#2294). Precedence,
   highest first: `--claude-config-dir`; shell `FABRIK_CLAUDE_CONFIG_DIR`; `.env` (its
   `FABRIK_` name over the bare name); `config.yaml` `claude_config_dir`; a bare inherited
   `CLAUDE_CONFIG_DIR`; unset. The bare variable ranks below `config.yaml` because it belongs
   to Claude Code and unrelated tools export it, so its presence says nothing about intent
   toward Fabrik; `FABRIK_CLAUDE_CONFIG_DIR` keeps the environment layer, so the documented
   "flag > env > .env > config.yaml" order holds with no exception.
2. **Provenance by withholding, not carrying.** `config.LoadDotenvWithholding` applies the
   same `.gitignore` guard and no-overwrite rule as `LoadDotenv` but returns the two names as
   data and never exports them; the resolver result is never `os.Setenv`'d. The re-exec paths
   then carry only true ambient and shell values and each start re-reads `.env` and YAML as
   data. The alternative — a scrubbed `GroupReexec` snapshot variable — needs edits at three
   exec sites plus a new classification, and adds state that can go stale. `LoadDotenv` is
   unchanged for `upgrade`, `repair-board`, `watch` and Pruefer (Pruefer relies on `.env`
   exporting `CLAUDE_CONFIG_DIR` through its review-env allowlist until #2294).
3. **Result held process-wide** (`claudeprofile.Set`/`Current`), following the `claudeGHHost`
   precedent: `buildClaudeEnv` is a free function and `tui`/`watch` cannot import `engine`.
   Engines built without `cmd` (tests) see the unconfigured zero value and inherit as before.
4. **Workers always get an explicit value:** `CLAUDE_CONFIG_DIR=<resolved>`, or a removal
   sentinel when nothing resolved, so an overridden ambient value cannot leak.
   `FABRIK_CLAUDE_CONFIG_DIR` is added to `workerenv.Scrubbed`.
5. **Interactive launches follow the resolution** (`claudeprofile.Command` in `tui` and
   `watch`). With `.env` values no longer exported they would otherwise silently switch to
   the ambient account — the #1807 failure in a new place.
6. **Visibility:** one `[startup] notice: Claude profile "<dir>" (<email>) from <source>`
   line, and on disagreement a `warnings.Report`ed `claude_config_dir:sources` panel entry
   naming every source and the winner. Its fingerprint is the sorted source/directory pairs,
   never the account emails.

## Consequences

- **Behaviour change:** a one-shot `CLAUDE_CONFIG_DIR=x fabrik` no longer beats `.env` or
  `config.yaml`. The supported per-invocation overrides are the flag and
  `FABRIK_CLAUDE_CONFIG_DIR`; the disagreement warning is the signal for anyone relying on
  the old behaviour. Documented in `docs/USER_GUIDE.md`.
- **Invariant for maintainers:** nothing may export the two names into the process
  environment or `os.Setenv` the winner; `internal/claudeprofile/scan_test.go` and the
  provenance test in `cmd` guard it.
- `fabrik resume` never loaded `.env` and stays ambient-only; `upgrade`, `repair-board` and
  `watch` keep `LoadDotenv` (they start no engine workers). `pruefer/tui_run.go` keeps one
  audited raw read until #2294.
- Same-directory comparison is `~` expansion plus `filepath.Clean`; symlinks are not
  resolved. Low-impact and reversible.
- No sim twin: this is process-start configuration, which `tests/sim` (built through
  `NewWithDeps`) does not model.
