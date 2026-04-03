# Fabrik Plugin for Claude Code

Stage workflow skills for the [Fabrik](https://github.com/tenaciousvc/fabrik) SDLC pipeline orchestrator.

## What This Plugin Provides

Six skills, one per pipeline stage:

| Skill | Stage | Purpose |
|-------|-------|---------|
| `fabrik:fabrik-specify` | Specify | Refine rough issues into clear specs |
| `fabrik:fabrik-research` | Research | Explore codebase and surface technical findings |
| `fabrik:fabrik-plan` | Plan | Design implementation approach with task checklist |
| `fabrik:fabrik-implement` | Implement | Execute the plan: code, test, commit, push |
| `fabrik:fabrik-review` | Review | Review implementation, fix issues, prepare PR |
| `fabrik:fabrik-validate` | Validate | Final quality gate: verify requirements met |

## Installation

### For development (local testing)

```bash
claude --plugin-dir /path/to/fabrik/plugin/fabrik
```

### For regular use

Add to a Fabrik plugin marketplace and install via Claude Code's plugin manager.

## How It Works

The Fabrik engine invokes Claude Code for each stage of the pipeline. Claude auto-loads these skills from `~/.claude/skills/`. The engine injects a minimal prompt directing Claude to follow the relevant stage skill, along with the issue context (title, body, comments).

The skills contain:
- **What to do** at each stage (methodology)
- **What the engine expects** (markers, conventions)
- **What NOT to do** (scope boundaries between stages)
- **Common pitfalls** to avoid

## Fabrik Markers

Skills reference these markers that the Fabrik engine processes:

| Marker | Purpose |
|--------|---------|
| `FABRIK_STAGE_COMPLETE` | Signal that the stage finished successfully |
| `FABRIK_SUMMARY_BEGIN` / `END` | Brief summary for issue (when output goes to PR) |
| `FABRIK_ISSUE_UPDATE_BEGIN` / `END` | Updated issue body from comment processing |

## More Information

- [Stage Lifecycle](https://github.com/tenaciousvc/fabrik/blob/main/docs/stage-lifecycle.md) — full engine lifecycle documentation
- [User Guide](https://github.com/tenaciousvc/fabrik/blob/main/docs/USER_GUIDE.md) — Fabrik setup and usage
