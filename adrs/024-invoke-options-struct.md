# ADR 024: InvokeOptions Struct for Per-Issue Claude Invocation Overrides

**Status**: Accepted  
**Date**: 2026-04-13

## Context

The `ClaudeInvoker` interface and underlying `InvokeClaude` / `InvokeClaudeForComments`
functions accept a `modelOverride string` parameter that lets a per-issue `model:` label
supersede the stage YAML's `model` field. When adding a second per-issue override
(`effort:` label → `CLAUDE_CODE_EFFORT_LEVEL`), there was a choice:

1. **Individual parameters**: Add `effortOverride string` alongside `modelOverride string`,
   growing the signature from N to N+1 for every future override. Each addition requires
   the same 11+ file churn: interface definition, `RealClaudeInvoker`, `mockClaudeInvoker`,
   `invokeFn` type, `claudeInvokeCall` struct, and every test closure.

2. **Options struct**: Bundle all per-issue overrides into `InvokeOptions{ModelOverride,
   EffortOverride string}`. The same 11-file churn happens once; every future override is
   a zero-churn field addition.

## Decision

Introduce `InvokeOptions` and replace the `modelOverride string` parameter in both
`ClaudeInvoker.Invoke` and `ClaudeInvoker.InvokeForComments` with `opts InvokeOptions`.

The struct is defined in `engine/interfaces.go`, co-located with the `ClaudeInvoker`
interface it parameterises.

## Consequences

**Positive**:
- Future per-issue overrides (e.g. `DisableAdaptiveThinking`, additional env var
  controls) are zero-churn: add a field to `InvokeOptions`, update the one place that
  populates it, done.
- Call sites read as self-documenting named fields (`InvokeOptions{ModelOverride: "opus"}`)
  rather than positional strings.
- `mockClaudeInvoker.calls` records the full `InvokeOptions` for each invocation, making
  assertions on multiple override dimensions straightforward in tests.

**Negative / Trade-offs**:
- `InvokeOptions{}` (zero value) is a slightly more verbose call site than `""` when no
  overrides are needed. This is a minor ergonomic cost.
- Adding a field to `InvokeOptions` and forgetting to populate it at call sites produces
  no compile error — silent zero values. Reviewers must check that new fields are wired
  end-to-end. (Individual parameters would produce a compile error if a new parameter
  were not supplied — but this benefit is outweighed by the ongoing churn cost.)

## Pattern for Future Overrides

When adding a new per-issue label override that affects Claude invocations:

1. Add a field to `InvokeOptions` in `engine/interfaces.go`.
2. Add `extractXxxOverride` in `engine/item.go`, following `extractEffortOverride`.
3. Populate the field at both call sites (`item.go` and `comments.go`).
4. Apply it in `engine/claude.go` — via `buildClaudeArgs` for CLI flags or `buildClaudeEnv`
   for environment variables.
5. Write tests following `TestExtractEffortOverride` and
   `TestExtractEffortOverrideMultipleLabelsPrecedence` as templates.
