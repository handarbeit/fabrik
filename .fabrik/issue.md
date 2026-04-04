## Summary

Add a `.fabrik/config.yaml` file for project-level, non-secret configuration that is safe to commit to git. Move all non-secret settings out of `.env` into this file, leaving only `FABRIK_TOKEN` / `GITHUB_TOKEN` in `.env`. This lets project configuration travel with the repo so collaborators only need to provide their own token. A new `terminal` setting is included to support issue #108 (configurable terminal app), which is blocked on this issue.

## Requirements

- Load `.fabrik/config.yaml` (relative to CWD) as the base configuration layer, using `gopkg.in/yaml.v3` (already a dependency)
- The config file is optional — if absent, behavior is identical to today
- Precedence order: `CLI flag > shell environment variable > .env file > .fabrik/config.yaml > defaults`
- All settings currently supported via CLI flags or env vars continue to work unchanged (no breaking changes)
- The `config` package gains a `LoadProjectConfig()` function that returns a parsed struct; `cmd/root.go` applies it before env var and flag resolution
- Add `--max-concurrent` CLI flag (currently env-var only) for parity with config.yaml and other flags
- Add `FABRIK_AUTO_UPGRADE` and `FABRIK_TUI` env var support (currently CLI flags only)
- Add `terminal` as a new config setting (string, default empty) that passes through to the TUI for use by issue #108; no behavior change in this issue beyond storing the value
- `fabrik init` generates a `.fabrik/config.yaml` template with all settings commented-out, with defaults pre-filled where possible (e.g., `# poll: 30`); required settings without defaults use placeholder comments (e.g., `# owner: your-org`)
- `fabrik init` also supports an interactive mode: if the config file does not exist and a TTY is present, prompt the user for required values (owner, repo, project, user) and write them into the generated config
- `fabrik init` skips generating `config.yaml` if file already exists; respects `--force` flag to overwrite
- `.fabrik/config.yaml` must be tracked in git — fabrik should warn (not fatal) if it is listed in `.gitignore`
- The `.env` safety check (refusal if not gitignored) remains unchanged
- Update `docs/USER_GUIDE.md` and `README.md` to document the new config file format, settings table, and updated setup flow

## Settings Mapping

| `config.yaml` key | Env var | CLI flag | Default | Notes |
|---|---|---|---|---|
| `owner` | `FABRIK_OWNER` | `--owner` | required | |
| `repo` | `FABRIK_REPO` | `--repo` | required | |
| `project` | `FABRIK_PROJECT_NUMBER` | `--project` | required | |
| `user` | `FABRIK_USER` | `--user` | required | |
| `stages` | `FABRIK_STAGES` | `--stages` | `./.fabrik/stages` | |
| `poll` | `FABRIK_POLL` | `--poll` | `30` | |
| `max_concurrent` | `FABRIK_MAX_CONCURRENT` | `--max-concurrent` *(new)* | `5` | flag added by this issue |
| `max_retries` | `FABRIK_MAX_RETRIES` | `--max-retries` | `3` | |
| `yolo` | `FABRIK_YOLO` | `--yolo` | `false` | |
| `auto_upgrade` | `FABRIK_AUTO_UPGRADE` *(new)* | `--auto-upgrade` | `false` | env var added by this issue |
| `tui` | `FABRIK_TUI` *(new)* | `--tui` | `false` | env var added by this issue |
| `terminal` | `FABRIK_TERMINAL` *(new)* | *(none)* | `""` | new passthrough setting for issue #108 |
| `debug_output` | `FABRIK_DEBUG_OUTPUT` | `--debug-output` | `false` | |

## What Stays in `.env`

| Setting | Reason |
|---|---|
| `FABRIK_TOKEN` / `GITHUB_TOKEN` | Secret — must not be committed |
| `FABRIK_USER` (optional override) | Per-developer identity when different from config.yaml |

## Scope

**In scope:**
- New `config` package function to load and parse `.fabrik/config.yaml`
- Integration in `cmd/root.go` applying config.yaml values at the base layer
- Add `--max-concurrent` CLI flag
- Add `FABRIK_AUTO_UPGRADE` and `FABRIK_TUI` env var support
- Add `terminal` as a new passthrough config setting (string)
- `fabrik init` generates `.fabrik/config.yaml` template (commented defaults + placeholders for required fields)
- `fabrik init` interactive mode: prompts for required values when no config exists and stdin is a TTY
- Documentation updates (`README.md`, `docs/USER_GUIDE.md`)

**Out of scope:**
- `no_tmux` — tmux support removed, not relevant
- `plugin_dir` — dev-only override; remains CLI flag / env var only
- Configurable terminal-app behavior in the TUI (that is issue #108's work; this issue only stores the `terminal` value)
- Removing or replacing the `godotenv` dependency
- Config file discovery beyond CWD (no parent-directory walking)

## Prior Art / Context

- Standard pattern: `docker-compose.yml`, `.goreleaser.yaml`, `buf.yaml` — project-level config files committed alongside code, with per-developer secrets in gitignored files
- Go precedence (flag > env > config file > default) is idiomatic and matches what's specified
- `gopkg.in/yaml.v3` is already a dependency (used by `stages/stages.go`), so no new dependency is needed
- Issue #108 (configurable terminal app) is explicitly blocked on this issue for the `terminal` setting

## Risks / Dependencies

- The precedence model requires careful implementation: `.env` values are currently loaded into the process environment before env var checks, so there is no code-level distinction between shell env and `.env` vars. The new layer inserts below both — config.yaml is checked after all env vars are exhausted.
- `fabrik init` interactive mode must gate on TTY detection (same pattern used elsewhere in the codebase) to remain safe in non-interactive CI environments.
- Issue #108 depends on this issue shipping the `terminal` config key before it can proceed.

---

## Research Findings

### Relevant Code

- `config/config.go` — Current config package. Contains `LoadDotenv()`, `isInGitignore()` (unexported), and `Token()`. The new `LoadProjectConfig()` function and `ProjectConfig` struct belong here (or in a new `config/project_config.go`). The unexported `isInGitignore()` can be reused directly to implement the "warn if config.yaml is in .gitignore" check.
- `cmd/root.go` — CLI entry point. The `Config` struct (cmd-local) and `Execute()` function handle all flag/env-var resolution. This is where config.yaml values must be wired in as the lowest-priority layer (after env var checks, before falling through to defaults). Currently has: `--max-concurrent` missing as a flag (hardcoded default of 5, env-var only), `FABRIK_AUTO_UPGRADE` / `FABRIK_TUI` env vars not yet checked.
- `cmd/init.go` — `runInit()` function. Extracts embedded stage YAMLs and plugin files. Needs new logic to generate `.fabrik/config.yaml` template and optionally prompt interactively. Already has `--force` flag via its own `flag.NewFlagSet`. Doesn't currently import `go-isatty`.
- `cmd/root_test.go` — Tests for `Execute()`. Uses `resetFlags()` to reset `flag.CommandLine` between tests via `flag.NewFlagSet`. Tests would need coverage for config.yaml loading and the new `--max-concurrent` flag.
- `cmd/init_test.go` — Tests for `runInit()`. Uses `os.Chdir(t.TempDir())` pattern. Tests for the new config.yaml generation and interactive mode (non-TTY path) belong here.
- `config/config_test.go` — Tests for the `config` package. Uses `chdir()` helper (unexported, defined in the test file). Tests for `LoadProjectConfig()` belong here.
- `stages/stages.go` — Shows the `gopkg.in/yaml.v3` usage pattern: `yaml.Unmarshal(data, &s)`. The `ProjectConfig` struct should follow the same YAML tag conventions (`yaml:"field_name"`).
- `tui/model.go` — `tui.New(pollSeconds int) Model`. The `terminal` string will eventually be passed here by #108. For now, it only needs to be stored in `cmd.Config`. If the engine is to carry it for #108, `engine.Config` also needs a `Terminal string` field.
- `engine/engine.go` — `engine.Config` struct. Currently has no `Terminal` field. Whether to add it now (for passthrough) or leave it for #108 is a decision point — the spec says "no behavior change in this issue beyond storing the value," suggesting storing it only in `cmd.Config` is acceptable.
- `docs/USER_GUIDE.md` — Existing docs cover CLI flags and env vars but not `config.yaml`. Sections 1 (Getting Started) and 2 (Configuration Reference) need updates. The `.env` example in section 2 needs to show only secrets.
- `README.md` — Quick Start section currently says "Create your .env file from the example / Edit .env with your GitHub token and repo details." This needs to be updated to reflect `config.yaml` as the primary non-secret config mechanism.
- `.env.example` — Currently lists all settings including non-secrets. Should be trimmed to only include `FABRIK_TOKEN` (and `GITHUB_TOKEN` fallback), with a comment pointing to `config.yaml` for the rest.
- `.gitignore` — Currently lists `.env` (line 9) but not `.fabrik/config.yaml`. The `.fabrik/worktrees/` and `.fabrik/debug/` directories are listed — `.fabrik/config.yaml` is NOT currently gitignored, which is correct for this feature.

### Architecture Notes

**Current config resolution flow in `Execute()`:**
1. `flag.Parse()` — populates `cfg` with flag values (or defaults if not passed)
2. `config.LoadDotenv()` — loads `.env` into process env via `godotenv.Load`
3. Token: flag > `FABRIK_TOKEN` > `GITHUB_TOKEN`
4. Per-setting env var checks using "at-default" heuristics:
   - Strings: `if cfg.Field == ""` → check env var
   - Booleans: `if !cfg.Flag` → check env var
   - Integers: `if cfg.Field == defaultValue` → check env var
5. Validation (required fields, token, user)
6. Stage loading
7. TUI / engine dispatch

**New config resolution flow (after this issue):**
1. `flag.Parse()` — same
2. `config.LoadDotenv()` — same
3. `config.LoadProjectConfig()` — load `.fabrik/config.yaml` (optional; nil/zero if absent)
4. Token: flag > `FABRIK_TOKEN` > `GITHUB_TOKEN` (no change; token never in config.yaml)
5. Per-setting resolution — same "at-default" heuristics, but now with a third fallback: if still at default after env var check, apply config.yaml value
6. `.fabrik/config.yaml` gitignore warning (non-fatal)
7. Validation, stage loading, dispatch — unchanged

**Default detection heuristics (existing pattern, extended to config.yaml):**

The code detects "user didn't explicitly set this flag" by comparing to the declared default:

| Setting | Default | Heuristic |
|---|---|---|
| `owner`, `repo`, `user`, `stages` | `""` or `./.fabrik/stages` | `if cfg.Field == ""` or `== "./.fabrik/stages"` |
| `yolo`, `auto_upgrade`, `tui`, `debug_output` | `false` | `if !cfg.Flag` |
| `poll` | `30` | `if cfg.PollSeconds == 30` |
| `max_concurrent` | `5` | `if cfg.MaxConcurrent == 5` (after flag is added) |
| `max_retries` | `3` | `if cfg.MaxRetries == 3` |

Config.yaml values apply only when the env var is also absent. This means: if a user passes `--poll 30` explicitly AND config.yaml has `poll: 45`, the poll stays at 30 (correct — flag wins). But if they pass `--poll 30` and NEITHER env var nor config.yaml has poll set, poll is 30 by default anyway. There is no way to distinguish explicit `--poll 30` from the default `30`, but this is an inherent limitation of Go's `flag` package and is consistent with the current env-var layer behavior.

**`go-isatty` availability**: Already a dependency (`go.mod` line 12, imported in `cmd/root.go`). `cmd/init.go` currently doesn't import it but can do so freely.

**Interactive prompting pattern**: `cmd/root.go` already uses `isatty.IsTerminal(os.Stdin.Fd())` for TUI gating. The same check gates interactive `init` mode. Input reading uses `bufio.NewReader(os.Stdin)` + `ReadString('\n')` (stdlib, no new deps).

#### Relevant ADRs

None of the 12 ADRs directly conflict with or are directly relevant to this feature. The config file approach is consistent with ADR 001 (single binary, minimize deps — we reuse an existing dep) and ADR 004 (YAML configs — we extend the pattern).

### Constraints

1. **`flag.Parse()` runs before config.yaml can be loaded**: Config.yaml must be applied after `flag.Parse()` as a "extended default" layer, not as true flag defaults. This is the same approach used for env vars today.

2. **No code-level distinction between shell env and `.env` vars**: Once `godotenv.Load` runs, both sources are in `os.Getenv`. Config.yaml correctly sits below both.

3. **`isInGitignore()` is unexported**: The new "warn if config.yaml is gitignored" check must live in the `config` package (same package as the unexported function), either in `config.go` or a new `config/project_config.go`. Export a `WarnIfConfigIgnored()` function or include the check inside `LoadProjectConfig()`.

4. **`cmd/init.go` has its own `flag.NewFlagSet`**: The `--force` flag is already defined there. No changes to flag parsing in init; just add the new config.yaml generation logic.

5. **`--max-concurrent` flag addition**: Currently the only flag missing from the settings table. Adding it means the `MaxConcurrent` field (currently set by hardcoded `cfg.MaxConcurrent = 5` followed by env var check) moves to `flag.IntVar`. The "at-default" check for config.yaml becomes `if cfg.MaxConcurrent == 5`.

6. **`terminal` passthrough**: The spec says "no behavior change beyond storing the value." The `cmd.Config` struct needs `Terminal string`. Whether `engine.Config` also needs it (for #108) is technically out of scope for this issue but worth noting for the planner — if the TUI needs it, it has to flow through `engine.Config → tui.New()`.

7. **`.fabrik/config.yaml` gitignore warning**: The current `.gitignore` does NOT gitignore `.fabrik/config.yaml` (it only ignores `.fabrik/worktrees/` and `.fabrik/debug/`), so the warning will not fire for this project's own config. The check needs to specifically test for the config file path, not just the filename.

### Risks

- **Integer default collision**: If a user sets `poll: 30` explicitly in config.yaml and also sets `FABRIK_POLL=30` in env, both are 30 — no issue. But if they set `poll: 45` in config.yaml and also set `FABRIK_POLL=30`, the env var wins (correct). The only edge case is if they pass `--poll 30` explicitly to override a config.yaml `poll: 45` — the flag wins because env var check doesn't change `cfg.PollSeconds` from 30, but config.yaml check also sees `cfg.PollSeconds == 30` and would override to 45. This is a precedence bug: explicit flag `--poll 30` can't beat config.yaml `poll: 45`.

  **Resolution**: This is the same limitation as with env vars today (can't tell `--poll 30` explicit from default). The spec acknowledges this pattern is established. The planner should document this known limitation. The workaround is to set env var `FABRIK_POLL=30` which wins over config.yaml.

- **Interactive mode and test safety**: `cmd/init_test.go` tests call `runInit()` directly. If the interactive mode check fires in tests (non-TTY environment), it must be a no-op (no prompt, just generate the commented-out template). This is safe because `isatty.IsTerminal(os.Stdin.Fd())` returns false in test environments.

- **`.fabrik/config.yaml` path for gitignore check**: The gitignore check should look for `.fabrik/config.yaml` specifically (not just `config.yaml`). The existing `isInGitignore()` function searches for a filename substring — it will need to be called with the full relative path `.fabrik/config.yaml` or extended to handle path prefixes.