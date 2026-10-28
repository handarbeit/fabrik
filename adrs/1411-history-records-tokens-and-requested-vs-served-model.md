# ADR 1411: History records tokens and requested vs served model

## Status

Accepted.

## Context

`history.json` recorded only turns and cost. The four token counts were computed
(`tokenUsageFromResponse`) and logged (`formatStatsLogLine`) but dropped at
`InvocationObserver`, and `StageModel` was the stage YAML string — blind to the
`model:<name>` label override and to which concrete model an alias resolved to.
Two model releases moved a production pipeline's aliases with no config change
and history could not show it.

## Decisions

1. **Tokens ride the existing `TokenUsage`.** `InvocationObserver` copies the four
   counts from `ItemState.LastTokenUsage`, the same value the log line formats,
   so history and `fabrik.log` agree by construction.
2. **`StageModel` becomes the effective requested model**, as written (alias stays
   alias, full id stays full id): `model:<name>` label over stage config, the
   precedence `buildClaudeArgs` applies. It is computed at the two real
   `InvocationRecorded` write sites (`finalizeStageOutcome`, the comment path) and
   carried as `InvocationRecorded.RequestedModel` / `ItemState.LastInvocationRequestedModel`.
   Computing it there uses the stage that actually ran and keeps the observer
   free of the logging `extractModelOverride` (the pure core is
   `modelOverrideFromLabels`). When nothing was recorded the observer falls back to
   the stage YAML value. Side effect: history *resume* (`tui/commands.go` passes
   `StageModel` to `claude --model`) now honours a `model:` label. The resolved id
   is never put in `StageModel`, so resume never passes a resolved id.
3. **`ServedModel` is a separate string on `TokenUsage`**: the sorted,
   comma-joined `modelUsage` keys. A string keeps `TokenUsage` comparable
   (`engine/localapi.go` compares it with `!=`); a joined set loses nothing in the
   multi-model case (a subagent on another model) and does not guess a "primary".
   An absent `modelUsage` gives empty = unknown; it is never defaulted to the
   requested model. `addTokenUsage` merges across the extension loop as a sorted
   union (it builds a fresh struct, so the field must be merged there explicitly).
4. **Additive schema.** Plain `int`/`string` fields without tags, like the
   surrounding fields; old files decode with zero values (tested), since a decode
   error makes `LoadHistory` drop the whole file.
5. **Cumulative on resume is recorded as-is.** On `--resume` the CLI reports
   session-cumulative tokens, cost and turns (ADR 1949). History records the same
   values as the log line; summing them across entries of one session
   double-counts. Per-invocation deltas would deliberately diverge from the log.

## Alias resolution probe

A one-off live run (`claude -p --model opus --output-format stream-json --verbose`,
2026-10-28) reported `"model":"claude-opus-5-5"` and
`"modelUsage":{"claude-opus-5-5":…}`: the `modelUsage` keys carry the resolved id,
not the alias, so no `message.model` fallback is needed.

## Out of scope

Cost/rate cards; TUI rendering of the new fields; any log line on an alias change;
`fabrik_status` `last_invocation`; merge-train episode and `Skipped` events (zero
values).
