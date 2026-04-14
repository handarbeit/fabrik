# ADR 013: `.fabrik/config.yaml` as Project-Level Config Source

## Status

Accepted

## Context

Fabrik originally used a single `.env` file for all configuration — secrets (tokens) and non-secrets (owner, repo, project number) alike. This meant every collaborator on a project had to recreate the full configuration, and it could not be committed to the repo without risking token leakage.

The pattern established by tools like `docker-compose.yml`, `.goreleaser.yaml`, and `buf.yaml` separates project-level, non-secret settings into a committed config file, leaving only secrets in a gitignored per-developer file.

## Decision

Add `.fabrik/config.yaml` as an optional, git-committable config file for non-secret project settings. This file:

- Is loaded from the current working directory (`CWD/.fabrik/config.yaml`)
- Is **optional** — if absent, behavior is identical to before
- Should be **committed to git** so project settings travel with the repo
- Holds only non-secret settings; tokens stay in `.env`

The new config layer integrates into the existing precedence stack:

```
CLI flag > shell env var > .env > .fabrik/config.yaml > built-in default
```

## Config Resolution Detail

Config is resolved in `Execute()` using an "at-default" heuristic for each setting. After `flag.Parse()`, each setting is at its declared default value. The resolution order then is:

1. If an env var is set (from `.env` or shell), use it
2. Else if the setting is still at its flag default AND the config.yaml has a non-zero value for this setting, use the config.yaml value
3. Otherwise, keep the flag default

**Known limitation**: There is no way to distinguish between "the user explicitly passed `--poll 30`" and "poll is at its default of 30". If config.yaml has `poll: 45`, and the user passes `--poll 30` intending to override it, config.yaml will win because the flag value equals the default. This is the same limitation that exists today with env vars and is inherent to Go's `flag` package. The workaround is to set `FABRIK_POLL=30` in the environment, which is checked before config.yaml.

## Implementation

- `config.LoadProjectConfig()` reads and unmarshals `.fabrik/config.yaml` using `gopkg.in/yaml.v3` (already a dependency)
- Integer fields use `*int` (pointer) to distinguish "not set in file" from "explicitly set to zero" — critical for `max_retries` where 0 means "unlimited"
- Boolean fields use plain `bool` since all defaults are `false` and setting `false` in config.yaml is indistinguishable from "not set" but is also a no-op. **Exception**: `tui` uses `*bool` because its default is `true` — absent means "use default on", while explicit `false` means "disable".
- `config.WarnIfConfigIgnored()` prints a non-fatal stderr warning if `.fabrik/config.yaml` is listed in `.gitignore`
- `fabrik init` generates a `.fabrik/config.yaml` template (all settings commented-out with defaults) and supports interactive prompting for required fields when stdin is a TTY

## Settings Covered

| `config.yaml` key | Env var | CLI flag | Default |
|---|---|---|---|
| `owner` | `FABRIK_OWNER` | `--owner` | required |
| `repo` | `FABRIK_REPO` | `--repo` | required |
| `project` | `FABRIK_PROJECT_NUMBER` | `--project` | required |
| `user` | `FABRIK_USER` | `--user` | required |
| `stages` | `FABRIK_STAGES` | `--stages` | `./.fabrik/stages` |
| `poll` | `FABRIK_POLL` | `--poll` | `30` |
| `max_concurrent` | `FABRIK_MAX_CONCURRENT` | `--max-concurrent` | `5` |
| `max_retries` | `FABRIK_MAX_RETRIES` | `--max-retries` | `3` |
| `yolo` | `FABRIK_YOLO` | `--yolo` | `false` |
| `auto_upgrade` | `FABRIK_AUTO_UPGRADE` | `--auto-upgrade` | `false` |
| `git_ssh` | `FABRIK_GIT_SSH` | `--ssh` | `false` |
| `tui` | `FABRIK_TUI` | `--notui` | `true` |
| `terminal` | `FABRIK_TERMINAL` | *(none)* | `""` |
| `debug_output` | `FABRIK_DEBUG_OUTPUT` | `--debug-output` | `false` |

Tokens (`FABRIK_TOKEN`, `GITHUB_TOKEN`) are intentionally excluded — they must not be committed.

## Known Limitations

- **At-default collision** (documented above): explicit `--flag value` where value equals the default cannot override config.yaml. Workaround: use env var.
- **Gitignore wildcard detection**: `isInGitignore` uses substring matching. A pattern like `*.yaml` in `.gitignore` would cover `config.yaml` but the warning would not fire. This is the same limitation as the existing `.env` gitignore check.
- **No parent-directory walking**: config is only loaded from `CWD/.fabrik/config.yaml`. No upward search.

## Consequences

- New collaborators only need to provide `FABRIK_TOKEN` in a gitignored `.env` file; all other settings come from the committed `config.yaml`
- `fabrik init` now generates `config.yaml` alongside stage configs
- `.env.example` is trimmed to secrets only, with a pointer to `config.yaml`
- The `terminal` config key is a passthrough for issue #108 (configurable terminal app); it has no behavior in this issue
