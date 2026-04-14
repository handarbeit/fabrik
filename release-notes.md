# Fabrik v0.0.36

## Fixes

- **Permission-independent of user's global Claude settings** (#361) — Fabrik now passes `--permission-mode dontAsk` and a comprehensive default `--allowedTools` set to every Claude invocation. Previously, Fabrik's behavior depended on the user's global `~/.claude/settings.json`: users without `"defaultMode": "dontAsk"` hit silent failures (e.g., "I'm unable to write files — the Claude Code session running this agent doesn't have write permissions pre-configured"). Now Fabrik is self-contained — its behavior is governed only by repo content and stage YAML, not personal config.

## Improvements

- **Default allowed-tools set** — Every Claude invocation now allows Read, Edit, Write, Glob, Grep, TodoWrite, Skill, Task, plus common dev Bash commands (git, gh, go, npm, yarn, pnpm, make, cargo, python, pip, uv, pytest, ls, cat, rm, cp, mv, mkdir, find). Stage YAML `allowed_tools` extends this default set.
- **TodoWrite added to Research/Plan/Specify stage YAMLs** — These stages need TodoWrite for task tracking during longer sessions.
- **CLAUDE.md updated** — Documents the `fabrik:unrestricted` label in the labels section. Documents the default permission posture and how `allowed_tools` extends the defaults.

## Internal

- `cut-release` skill now explicitly verifies `conclusion == "success"` on the release workflow (not just `status == "completed"`). Previous releases had silent Discussion-announcement failures that went unnoticed.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo shadoworg/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
