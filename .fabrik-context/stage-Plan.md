🏭 **Fabrik — stage: Plan**
*branch: fabrik/issue-109 | commit: 9d1c8386 | 2026-04-03 22:10 UTC*

Now I have everything I need. Let me write the plan.

## Implementation Plan

### Approach

The work splits naturally into three areas:
1. **`config` package** — new `ProjectConfig` struct + `LoadProjectConfig()` + `WarnIfConfigIgnored()` 
2. **`cmd/root.go`** — wire in the new config layer, add missing flag/env-var support
3. **`cmd/init.go`** — generate the config.yaml template, add interactive mode

**Key design decision**: Use `*int` (pointer) for integer fields in `ProjectConfig` and plain `bool` for booleans. This gives clean nil-vs-set semantics for integers (especially `max_retries` where 0 is a valid value meaning "unlimited") without the at-default collision risk. Booleans use plain `bool` since all defaults are `false` and setting `false` in config.yaml is a no-op anyway.

**Integration order in `Execute()`**: Load config.yaml immediately after `LoadDotenv()`, before the per-setting env var checks. For each setting, check env var first; if env var is absent AND the setting is still at flag-default, apply the config.yaml value. This preserves the established `flag > env > config.yaml > default` precedence.

**`Terminal` field scope**: Store only in `cmd.Config` — not `engine.Config`. Issue #108 will thread it through when needed. Supports `FABRIK_TERMINAL` env var.

**`isInGitignore` reuse**: The existing unexported function works for `.fabrik/config.yaml` as-is. Export a thin `WarnIfConfigIgnored()` wrapper in `config/config.go` that logs to stderr (non-fatal).

**No new dependencies**: `gopkg.in/yaml.v3` and `go-isatty` are already present.

---

### New/Modified Files

| File | Change |
|------|--------|
| `config/config.go` | Add `ProjectConfig` struct, `LoadProjectConfig()`, `WarnIfConfigIgnored()` |
| `config/config_test.go` | Add tests for `LoadProjectConfig()` |
| `cmd/root.go` | Add `Terminal` to `Config` struct; add `--max-concurrent` flag; add `FABRIK_AUTO_UPGRADE`, `FABRIK_TUI`, `FABRIK_TERMINAL` env var checks; wire `LoadProjectConfig()` and `WarnIfConfigIgnored()` |
| `cmd/root_test.go` | Add tests for `--max-concurrent` flag and config.yaml integration |
| `cmd/init.go` | Add config.yaml template generation and interactive prompting |
| `cmd/init_test.go` | Add tests for config.yaml generation (non-TTY path) |
| `.env.example` | Trim to `FABRIK_TOKEN`/`GITHUB_TOKEN` only, add pointer to config.yaml |
| `docs/USER_GUIDE.md` | Update Getting Started and Configuration Reference sections |
| `README.md` | Update Quick Start to introduce config.yaml as primary non-secret config |

---

### Key Decisions

- **Pointer integers in `ProjectConfig`**: `*int` for `ProjectNum`, `Poll`, `MaxConcurrent`, `MaxRetries`. Nil = not present in file. This avoids the collision between "user set 0 in config.yaml" and "field not in config.yaml" — critical for `MaxRetries` where 0 is valid.
- **Plain bool in `ProjectConfig`**: All bool defaults are `false`. Setting `false` in config.yaml is indistinguishable from "not set" but is also a no-op, so plain `bool` is fine.
- **Config.yaml generated but not embedded**: The template is a string constant in `cmd/init.go`; it does not need to be an embedded file.
- **`WarnIfConfigIgnored` location**: In `config/config.go` (same package as `isInGitignore`), exported. Called from `cmd/root.go` after loading config. If `.fabrik/config.yaml` doesn't exist, the warning is silently skipped.
- **Interactive mode**: Prompts only when `isatty.IsTerminal(os.Stdin.Fd())` is true AND config.yaml does not exist AND `--force` was not passed. In all other cases, write the all-commented-out template. The interactive values are written into the non-commented fields of the generated file.
- **`Terminal` in `engine.Config`**: Not added in this issue. Only `cmd.Config` stores it. ADR not needed for this deferral — it's explicitly called out in scope.

---

### ADR Worthiness

The addition of `.fabrik/config.yaml` as a new config source with defined precedence (flag > env > config.yaml > default) warrants an ADR. A new contributor needs to understand why three sources exist, what order they resolve in, and why the at-default heuristic has a known limitation. Add the ADR creation to the task checklist; implementer should pick the next number from `adrs/` at implementation time.

---

### Task Checklist

- [ ] Task 1: Add `ProjectConfig` struct (with pointer integer fields) and `LoadProjectConfig()` to `config/config.go`; import `gopkg.in/yaml.v3`
- [ ] Task 2: Add `WarnIfConfigIgnored()` to `config/config.go` (calls `isInGitignore(".fabrik/config.yaml")`, prints to stderr, non-fatal)
- [ ] Task 3: Add tests for `LoadProjectConfig()` in `config/config_test.go` (missing file returns zero-value struct; valid YAML parses correctly; integer and bool fields; unknown keys ignored)
- [ ] Task 4: Add `--max-concurrent` CLI flag in `cmd/root.go` via `flag.IntVar`; remove the hardcoded `cfg.MaxConcurrent = 5` line and its env-var block (replaced in task 6)
- [ ] Task 5: Add `Terminal string` to `cmd.Config` struct in `cmd/root.go`
- [ ] Task 6: Wire `LoadProjectConfig()` into `Execute()` after `LoadDotenv()`; add config.yaml fallback to every per-setting env-var check block (including new `FABRIK_AUTO_UPGRADE`, `FABRIK_TUI`, `FABRIK_TERMINAL` env var checks); call `WarnIfConfigIgnored()`
- [ ] Task 7: Add tests for config.yaml integration in `cmd/root_test.go` (config.yaml values apply when env/flag absent; env var beats config.yaml; `--max-concurrent` flag works)
- [ ] Task 8: Add config.yaml template constant and `writeConfigTemplate()` helper to `cmd/init.go`; call it from `runInit()` (skip if file exists and `--force` not set)
- [ ] Task 9: Add interactive prompting to `cmd/init.go` — when stdin is a TTY and config.yaml doesn't exist, prompt for owner/repo/project/user and write their values into the generated file (non-commented); gate on `isatty.IsTerminal`
- [ ] Task 10: Add tests for config.yaml generation in `cmd/init_test.go` (non-TTY: template written with all settings commented; file not overwritten without `--force`; `--force` overwrites)
- [ ] Task 11: Update `.env.example` to keep only `FABRIK_TOKEN` / `GITHUB_TOKEN`; add a comment pointing to `.fabrik/config.yaml` for other settings
- [ ] Task 12: Create ADR NNN: `.fabrik/config.yaml` as project-level config source (document precedence order and at-default heuristic limitation)
- [ ] Task 13: Update `docs/USER_GUIDE.md` — Getting Started section (setup flow) and Configuration Reference (settings table, config.yaml format, `.env` trimmed example)
- [ ] Task 14: Update `README.md` Quick Start to introduce `config.yaml` as primary non-secret config; update setup instructions

---

### Risks

- **`MaxRetries` at-default collision**: If a user explicitly passes `--max-retries 3` and config.yaml has `max_retries: 10`, the config.yaml value will win because the flag default is 3. Same known limitation as env vars. Workaround: set `FABRIK_MAX_RETRIES=3`. Document in the ADR.
- **Interactive mode in CI**: `isatty.IsTerminal` returns false in CI — no prompts fire. Safe by construction.
- **`isInGitignore` with wildcard gitignore patterns**: `*.yaml` in `.gitignore` would cover `config.yaml` but `isInGitignore(".fabrik/config.yaml")` won't detect it. Acceptable — same limitation exists for `.env` check. Note in ADR.
- **`--max-concurrent` flag removal of hardcoded block**: The current `cfg.MaxConcurrent = 5` + env-var block must be fully replaced by the new `flag.IntVar` + unified resolution in task 6. Implementer must not leave the hardcoded block in place.

---
Used 8/50 turns, 0k input / 5k output tokens.