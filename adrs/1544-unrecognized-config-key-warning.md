# ADR 1544: Unrecognized config.yaml Key Detection

**Date**: 2026-08-12
**Status**: Accepted
**Issue**: #1544 — `config.yaml` silently discards unknown keys, so every CLI/env-only knob
looks configured when it isn't

## Context

`LoadProjectConfig` (`config/config.go`) decodes `.fabrik/config.yaml` with a plain
`yaml.Unmarshal` into `ProjectConfig`. go-yaml's default behavior is to silently discard any
key with no matching struct field. `archive_after: 48h` sat in a production `config.yaml` for
weeks under the belief that board archiving was on; it never ran once. The gap was only found
by grepping the binary's own `yaml:"..."` struct tags by hand.

At least six real, flag-and-env-backed knobs had no `config.yaml` equivalent at the time this
issue was filed (`archive_after`, `archive_done`, `reconcile_interval`, `max_review_cycles`,
`max_slice_retries`, `drain_deadline`). Research found the true count substantially higher (~20
knobs), and the list grows by default every time a new CLI/env-only knob is added — nothing
today forces a decision about `config.yaml` support when one is introduced. #992 added one such
key (`merge_train`) reactively, after someone happened to notice it missing.

Fabrik already has a structurally identical precedent: `stages/drift.go`'s stage-YAML drift
warning prints `[startup] warning: ...` when a stage config file is *missing* fields present in
newer embedded defaults. This issue is the mirror case — a config file has an *extra*,
unrecognized key — and should read as the same family of diagnostic.

## Decision

### Detect, don't reject

`LoadProjectConfig` itself is untouched — same signature, same behavior, unknown keys keep
being silently ignored *functionally*. A new pure function, `config.UnrecognizedTopLevelKeys()`,
runs as a side pass: it re-reads `.fabrik/config.yaml` into a `map[string]any`, reflects over
`ProjectConfig`'s `yaml:"..."` struct tags to build the known-key set, and returns the sorted
top-level keys present in the file but absent from that set. `yaml.KnownFields(true)` strict
decoding (which fails the whole parse on the first unknown key) was explicitly rejected — existing
installs may have accumulated dead keys from past experiments or typos, and failing startup over
those would be a regression in its own right.

Detection is top-level-only by construction: the second unmarshal only ever inspects the
document's outermost keys, so values inside `RequiredStatusContexts` (the one map-typed field)
are structurally never visited, never recursed into, and never misreported as unrecognized.

Deriving the known-key set from the struct's own tags (rather than a second hand-maintained
list) means it can never drift from what `LoadProjectConfig`'s own `yaml.Unmarshal` actually
recognizes — the reflection walk and the real decode read the exact same tags.

### Pure-detection / flag-aware-suggestion split

`config.UnrecognizedTopLevelKeys()` returns a flag-registry-independent `[]string`. A separate
function, `cmd.warnUnrecognizedConfigKeys(keys []string, w io.Writer)`, takes that list and does
the flag-aware part: for each key it derives a candidate flag name
(`strings.ReplaceAll(key, "_", "-")`) and env var name (`"FABRIK_" + strings.ToUpper(key)`), and
checks `flag.CommandLine.Lookup(flagName)`. `cmd/root.go` always calls
`flag.CommandLine.Parse()` before `config.LoadProjectConfig()` runs, so every real CLI flag is
already registered in the global registry by the time this check executes — `flag.Lookup` is a
live existence oracle, not a maintained table. A hit prints the suggestion clause naming both
the flag and env var; a miss (a pure typo, or a genuinely env-only knob such as
`FABRIK_ANTHROPIC_API_KEY`, which has no registered flag at all) prints only the base warning
line.

This split exists for testability, not layering purity: `config`'s test suite is table-driven
and runs without any dependency on Go's global, order-sensitive `flag.CommandLine` registry
(which panics on duplicate flag registration) today, and this change keeps it that way. `cmd`
already imports both `flag` and `config`, and already has a `resetFlags()` test helper built for
exactly this kind of registry-dependent test.

### Naming-convention exceptions are a known, accepted limitation

Every current CLI flag with **no** `config.yaml` support today follows the mechanical
snake_case ↔ kebab-case ↔ `FABRIK_SCREAMING_SNAKE_CASE` convention cleanly — verified against
all ~55 registered flags, not just the six named in the issue. Four flags whose triple *does*
break the convention were found, but all four already have a recognized `config.yaml` key —
they can never be the *subject* of this warning, only a source of a misleading suggestion if
someone mistypes the recognized key in a way that happens to collide with the derived name:

| `config.yaml` key | flag | env var |
|---|---|---|
| `git_ssh` | `--ssh` | `FABRIK_GIT_SSH` |
| `tui` | `--notui` (inverted) | `FABRIK_TUI` |
| `project` | `--project` | `FABRIK_PROJECT_NUMBER` |
| `janitor_interval_hours` | `--janitor-interval` | `FABRIK_JANITOR_INTERVAL` |

E.g. `janitor_interval:` (missing the `_hours` suffix) mechanically resolves to a real
`--janitor-interval`/`FABRIK_JANITOR_INTERVAL` pair and prints "CLI/env-only" even though
`janitor_interval_hours` **is** already supported, just spelled differently. This is documented
as a known limitation in `docs/USER_GUIDE.md` rather than special-cased: special-casing four
specific keys would recreate exactly the hand-maintained-exception-list problem this issue
exists to eliminate, for a narrow edge case (typo of an already-recognized key) whose outcome
is still directionally useful even when imprecisely worded.

### Scope: stderr-only, `cmd/root.go`-only

The warning is printed to stderr only — no `warnings.Record`/TUI Warnings-panel entry, and no
fan-out to `.fabrik/fabrik.log`. `stages.WarnStageDrift` gets that fan-out via
`io.MultiWriter(os.Stderr, e.logFile)` inside `engine.Run()`'s `poll()`, but this diagnostic
fires from `cmd/root.go`'s `Execute()`, before `engine.New`/`Run` exist or the log file is open.
The issue's own Requirements specify only a stderr line; duplicating the log/TUI fan-out would
need real restructuring the issue doesn't ask for.

Detection is wired into `cmd/root.go`'s daemon path only, immediately after the existing
`config.WarnIfConfigIgnored()` call — not `fabrik watch`'s `LoadProjectConfig()` call site. The
issue's Requirements text explicitly scopes this to "the main daemon startup path." `watch` also
uses its own local `flag.NewFlagSet("watch", ...)`, not the global `flag.CommandLine` registry
this detection's existence check relies on — extending coverage there would need a second,
`watch`-specific existence check with narrower coverage (5 flags vs. ~55), not a trivial
extension of the same code path.

## Consequences

- An operator with a stray or misspelled `config.yaml` key now gets a same-startup diagnostic
  instead of silent, indefinite no-op behavior — closing the exact gap the `archive_after`
  incident exposed.
- The flag/env suggestion is self-maintaining: adding a new CLI/env-only knob to `cmd/root.go`
  automatically makes its suggestion clause fire the moment someone tries the matching
  `config.yaml` key, with no additional code change required.
- The four naming exceptions (all already-recognized keys) can produce a misleading suggestion
  for a narrow typo shape; documented, not fixed, per the trade-off above.
- `fabrik watch` and TUI/log-file visibility are explicitly out of scope for this issue. Both are
  reasonable, low-cost follow-ups if wanted later — `watch` needs its own `flag.FlagSet`-based
  existence check, and TUI/log fan-out needs this detection to run somewhere `engine.Run()` can
  reach with its own writer, mirroring `WarnStageDrift`'s call site in `engine/poll.go`.
