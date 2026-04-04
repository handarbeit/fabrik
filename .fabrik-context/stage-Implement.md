🏭 **Fabrik — stage: Implement**
*branch: fabrik/issue-109 | commit: 5d657621 | 2026-04-03 22:29 UTC*

All 14 tasks are complete. Here's a summary of what was done:

**Tasks 1-11** (from a prior session): `config.ProjectConfig` struct with `LoadProjectConfig()` and `WarnIfConfigIgnored()`, full wiring in `cmd/root.go` (including `--max-concurrent` flag, `FABRIK_AUTO_UPGRADE`/`FABRIK_TUI`/`FABRIK_TERMINAL` env vars, config.yaml fallback layer), `cmd/init.go` template generation with interactive prompting, tests for all new code, and `.env.example` trimmed to secrets only.

**Tasks 12-14** (this session): Created ADR 013 documenting the config.yaml precedence model and known at-default limitation, updated `docs/USER_GUIDE.md` with new setup flow and expanded configuration reference, and updated `README.md` Quick Start to introduce `config.yaml` as the primary non-secret config mechanism.

Resumed implementation from a prior partial session. Tasks 1-11 were already complete (config package, cmd/root.go wiring, cmd/init.go template generation, tests, .env.example). This session completed the remaining tasks: created ADR 013 documenting the config.yaml precedence model and known limitations, updated docs/USER_GUIDE.md with the new setup flow and expanded configuration reference table, and updated README.md Quick Start to reflect config.yaml as the primary non-secret config mechanism. All tests pass, all changes committed and pushed.

---
Used 39/50 turns, 5k input / 10k output tokens.