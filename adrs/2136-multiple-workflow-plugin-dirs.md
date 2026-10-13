# ADR 2136: Workers load several workflow plugins at once

## Status

Accepted

## Context

A worker received exactly one plugin directory. Configuring `--plugin-dir` /
`FABRIK_PLUGIN_DIR` replaced Fabrik's deployed `.fabrik/plugin`, so customising one stage
meant vendoring every stage skill. Claude Code accepts `--plugin-dir` repeatedly.

## Decision

- **A separate additive setting**, `plugin_dirs` (`config.yaml` list), `--plugin-dirs`,
  `FABRIK_PLUGIN_DIRS` (comma-separated on flag and env, like
  `merge_train_overlap_ignore`). The singular `--plugin-dir` is not reused: it keeps its
  "replace the base plugin" meaning (development override). `plugin_dirs` is a real
  `ProjectConfig` key, so it is honoured wherever documented; any other unknown key is
  still reported.
- **Order is base first, then extras in configured order.** The state holder is a single
  slice (`claudePluginDirs`); `buildClaudeArgs` emits one `--plugin-dir` per entry. With
  no extras the arguments are identical to before.
- **Validation in `engine.New`** (`engine/plugin_dirs.go`, `resolvePluginDirs`), before any
  global state is assigned: each extra must be an existing directory with a readable
  `.claude-plugin/plugin.json` with a non-empty `name` (error names the path); two loaded
  plugins with the same manifest name are refused naming both paths, rather than leaving
  the outcome to Claude Code's load order.
- A base without a readable manifest has an unknown name and is skipped in the name check,
  so a bare-directory development override still works. Only extras require a manifest.
- Relative extras resolve against the Fabrik directory; exact repeats and an extra equal to
  the base are dropped silently. Extras load alone when no base exists.
- Extras stay outside `CheckPluginState`, `RefreshPlugin` and `fabrik upgrade` (ADR-1787):
  they are the project's own.

## Non-goals

- Interactive resume/watch commands still pass only the single base override.
- How stage YAML names a skill within a given plugin, and converting existing
  customisations, are later links in the chain.

## Consequences

A project can carry its own stage skill without forking Fabrik's twelve. A project plugin
may not be named `fabrik` while the default base plugin is loaded. The change is visible
only at the argument level, so it is covered by unit tests on `buildClaudeArgs`; the sim
never reaches that layer.
