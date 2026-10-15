# Feature Specification: config: resolve CLAUDE_CONFIG_DIR through one layered resolver (flag, FABRIK_ env, .env, config.yaml, ambient) and warn when sources disagree

**Feature Branch**: `fabrik/issue-2293`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "config: resolve CLAUDE_CONFIG_DIR through one layered resolver (flag, FABRIK_ env, .env, config.yaml, ambient) and warn when sources disagree"

## Background

`CLAUDE_CONFIG_DIR` decides which Claude account every stage worker bills to, but Fabrik never resolves it. It takes whatever the daemon's process environment holds. Report #1807 describes three projects that ran for about three days on the wrong subscription: their `.env` named one profile, and a tmux server had exported a different one.

Current behaviour (v0.0.85):

- `.env` loading ends in `godotenv.Load(".env")`, which never overwrites a variable that is already set. An inherited `CLAUDE_CONFIG_DIR` therefore silently beats the `.env` line. The same loader is called by the main command after flag parsing and by the `upgrade`, `repair-board` and `watch` subcommands and by Pruefer.
- There is no `claude_config_dir` key in the project config, no `--claude-config-dir` flag and no `FABRIK_*` variable. A project cannot record the account it belongs to, although the documented layering for every other setting is "CLI flag > shell env var > .env > .fabrik/config.yaml > built-in default" (stated in this repo's `.fabrik/config.yaml` and the `fabrik init` template).
- Three places read the raw variable: the startup notice (which prints only the winning value and its account), the user-settings-layer lookup that the `apiKeyHelper` check scans, and the TUI footer's Claude account.
- Workers get the value only by inheritance: the worker environment is the process environment plus engine-set extras, and nothing sets `CLAUDE_CONFIG_DIR`. A test pins this passthrough.
- Re-exec blurs where a value came from. The SIGHUP restart and the self-upgrade exec both pass the current process environment, which already holds whatever `.env` loaded. A re-exec'd process therefore cannot tell an inherited value from a `.env` value by looking at its environment alone.
- When two sources disagree nothing warns, and the operator warnings panel gets no entry, although this is exactly the kind of operator-level config fault the panel exists for.

Why bare `CLAUDE_CONFIG_DIR` sits *below* `config.yaml` in the new order: the variable belongs to Claude Code and is exported by unrelated tools, so its presence says nothing about intent toward Fabrik. Placing it there keeps the documented order exactly, with no exception. `FABRIK_CLAUDE_CONFIG_DIR` stays at the env layer, so an operator can still override a committed `config.yaml` from the environment, as the house rule promises for every setting.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A project's `.env` profile beats a stray exported variable (Priority: P1)

An operator runs Fabrik in a project whose `.env` names a Claude profile. Their shell or tmux server has exported a different `CLAUDE_CONFIG_DIR`. Fabrik uses the `.env` profile and tells them the other value was overridden.

**Why this priority**: This is the exact regression behind #1807: workers billed to the wrong subscription for days with no signal.

**Independent Test**: Resolve with ambient = A, `.env` = B and no other source set; the effective directory is B, the source is `.env`, and the disagreement warning names both A and B.

**Acceptance Scenarios**:

1. **Given** ambient `CLAUDE_CONFIG_DIR` = A and `.env` `CLAUDE_CONFIG_DIR` = B, **When** Fabrik starts, **Then** the effective directory is B, the startup line names `.env` as the source, and the panel entry lists both sources and says B won.
2. **Given** only the ambient value is set, **When** Fabrik starts, **Then** the ambient value is the effective directory and is passed to workers, and no warning is raised.

---

### User Story 2 - Committed project default and deliberate per-invocation overrides (Priority: P1)

A team records the project's Claude profile in `.fabrik/config.yaml` with `claude_config_dir:`. An operator can still override it for one run with `--claude-config-dir` or `FABRIK_CLAUDE_CONFIG_DIR`.

**Why this priority**: Without a place to record the account, a project cannot declare which subscription it belongs to.

**Independent Test**: Table test of all six layers set to distinct directories: each layer wins over every layer below it.

**Acceptance Scenarios**:

1. **Given** ambient = A and YAML = C, **When** resolving, **Then** the result is C.
2. **Given** YAML = C and `FABRIK_CLAUDE_CONFIG_DIR` = D in the shell, **When** resolving, **Then** the result is D.
3. **Given** every layer set, **When** `--claude-config-dir` is passed, **Then** the flag value wins.
4. **Given** `.env` holds both `FABRIK_CLAUDE_CONFIG_DIR` and `CLAUDE_CONFIG_DIR` with different values, **When** resolving, **Then** the `FABRIK_` one wins and the conflict is reported.

---

### User Story 3 - Workers always use the resolved profile (Priority: P1)

Every `claude` worker subprocess the engine starts runs under the resolved profile, regardless of what the daemon's own environment holds.

**Why this priority**: Resolving correctly is useless if workers still inherit the overridden ambient value.

**Independent Test**: With ambient = A and resolved = C, the constructed worker environment contains exactly `CLAUDE_CONFIG_DIR=C` and no `FABRIK_CLAUDE_CONFIG_DIR`; with nothing set it contains no `CLAUDE_CONFIG_DIR`.

**Acceptance Scenarios**:

1. **Given** a resolved directory, **When** a worker is started, **Then** its environment carries `CLAUDE_CONFIG_DIR=<resolved>`.
2. **Given** nothing resolved, **When** a worker is started, **Then** its environment carries no `CLAUDE_CONFIG_DIR`.
3. **Given** `FABRIK_CLAUDE_CONFIG_DIR` set in the daemon, **When** a worker is started, **Then** that variable is absent from the worker.

---

### User Story 4 - Provenance survives a restart (Priority: P2)

After a SIGHUP restart or a self-upgrade exec, Fabrik resolves the same directory from the same source as it did at first start, and never reports a `.env` or resolved value as ambient.

**Why this priority**: Otherwise the layering and the warning would be correct on first start and wrong after every restart.

**Independent Test**: Start from an environment that already contains the `.env` value (as a SIGHUP restart passes it); the `.env` value is reported as `.env`, not ambient, and the result and source equal the original start's.

**Acceptance Scenarios**:

1. **Given** an original start with ambient = A and `.env` = B, **When** the process re-execs, **Then** the new process resolves B from `.env` and still reports A as the (overridden) ambient source.
2. **Given** an original start with no ambient value and `.env` = B, **When** the process re-execs with B now in its environment, **Then** B is reported as `.env`, never as ambient, and no spurious disagreement is raised.

---

### User Story 5 - One resolution for every consumer, with a visible source (Priority: P2)

The `apiKeyHelper` check, the startup line and the TUI footer all use the same resolved directory. The startup line says where it came from.

**Why this priority**: Consumers reading different values is the same class of bug in a quieter form.

**Independent Test**: A test fails if the footer account and the startup line diverge; `checkAPIKeyHelper` scans the resolved directory's `settings.json`, not the ambient one's.

**Acceptance Scenarios**:

1. **Given** a resolved directory with a readable account, **When** Fabrik starts, **Then** it prints one line such as `[startup] notice: Claude profile "/x" (a@b.com) from .fabrik/config.yaml`.
2. **Given** nothing resolved, **When** Fabrik starts, **Then** it prints nothing about the profile.

---

### User Story 6 - The resolver is reusable by Pruefer (Priority: P3)

The resolver lives in a stdlib-only package outside `engine` and `cmd` so that Pruefer (#2294) can import it unchanged.

**Why this priority**: Pruefer has the same bug (report #2025); this issue only provides the shared piece.

**Independent Test**: The package imports nothing from `engine`, `cmd` or other project-internal layers, and takes all of its inputs as data.

**Acceptance Scenarios**:

1. **Given** the resolver package, **When** its imports are inspected, **Then** it depends only on the standard library.

---

### Edge Cases

- Two sources naming the *same* directory are not a disagreement: no warning is raised and an existing panel entry is cleared.
- A lone source, or none, never warns; `warnings.Resolve` clears any earlier entry.
- A source whose account email cannot be read is still listed; the email is simply omitted.
- Account emails may change between runs; the panel entry's fingerprint is built from source/directory pairs only, so a changed email does not make a dismissed warning reappear.
- A one-shot `CLAUDE_CONFIG_DIR=x fabrik` no longer beats `.env` or `config.yaml`. This is a deliberate behaviour change; the flag and `FABRIK_CLAUDE_CONFIG_DIR` are the supported per-invocation overrides.
- Ambient present with all lower layers empty: the ambient value is the resolved one and is passed to workers.
- A SIGHUP restart or self-upgrade exec started from an environment that already contains the `.env` value.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A single resolver function computes the effective Claude config directory and reports where it came from. It lives in a stdlib-only package outside `engine` and `cmd` (e.g. `internal/claudeprofile`) so Pruefer can import it.
- **FR-002**: The resolver takes all inputs as data (flag value, deliberate env value, parsed `.env` map, YAML value, ambient snapshot, and the name of the deliberate variable) and performs no I/O beyond what the caller injects.
- **FR-003**: The resolver returns the effective directory, the winning source, and every source that supplied a value, each with its directory and (when readable) its account email. The account lookup (`ClaudeProfileAccount` today) moves into or is wrapped by this package; the engine keeps a thin wrapper.
- **FR-004**: Precedence, highest first: (1) `--claude-config-dir` flag; (2) `FABRIK_CLAUDE_CONFIG_DIR` from the shell environment; (3) `.env`, read as data, where `FABRIK_CLAUDE_CONFIG_DIR` wins over `CLAUDE_CONFIG_DIR`; (4) `.fabrik/config.yaml` `claude_config_dir:`; (5) bare `CLAUDE_CONFIG_DIR` inherited from the environment (ambient); (6) unset, meaning Claude Code's own default.
- **FR-005**: A new `--claude-config-dir` flag, a new `FABRIK_CLAUDE_CONFIG_DIR` variable and a new `claude_config_dir` project-config key are accepted and feed the resolver.
- **FR-006**: The ambient value is captured before `.env` is loaded. `.env`'s values for the two names are read as data (`godotenv.Read`, available in the pinned v1.5.1) and are never indistinguishable from the environment.
- **FR-007**: Provenance survives both re-exec paths (SIGHUP restart and self-upgrade). Given unchanged inputs, a re-exec'd process resolves the same value from the same source as the original start and never reports a `.env` or resolved value as ambient. The mechanism (e.g. keeping these names out of the process environment, or carrying the original ambient snapshot across the exec under a dedicated variable) is chosen at planning time.
- **FR-008**: The user-settings-layer lookup used by `checkAPIKeyHelper`, the startup notice and the TUI footer account all use the resolved value. No production code outside the resolver reads `os.Getenv("CLAUDE_CONFIG_DIR")`, enforced by a static scan test in the style of the existing source-scan tests.
- **FR-009**: Every `claude` subprocess the engine starts carries `CLAUDE_CONFIG_DIR=<resolved>` under that exact name when a value was resolved, even when the process environment holds a different, overridden ambient value. When nothing resolved, the worker carries no `CLAUDE_CONFIG_DIR`. `FABRIK_CLAUDE_CONFIG_DIR` itself never reaches a worker.
- **FR-010**: A single startup line replaces the current notice, giving the effective directory, its account email when readable, and its source, e.g. `[startup] notice: Claude profile "/x" (a@b.com) from .fabrik/config.yaml`. When nothing resolved, the output stays silent as today.
- **FR-011**: When two or more sources supply different directories, the resolution is reported with `warnings.Report` (log line plus panel entry, stable type such as `claude_config_dir`). The entry names every source with its directory and account email and says which one won. Its `Fingerprint` is the set of source/directory pairs, not the account emails. When the sources agree or only one is set, `warnings.Resolve` clears the entry. No bare `[warn]` or `WARNING:` line is written, so `TestOperatorWarningsReachThePanel` keeps passing.
- **FR-012**: `FABRIK_CLAUDE_CONFIG_DIR` is added to `internal/workerenv.Scrubbed` with a reason, so `TestEveryFabrikLiteralIsClassified` passes.
- **FR-013**: `fabrik init`'s config template gains a commented `# claude_config_dir:` entry with a one-line explanation. The precedence header in the template and in this repo's `.fabrik/config.yaml` states where bare `CLAUDE_CONFIG_DIR` sits and why.
- **FR-014**: `docs/USER_GUIDE.md` "Alternate Claude Profile (`CLAUDE_CONFIG_DIR`)" is rewritten for the layering, the flag, the env var, the key, the startup line, the warning, the one-shot behaviour change and re-exec behaviour. The existing paragraph claiming re-execs re-run `.env` loading is corrected to describe the new mechanism. The configuration reference gains the new key and flag, `docs/llms-full.txt` is regenerated (`bash scripts/generate-llms-full.sh`), and `CLAUDE.md` gets a note on where the resolver lives.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A resolver table test with all six layers set to distinct directories shows each layer winning over every layer below it.
- **SC-002**: The regression case (ambient = A, `.env` = B, no YAML key) resolves to B and the disagreement warning names both; ambient = A with YAML = C resolves to C; `FABRIK_CLAUDE_CONFIG_DIR` = D beats YAML = C; the flag beats everything; `.env` holding both names resolves to the `FABRIK_` one and reports the conflict.
- **SC-003**: A provenance test, run from an environment already containing the `.env` value (simulating a SIGHUP restart), never reports a `.env` value as ambient and resolves identically to the original start.
- **SC-004**: A worker-environment test (extending `TestInvokeClaude_ClaudeConfigDirSurvives`) shows exactly `CLAUDE_CONFIG_DIR=C` and no `FABRIK_CLAUDE_CONFIG_DIR` with ambient = A and resolved = C, and no `CLAUDE_CONFIG_DIR` with nothing set.
- **SC-005**: `checkAPIKeyHelper` scans the resolved directory's `settings.json`, not the ambient one's.
- **SC-006**: The TUI footer account and the startup line come from the same resolution; a test fails if they diverge.
- **SC-007**: The panel entry is recorded on disagreement and cleared on agreement (using `testenv.IsolateWarningsProcess` for isolation).
- **SC-008**: A static scan finds no `os.Getenv("CLAUDE_CONFIG_DIR")` outside the resolver package.
- **SC-009**: Non-vacuity: with the resolver's precedence reverted to "environment wins", the A/B regression test fails. This is recorded in the PR.
- **SC-010**: The PR body states that no sim twin exists because this is process-start configuration, which `tests/sim` (built through `NewWithDeps`) does not model.
- **SC-011**: The PR body mentions #1807 by bare number only, with no closing keyword near it; the PR closes this issue (#2293).

## Assumptions

- An empty string at any layer is treated as "not set" for that layer.
- Two sources count as the same directory when their paths are equal after the resolver's normalization (at minimum, cleaning); the exact normalization is a planning detail and is low-impact and reversible.
- The `upgrade`, `repair-board` and `watch` subcommands and Pruefer start no Claude workers through the engine; they keep calling the existing `.env` loader, and Pruefer adopts the resolver in #2294. Whether those subcommands also feed the resolver is a planning detail, not a requirement here.
- The account email shown for a source is read the same way as today (from the profile directory); an unreadable profile simply omits the email.
- The configuration is read once at process start; changing it requires a restart.

## Out of Scope *(optional)*

- Pruefer's adoption of the resolver (#2294, report #2025).
- Any other foreign-namespace variable.
- Switching `LoadDotenv` wholesale to `godotenv.Overload`, which would change precedence for every `.env` entry.
- Validating the profile directory's contents or login state.
- Reloading the value without a restart.

## Source References *(optional)*

- Report #1807 (three projects billed to the wrong subscription).
- Issue #2294 / report #2025 (Pruefer, follow-up reusing this resolver).
- `docs/state-machine.md` §7.11 (operator warnings panel).
