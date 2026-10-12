# ADR 2138: A stage whose skill failed to load is paused, never completed

## Status

Accepted

## Context

For a skill-configured stage the skill *is* the stage's instructions: the prompt is only
"follow the `<skill>` skill exactly" plus issue context. When the skill does not load in
the worker, the model runs with no instructions, is still told to emit
`FABRIK_STAGE_COMPLETE`, and does. The engine marked the stage complete and advanced the
item having done none of the stage's work (#1373 saw exactly this, "silent").

ADR 2137 stops a reference that resolves to no loaded plugin from reaching a worker. What
remains is the runtime case: a skill the resolver found on disk that Claude Code still
failed to register or load. Only the worker's own transcript shows it.

## Decision

**Detection** (`engine/skill_load_detect.go`, called from `runClaude`) reads the run's
structured tool-call record, never output prose (FR-002):

- an assistant `tool_use` block named `Skill` whose `input.skill` is the invocation's own
  configured skill is an *own-skill call*; its `tool_result`, matched by `tool_use_id`,
  carries `is_error`. The result's text is never used to decide, only quoted to the
  operator.
- it is a failure only when an own-skill call failed and **none succeeded** (a failed bare
  name followed by a working qualified name means the skill loaded).
- no call at all is not a failure (a `--resume`d session may hold the skill already), nor
  is a call without a result (truncated stream), an unparseable line or a block without a
  `tool_use_id`. Lines with a non-null `parent_tool_use_id` (sub-agents) are ignored.
- names match on the base name; when both sides are plugin-qualified the plugin must be
  equal, and a bare side matches any plugin. A same-named skill of another plugin, a
  different failing skill, a prose mention or a failing non-`Skill` tool never fire.
- the configured skill is `stage.Skill` for stage dispatch and `stage.CommentSkill` for the
  comment, CI-fix and rebase paths (which swap their skill into `CommentSkill`); a stage
  with `prompt:` and no skill passes none, so the detector is inert.
- It runs after `interpretClaudeResult` and overrides only an unclassified outcome and the
  marker-wins non-zero exit; usage-limit, api-error, tools-denied, turn-limit and
  resume-failure classifications keep precedence (`skillLoadOverride`).

The assumption that a failed `Skill` call is structural was checked against a real capture
(`claude -p --output-format stream-json --verbose`, 2026-10-11, committed as
`engine/testdata/skill_load/failed_skill_call_real.ndjson`): the `tool_result` carries
`"is_error": true`, and `tool_use_result` is the string `Error: Unknown skill: …`.

**Failure shape (FR-004): an immediate pause, no counter, no new label.** The typed
`claudeerr.SkillLoadError` is handled by `handleSkillLoadFailed`: one comment through
`pauseIssue` (`fabrik:paused` + `fabrik:awaiting-input`) naming the stage, the skill, the
name the worker called when different, the plugins loaded and the sanitised error; a
warnings-panel entry (type `skill_load`, subject `<stage>.<key>`); and `EnginePaused`.
It records no `StageRetryIncremented` and no `stage:<name>:failed`, runs no `commitWIP` or
push (the run did none of the stage's work; any stray edit stays in the worktree), and the
read-only stash restore still runs before the error switch.

This differs from ADR 2137's self-healing did-not-run on purpose. There, the next
dispatch re-resolves, so a cooldown suffices. Here resolution already passed and nothing
changes between dispatches: a cooldown with no bound would re-spawn a worker every cycle.
A counter with limit 1 is the same as pausing with extra state, so the pause is immediate.
No new label is needed — `fabrik:paused` is already `needs-human` in the attention
classifier — and not reusing `fabrik:skill-unresolved` avoids its "any invocation that
reached Claude clears it" paths, which assume a pre-flight cause.

**Comment, CI-fix and rebase reinvokes** take the same pause and return the new
`didNotRunSkillLoadFailed`, so `dispatchReinvoke` refunds the pre-charged cycle (#1812);
both comment breakers are bypassed, like the usage-limit arm. The triggering comment
predates the pause, so by ADR 1813 it does not resume the item.

The panel entry is cleared by `warnings.Resolve` on the next invocation of that stage and
key that reached Claude without a skill-load failure.

## Consequences

- A worker that never loaded its skill can no longer complete a stage or advance an item.
- The operator sees the cause on the item and in the panel, and must lift the pause by
  hand (remove `fabrik:paused` and `fabrik:awaiting-input`). Resuming without fixing the
  cause costs one more worker run and a fresh pause: bounded and human-gated.
- Detection fails toward not detecting. If a Claude Code version ever reported a failed
  skill without `is_error`, detection would silently not fire; text matching is not a
  fallback because FR-002 forbids it.
- `runClaude` gained an `expectedSkill` parameter; `interpretClaudeResult` and its tests
  are unchanged.

## Non-goals

- Pre-flight resolution (ADR 2137).
- Detecting arbitrary tool errors beyond the stage's own skill.
- #1373 is a community report, mentioned for context only; this change does not close it.
