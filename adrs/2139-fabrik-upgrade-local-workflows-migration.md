# ADR 2139: `fabrik upgrade` migrates to qualified skill names and `local-workflows`

## Status

Accepted

## Context

Customising a stage meant editing Fabrik's deployed plugin in `.fabrik/plugin/` in place.
Fabrik protects those edits (ADR 1297, ADR 1787): `fabrik upgrade` refuses to overwrite
them and startup warns. The cost is that a customised deployment never receives plugin
updates again, and its edits share Fabrik's own plugin name, which blocks both
multi-plugin workers (ADR 2136) and the rename of the base plugin (#2140). With qualified
skill references (ADR 2137) customisations can live in a plugin of their own.

The binary and the stage configuration change at different times. Release auto-upgrade
swaps the binary and re-execs without running `fabrik upgrade`; operators skip releases.
So the migration cannot be tied to a version step.

## Decision

- **`fabrik upgrade` migrates; the daemon never does.** It qualifies the skill references
  in `.fabrik/stages/*.yaml` and moves an operator-edited `.fabrik/plugin` to
  `.fabrik/plugins/local-workflows/`, restoring a clean base plugin. Startup only records
  a `plugin_state:migration_pending` warnings-panel entry (ADR 2135) while
  `planMigration(...).Pending()`; it clears on the first start after the migration ran.
- **It decides from disk, never from version numbers.** Customisation is judged per file
  against a table of every sha256 any release shipped (`plugin/shipped_files.json`), plus
  the current embed at runtime. `KnownEmbeddedVersions` stays as it is: it holds aggregate
  fingerprints and cannot say whether one file differs from every shipped version.
  The table is generated from the release tags by `tools/gen-shipped-files`, extended at
  each release by `scripts/cut-release.sh` (the script that already owns
  `KnownEmbeddedVersions`), and a unit test fails a PR that changes the embed without it.
  Embedding full historic copies was rejected as larger and unnecessary.
- **A tree moves only when** `CheckPluginState` reports `customWorkflow` **and** some file
  is unknown to the table. A mixture of shipped versions is refreshed in place; a dev-build
  tree (ADR 1297, whose fingerprints are never in any table) is not customised, so it
  never moves and an unreleased dev state cannot be mistaken for an edit.
- **Whole-tree move, per-skill redirect.** Shared files (`LABELS.md`, `README.md`, the
  manifest) and extra files travel with the skills that use them; only stages whose own
  skill has an unknown file are pointed at `local-workflows`. Unedited skills that travel
  are left unreferenced. A customisation of only a shared file therefore still moves the
  tree (which unsticks upgrades) but redirects no stage.
- **The manifest `name` is the one changed byte in the moved tree.** `resolvePluginDirs`
  refuses two plugins with the same name, so it must differ from the base. The edit is a
  textual substitution verified by decoding both versions; `.installed-version` is dropped
  because it described the base. Skills are byte-identical.
- **Order makes every interruption resumable:** rename the tree aside (destination never
  overwritten) → fix manifest/drop `.installed-version` → restore the base
  (`RefreshPluginTo` + `WriteInstalledVersion`) → rewrite stage files atomically. Each
  state is recognisable from disk, so the next plan completes the remaining steps; on a
  resume the edited skills are read from the existing `local-workflows`. Nothing is
  half-moved without being re-reported. A `local-workflows` that already exists while the
  base has edits blocks the move: it is never overwritten, nothing changes, the old
  refusal follows. An existing `local-workflows` is also only normalised (manifest `name`,
  `.installed-version`) when it is recognisably a moved base copy — its manifest names the
  base plugin or is absent. A manifest naming any other plugin is the operator's own: it is
  left untouched, reported, and no stage is redirected to `local-workflows:` (the name would
  not resolve).
- **Stage files are edited surgically** (`stages.RewriteSkillRefs`): only column-0 skill
  keys, in place, so comments, quoting and CRLF survive and block-scalar text cannot
  match, and the original and result are both decoded to confirm only the intended values
  differ. A `yaml.Node` round trip (the `refresh-stages` precedent) re-marshals the whole
  file and was rejected. Already-qualified references are never rewritten; if one names a
  base skill that was edited the report names the `local-workflows` copy. A bare name no
  visible plugin provides (`fabrik upgrade` has no config resolution, so `plugin_dirs`
  are invisible to it) stays bare and is reported, never guessed as `fabrik:`; it does not
  keep the migration pending.
- **Vendored sets are not moved.** `FABRIK_PLUGIN_DIR` set, or `.fabrik/plugin` a symlink,
  is reported with instructions. The `--plugin-dir` flag is not visible to the subcommand;
  startup skips the pending check when it is set.
- **Dev-build auto-upgrade does not migrate.** Its `PostBuildHook` runs
  `fabrik upgrade --no-migrate`: it is an unattended subprocess in the daemon's working
  tree, and the migration moves files and edits tracked YAML. `--no-migrate` keeps the
  pre-#2139 behaviour exactly, including the refusal on customised plugins (ADR 1787).
- **`local-workflows` loads automatically** (after the base, before `plugin_dirs`, same
  validation), so no config change is needed. `fabrik resume`/`watch` still pass only the
  base plugin (ADR 2136's non-goal) and do not load it.
- **The base name comes only from `plugin.BasePluginName`**, so #2140's rename is a
  one-line change; a test pins that the migration code hard-codes no qualified name.

## Consequences

- A customised deployment receives plugin updates again after one `fabrik upgrade`; an
  uncustomised one only has its stage references qualified.
- Every release that changes the embedded plugin must extend `shipped_files.json`; a
  forgotten `-add-current` only mislabels the *next* release's users until `-check` (which
  `cut-release.sh` runs) fails first.
- A stage already written as `fabrik:<edited skill>` keeps pointing at the now-pristine
  base copy; the report says so. This follows the rule that qualified references are left
  alone.
- **No sim twin.** The sim bed builds engines with `NewWithDeps`, which load no plugins
  and have no skill resolver, so it cannot express `local-workflows` loading. The
  equivalent coverage is at the loader (`TestResolvePluginDirs_*`) plus the migration
  tests in `cmd/upgrade_migrate_test.go`, which run on fixtures captured from
  v0.0.64, v0.0.70 and v0.0.77 (`plugin/testdata/migration/`).
- The migration code stays in future releases; the User Guide documents it and how to undo
  it with git.
