# fabrik-workflows — Claude Code plugin (worker side)

Stage workflow skills for the [Fabrik](https://fabrik.shadoworg.dev) SDLC pipeline orchestrator. **This plugin loads into the Claude Code workers that the Fabrik engine spawns to execute pipeline stages.** It is not the plugin a human user installs in their interactive Claude Code session — for that, see `plugin/fabrik` (the user-facing PM-buddy plugin).

## What This Plugin Provides

Stage skills, one per pipeline stage (plus a comment-processing variant for each):

| Skill | Stage | Purpose |
|-------|-------|---------|
| `fabrik-workflows:fabrik-specify` | Specify | Refine rough issues into clear specs |
| `fabrik-workflows:fabrik-research` | Research | Explore codebase and surface technical findings |
| `fabrik-workflows:fabrik-plan` | Plan | Design implementation approach with task checklist |
| `fabrik-workflows:fabrik-implement` | Implement | Execute the plan: code, test, commit, push |
| `fabrik-workflows:fabrik-review` | Review | Review implementation, fix issues, prepare PR |
| `fabrik-workflows:fabrik-validate` | Validate | Final quality gate: verify requirements met |

Each stage also has a `-comment` variant used by the engine when processing user comments on an in-flight issue.

## How It Works

This plugin source tree is embedded in the `fabrik` binary via `//go:embed`. On `fabrik init` the embedded tree is extracted to `.fabrik/plugin/`; on `fabrik upgrade` the same tree is re-extracted (overwriting). The engine then passes `--plugin-dir .fabrik/plugin/` to every worker invocation, and the stage YAML names which skill to load via the `skill:` / `comment_skill:` keys.

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
| `FABRIK_BLOCKED_ON_INPUT` | Signal the stage needs user input before continuing |
| `FABRIK_SUMMARY_BEGIN` / `END` | Brief summary for issue (when output goes to PR) |
| `FABRIK_ISSUE_UPDATE_BEGIN` / `END` | Updated issue body from comment processing |

## Editing These Skills

Edit the source under `plugin/fabrik-workflows/skills/<name>/SKILL.md` in the Fabrik repo. The deployed copy at `.fabrik/plugin/` in any project is gitignored and will be silently overwritten on the next `fabrik upgrade` — edits there are lost.

## More Information

- [Stage Lifecycle](https://fabrik.shadoworg.dev/stage-lifecycle) — full engine lifecycle documentation
- [User Guide](https://fabrik.shadoworg.dev/USER_GUIDE) — Fabrik setup and usage
- [State Machine](https://fabrik.shadoworg.dev/state-machine) — engine state transitions and label semantics
