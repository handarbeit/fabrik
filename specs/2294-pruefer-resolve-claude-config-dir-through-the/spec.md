# Feature Specification: Pruefer resolves CLAUDE_CONFIG_DIR through the shared resolver, with `claude_config_dir`, `-claude-config-dir`, and effective-account logging

**Feature Branch**: `fabrik/issue-2294`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "pruefer: resolve CLAUDE_CONFIG_DIR through the shared resolver, add claude_config_dir and -claude-config-dir, log the effective account"

## Background

Pruefer cannot declare which Claude account its review subprocesses bill to. Report #2025 is the Pruefer half of #1807 (the engine half is #2293, which introduced the shared resolver and which this issue depends on).

State in v0.0.85:

- `pruefer/execute.go:27` calls `config.LoadDotenv()`, which ends in `godotenv.Load(".env")` (`config/config.go:219`). A `CLAUDE_CONFIG_DIR` line in `.env` therefore loses, silently, to an inherited value, for example one pinned by a tmux server.
- `LoadConfig` (`pruefer/config.go:504`) resolves flag > `PRUEFER_*` env > YAML > default for every other setting. Its YAML struct (`pruefer/config.go:384-439`) has no `claude_config_dir` key, its flag set has no `-claude-config-dir`, and there is no `PRUEFER_CLAUDE_CONFIG_DIR`.
- The review subprocess gets the value only by inheritance. `runReview` sets `cmd.Env = reviewProcessEnv(os.Environ(), req)` (`pruefer/claude.go:802`). `reviewProcessEnv` keeps allowlisted names, and `CLAUDE_CONFIG_DIR` is on that list (`pruefer/review_env.go:36`). `buildReviewEnv` (`pruefer/claude.go:665`) adds only the effort and adaptive-thinking overrides. The `buildReviewArgs` comment (`pruefer/claude.go:622`) already names "whichever profile the daemon happened to inherit via CLAUDE_CONFIG_DIR" as a reproducibility hazard.
- Nothing logs which account reviews run under, at startup or on a SIGHUP reload (`handleReload`, `pruefer/execute.go:242`).
- Self-upgrade re-execs through `internal/selfupgrade` (`pruefer/upgrade.go:96`, `internal/selfupgrade/release.go:188`). That passes `os.Environ()`, which already contains whatever `.env` loaded, so a re-exec'd process cannot tell a `.env` value from an inherited one.

`cmd/pruefer/README.md` "The per-account model" recommends one daemon per account, each pointing `CLAUDE_CONFIG_DIR` at its own profile. Doing that reliably today needs a wrapper that exports the variable before `exec`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Declare the review account in Pruefer's own configuration (Priority: P1)

An operator running one Pruefer daemon per account sets the Claude profile directory with `claude_config_dir:` in the Pruefer YAML, `-claude-config-dir`, `PRUEFER_CLAUDE_CONFIG_DIR`, or `.env`, instead of wrapping the daemon in a script that exports `CLAUDE_CONFIG_DIR`. Every review subprocess bills to that profile, even if the daemon inherited a different value.

**Why this priority**: This is the core defect. Without it, the account reviews bill to is an accident of the parent environment.

**Independent Test**: Start Pruefer with an ambient `CLAUDE_CONFIG_DIR=A` and `.env` containing `CLAUDE_CONFIG_DIR=B`; the review subprocess environment holds exactly `CLAUDE_CONFIG_DIR=B`.

**Acceptance Scenarios**:

1. **Given** ambient = A, `.env` = B, no YAML key, **When** a review is dispatched, **Then** the review process environment contains exactly `CLAUDE_CONFIG_DIR=B`.
2. **Given** ambient, `.env` and YAML all set to different directories, **When** the daemon resolves, **Then** the documented order holds, and the flag and `PRUEFER_CLAUDE_CONFIG_DIR` each beat YAML.
3. **Given** nothing resolves, **When** a review is dispatched, **Then** the subprocess carries no `CLAUDE_CONFIG_DIR`.
4. **Given** `PRUEFER_CLAUDE_CONFIG_DIR` is set, **When** a review is dispatched, **Then** that variable is absent from the subprocess environment.

---

### User Story 2 - See which account reviews run under (Priority: P1)

An operator reads the startup log (and the TUI log view when the TUI is on) and sees the effective directory, its account email when readable, and which source supplied it. After a SIGHUP reload that changes the effective directory, the same line is logged again.

**Why this priority**: The report asks for the account to be visible; a silent default is the original problem.

**Independent Test**: Capture log output at startup and after a reload; assert the line's content.

**Acceptance Scenarios**:

1. **Given** a directory resolves, **When** the daemon starts, **Then** one line names the directory, the account email when readable, and the source.
2. **Given** nothing resolves, **When** the daemon starts, **Then** one line states that reviews use Claude Code's default profile.
3. **Given** a reload changes the effective directory, **When** the reload completes, **Then** the line is logged again; a reload that leaves it unchanged logs no new line.

---

### User Story 3 - Be warned when sources disagree (Priority: P2)

When two or more sources supply different directories, the operator gets a warning naming every source, its directory and its account email, and the winner, at startup and on reload.

**Why this priority**: The `.env`-loses-to-ambient surprise is exactly a disagreement the operator never saw.

**Independent Test**: Configure ambient = A and `.env` = B; assert the warning on captured log output names both sources, both directories, and B as the winner.

**Acceptance Scenarios**:

1. **Given** two sources with different directories, **When** the daemon starts or reloads, **Then** one warning lists every source, directory, email and the winner.
2. **Given** all supplying sources agree, **When** the daemon starts, **Then** no disagreement warning is logged.

---

### User Story 4 - Change the account on reload without a restart (Priority: P2)

An operator edits `claude_config_dir:` in the YAML and sends SIGHUP. Reviews dispatched afterwards use the new directory; a review already running keeps the one it started with.

**Why this priority**: Matches how Pruefer treats its other live settings.

**Independent Test**: Reload test in the style of `pruefer/reload_test.go`: change the YAML, reload, and assert the next review's `CLAUDE_CONFIG_DIR` and the logged effective account. The ambient snapshot taken at process start is not re-read.

**Acceptance Scenarios**:

1. **Given** a running daemon, **When** the YAML value changes and a reload is signalled, **Then** the next review carries the new value and the new effective account is logged.
2. **Given** a review in flight during a reload, **When** the reload completes, **Then** that review's environment is unchanged.

---

### User Story 5 - A reviewed repo cannot choose the billing account (Priority: P1)

A reviewed repository's `.pruefer/config.yaml` that sets `claude_config_dir` is ignored with the existing operator-only-key warning and never affects the review environment.

**Why this priority**: Letting a reviewed repo select the billing account would be a security regression.

**Independent Test**: Place the key in a reviewed repo's config and assert the warning is logged and the review environment is unchanged.

**Acceptance Scenarios**:

1. **Given** a reviewed repo config sets `claude_config_dir`, **When** it is loaded, **Then** the key is ignored with the operator-only-key warning and the review environment is unchanged.

---

### Edge Cases

- Self-upgrade re-exec: the re-exec'd process must still tell an ambient value from a `.env`-loaded one (same provenance requirement as the engine in #2293).
- A directory whose account email cannot be read: the line and the warning still appear, without an email.
- `PRUEFER_CLAUDE_CONFIG_DIR` and `CLAUDE_CONFIG_DIR` both in `.env`: the `PRUEFER_`-prefixed name wins within `.env`.
- Reload when the ambient value differs from the one at process start: the start-time snapshot is used, never the current process environment.
- A review already running at reload time keeps its starting value.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Pruefer resolves the directory through the shared resolver introduced by #2293, including its account-email lookup. No second implementation of the precedence, provenance capture or disagreement detection is added.
- **FR-002**: Precedence, highest first: (1) `-claude-config-dir` flag (new); (2) `PRUEFER_CLAUDE_CONFIG_DIR` from the shell environment (new); (3) `.env`, read as data, with `PRUEFER_CLAUDE_CONFIG_DIR` beating `CLAUDE_CONFIG_DIR` within it; (4) `claude_config_dir:` in the Pruefer YAML config (new; `.pruefer/config.yaml` by default); (5) bare `CLAUDE_CONFIG_DIR` inherited from the environment (ambient); (6) unset.
- **FR-003**: The ambient value is captured in `Execute` before `config.LoadDotenv()` runs, and its provenance survives a self-upgrade re-exec in the same way #2293 requires for the engine.
- **FR-004**: When a value resolved, every `claude` review subprocess carries `CLAUDE_CONFIG_DIR=<resolved>` under that name, even when the daemon's own environment holds a different, overridden value. When nothing resolved, the subprocess carries no `CLAUDE_CONFIG_DIR`.
- **FR-005**: `PRUEFER_CLAUDE_CONFIG_DIR` never reaches the review subprocess; a test pins that against `reviewEnvNeverPassthrough`'s refusal of the `PRUEFER_` prefix.
- **FR-006**: A new `Config` field carries a `reload:` tag. On SIGHUP reload the value is re-resolved from all layers: `.env` is re-read as data, and the ambient snapshot is the one taken at process start. Reviews dispatched after the reload use the new value; a review already running keeps the value it started with.
- **FR-007**: The field is classified `live` unless planning shows a reason it cannot be, and the `cmd/pruefer/README.md` reload table is updated to match.
- **FR-008**: At startup, and on every reload where the effective directory changed, one log line states the effective directory, its account email when readable, and its source. When nothing resolved, the line states that reviews use Claude Code's default profile. This line is emitted even when unset.
- **FR-009**: When two or more sources supply different directories, a warning names every source, its directory and its account email, and the winner, at startup and on reload. It is logged through `logf`, and appears in the TUI log view when the TUI is on.
- **FR-010**: `PRUEFER_CLAUDE_CONFIG_DIR` follows Pruefer's existing `PRUEFER_*` conventions as documented in the configuration reference. It is not added to the engine's `internal/workerenv`.
- **FR-011**: `claude_config_dir` is an operator-only key. In a reviewed repo's `.pruefer/config.yaml` it is ignored with the existing operator-only-key warning (the behaviour for every key outside the "May" list), and a test pins this.
- **FR-012**: `cmd/pruefer/README.md` documents the key, flag and env var in the configuration reference; the precedence and why the ambient variable sits below YAML; the reload classification; and a multi-instance example in "The per-account model" using `claude_config_dir` instead of an exported variable. The "What a repo may and may not set" table lists the key under "Everything else".

### Key Entities *(if applicable)*

- **Effective Claude profile directory**: the single directory resolved from the layered sources, with its winning source, the list of other contributing sources, and its account email when readable.
- **Ambient snapshot**: the inherited `CLAUDE_CONFIG_DIR` captured once at process start, before `.env` is loaded, preserved across re-exec.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With ambient = A, `.env` = B and no YAML key, the review process environment holds exactly `CLAUDE_CONFIG_DIR=B`.
- **SC-002**: With ambient, `.env` and YAML all different, the documented order holds, and the flag and `PRUEFER_CLAUDE_CONFIG_DIR` each beat YAML.
- **SC-003**: A reviewed repo's `claude_config_dir` never changes any review's environment and always produces the operator-only-key warning.
- **SC-004**: A reload test changes the YAML value, sends a reload, and observes the next review's `CLAUDE_CONFIG_DIR` change and the new effective account logged; the ambient snapshot is not re-read.
- **SC-005**: The startup line and the disagreement warning are asserted on captured log output.
- **SC-006**: Non-vacuity: with the FR-004 override removed, the A/B test's review-environment assertion fails. The PR records this.
- **SC-007**: An operator can configure one daemon per account with no wrapper script, using only the documented configuration.

## Assumptions

- #2293 is merged and the shared resolver and account-email lookup are available to Pruefer without changes to them.
- The ambient snapshot and `.env`-as-data handling follow the mechanism #2293 established for the engine; this issue only adopts it for Pruefer.
- `PRUEFER_CLAUDE_CONFIG_DIR` and `claude_config_dir` follow existing Pruefer naming conventions; `.pruefer/config.yaml` is the default config path.
- No canonical Fabrik doc page changes, so no `docs/llms-full.txt` regeneration is expected; if one does change, it is regenerated.
- Pruefer is outside `tests/sim`, so there is no sim twin; the PR says so.

## Out of Scope *(optional)*

- The resolver itself and every engine change (#2293).
- Making `claude_config_dir` repo-settable: a reviewed repo must never choose the billing account.
- Validating the profile's login state.
- Closing report #2025: the PR mentions #2025 by bare number only and uses no closing keyword (`Closes`/`Fixes`/`Resolves`) near it; this issue's own number gets `Closes`.

## Source References *(optional)*

- Report #2025 (Pruefer half of #1807); #2293 (shared resolver).
- `pruefer/execute.go`, `pruefer/config.go`, `pruefer/claude.go`, `pruefer/review_env.go`, `pruefer/upgrade.go`, `internal/selfupgrade/release.go`, `config/config.go`, `cmd/pruefer/README.md`, `pruefer/reload_test.go`
