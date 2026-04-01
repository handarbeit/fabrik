# ADR 004: Stage-Based Pipeline with YAML Configs

## Status

Accepted

## Context

Fabrik needs to map workflow steps to Claude Code configurations. Each step
in the SDLC has different goals, prompts, and constraints.

## Decision

Define stages as YAML files, one per file, in a configurable directory. Each
stage maps directly to a column/status on the GitHub Project board by name.

## Rationale

- **Declarative**: YAML is human-readable and easy to edit. No code changes
  needed to tweak prompts or add stages.
- **Composable**: Each stage is independent. Add, remove, or reorder stages
  by editing files and board columns.
- **Version-controlled**: Stage configs live in the repo alongside the code,
  so prompt engineering is tracked in git.
- **Separation of concerns**: The driver (Go code) is generic. All
  domain-specific behavior lives in the stage YAML.
- **Per-user customization**: Users can copy examples to their own directory
  and customize without affecting others.

## Stage Fields

```yaml
name: Research          # Matches board column name (case-sensitive)
order: 1                # Pipeline ordering
prompt: |               # Main stage processing prompt
comment_prompt: |       # Comment review prompt (optional)
model: sonnet           # Claude model (optional)
max_turns: 10           # Turn limit (optional)
allowed_tools: []       # Tool restrictions (optional)
completion:             # How to detect stage completion
  type: claude          # claude | tasklist | label | approval
auto_advance: false     # Override yolo mode per-stage (optional)
```

## Default Pipeline

The example configs implement: Research -> Plan -> Implement -> Review -> Validate.
This is opinionated but replaceable — users define their own stages.

## Consequences

- Stage names must exactly match board column names (case-sensitive).
- Adding a stage requires both a YAML file and a board column.
- No conditional branching between stages (linear pipeline only, for now).
