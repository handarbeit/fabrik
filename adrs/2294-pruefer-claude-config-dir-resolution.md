# ADR 2294: Pruefer Resolves CLAUDE_CONFIG_DIR Through the Shared Resolver

**Date**: 2026-10-15
**Status**: Accepted
**Issue**: #2294 — pruefer: resolve CLAUDE_CONFIG_DIR through the shared resolver (the Pruefer half of #1807; report #2025)

## Context

Pruefer could not declare which Claude account its review subprocesses bill to. `Execute`
loaded `.env` with `config.LoadDotenv()`, which never overwrites a variable that is already
set, so a `CLAUDE_CONFIG_DIR` line in `.env` silently lost to an inherited value. `LoadConfig`
had no `claude_config_dir` key, flag or `PRUEFER_` variable, and the review subprocess got
the value only by inheritance (it is on the review-environment allowlist). Nothing logged
which account reviews ran under. ADR 2293 solved the same problem for the engine with a
pure, stdlib-only resolver in `internal/claudeprofile` and recorded that Pruefer adopts it
"unchanged" in this issue.

## Decision

1. **The resolver gains an additive `Names` on `Inputs`.** The resolver printed Fabrik's own
   spellings (`FABRIK_CLAUDE_CONFIG_DIR`, `.fabrik/config.yaml`, `--claude-config-dir`) in
   `Source.Name` and read the `FABRIK_` key from the `.env` data, so Pruefer's startup line
   and disagreement warning would have named variables that do not exist for it. `Names`
   carries the flag, deliberate-variable and config-file spellings; its zero value is
   Fabrik's, so no engine call site or output changes (pinned by a test). Precedence,
   `Disagree`, `Fingerprint` and `FillAccounts` stay shared: there is one implementation. The
   alternative — mapping Pruefer's variable into the `FABRIK_` slots and relabelling after
   resolution — was rejected as a shim that leaks Fabrik names and duplicates presentation.
2. **`claudeprofile.ResolveHost`** is the Home/Cwd/`FillAccounts` wrapper that used to live
   in `cmd`; `cmd.resolveClaudeProfile` now delegates to it and Pruefer calls it directly.
   The startup-line and disagreement formatters are *not* shared: Pruefer prints a line when
   nothing resolved (the engine stays silent), writes to its log rather than a warnings
   panel, and cannot import `engine` (ADR 1113).
3. **Precedence for Pruefer**: `-claude-config-dir`; shell `PRUEFER_CLAUDE_CONFIG_DIR`; `.env`
   (`PRUEFER_CLAUDE_CONFIG_DIR` over the bare name); `claude_config_dir` in the Pruefer YAML;
   the ambient `CLAUDE_CONFIG_DIR`; unset. The ambient variable ranks below YAML for the same
   reason as in ADR 2293. `PRUEFER_CLAUDE_CONFIG_DIR` follows the `PRUEFER_*` convention and
   is not added to the engine's `internal/workerenv`.
4. **Provenance by withholding.** `Execute` snapshots the ambient value before any `.env` read
   and loads `.env` through `config.LoadDotenvWithholding`, which exports neither profile
   name. The resolved winner is never `os.Setenv`'d. Both would otherwise come back from a
   self-upgrade re-exec (which passes `os.Environ()`) looking like inherited values.
5. **Strict propagation.** `Config.ClaudeConfigDir` holds the resolved directory; it is copied
   into `ReviewRequest` at dispatch and `buildReviewEnv` emits `CLAUDE_CONFIG_DIR=<dir>`.
   `reviewProcessEnv` no longer copies the inherited `CLAUDE_CONFIG_DIR` at all, so a
   resolved value is the only source and "nothing resolved" yields no variable. The
   alternative — a "configured" flag so hand-built configs keep inheriting — would leave a
   second path through which inheritance survives silently. Because the ambient value is the
   fifth layer, an operator who configures nothing still gets their inherited directory.
   `CLAUDE_CONFIG_DIR` stays on the allowlist so `checkReviewEnvScrubbed` keeps accepting it;
   `PRUEFER_CLAUDE_CONFIG_DIR` is refused by the existing `PRUEFER_` never-passthrough rule.
6. **Reload (`live`).** The field is `reload:"live"`. A reload re-reads `.env` as data through
   the new pure `config.ReadDotenv` (reusing `LoadDotenvWithholding` would newly export any
   `PRUEFER_*` name added to `.env` since startup, changing unrelated settings) and keeps the
   ambient snapshot frozen. `loadConfig` returns the `Resolution` beside the `Config`, so the
   line logged is exactly the resolution of the config applied; per-source detail cannot live
   on `Config` because `applyConfigReload` walks its fields with `reflect`. A failed reload
   restores the previous `.env` data. A running review keeps the value in its `ReviewRequest`.
7. **Logging.** One startup line (always, set or not) and a reload line when the directory
   changed; a disagreement warning at startup and on every reload while it persists. There is
   no TUI log view, so the log file is the surface; the footer's account is refreshed after a
   reload by a new `ClaudeAccountEvent`, which is what keeps the field `live`.
8. **Repo safety.** `claude_config_dir` is not in `recognizedRepoConfigKeys`, so a reviewed
   repo's `.pruefer/config.yaml` cannot set it; it is ignored with the existing "not
   repo-settable" warning. A test runs the real review path to pin this.

## Consequences

- A one-shot `CLAUDE_CONFIG_DIR=x pruefer` no longer beats `.env` or YAML; the disagreement
  warning is the signal and the README says so. A `.env` value now applies even with nothing
  inherited.
- `pruefer/tui_run.go`'s one audited raw read of `CLAUDE_CONFIG_DIR` is gone and its exception
  in `internal/claudeprofile/scan_test.go` removed.
- There is no sim twin: Pruefer is outside `tests/sim`.
