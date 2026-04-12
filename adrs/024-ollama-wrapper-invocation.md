# ADR 024: Ollama Wrapper for Open-Weight Model Invocations

**Status**: Accepted  
**Date**: 2026-04-12

## Context

Fabrik invokes Claude Code by shelling out to `claude <args>` (ADR 005). All
invocations are Anthropic-hosted; there is no mechanism to route a stage to an
open-weight model running through a local or cloud-hosted Ollama instance.

Claude Code supports Ollama as an API provider via the `ollama launch claude`
CLI wrapper, which sets `ANTHROPIC_BASE_URL` to Ollama's local API endpoint and
`ANTHROPIC_AUTH_TOKEN=ollama` before exec-ing `claude`. Arguments after `--`
are passed through verbatim to `claude`.

Two alternative implementation strategies were considered:

1. **Direct env var injection**: Detect Ollama mode and inject
   `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` directly into the
   `claude` subprocess environment inside Fabrik's `runClaude` function.

2. **`ollama launch claude` wrapper**: Detect Ollama mode and change the
   invocation from `claude <args>` to
   `ollama launch claude --model <model> --yes -- <args>`.

## Decision

Use the `ollama launch claude` CLI wrapper (option 2). When Ollama mode is
active, `runClaude` builds:

```
ollama launch claude --model <model> --yes -- <original claude args>
```

instead of `claude <original claude args>`.

Ollama mode is activated via:
- An `ollama:<model>` issue label (e.g. `ollama:kimi-k2.5:cloud`), which
  overrides both `model:` labels and stage-level `model:` YAML.
- A `model: ollama:<model>` entry in a stage's YAML config.

The `ollama:` prefix is stripped to extract the model name; the remainder is
passed verbatim to `--model` (supporting colons in model names like
`kimi-k2.5:cloud`).

## Rationale

**Prefer the wrapper over direct env var injection** because:

- **Delegated configuration**: The `ollama` CLI maintains the canonical list of
  env vars needed to route Claude Code to Ollama. Using the wrapper means
  Fabrik does not need to track which variables are required, their correct
  values, or how they change across Ollama versions.
- **Confirmed stdin passthrough**: The `ollama launch claude` wrapper passes
  `os.Stdin` straight through to `claude` without buffering. Fabrik delivers
  prompts via `cmd.Stdin = strings.NewReader(prompt)`, which works unchanged.
- **Simplicity**: The invocation change is confined to three lines in
  `runClaude`. No new env-building logic, no variable injection, no Ollama
  version detection.
- **Future-proof**: If Ollama changes its env var names or adds new required
  variables, the `ollama` binary absorbs the change automatically.

**The `--yes` flag** is required for headless/CI use. Without it, `ollama
launch claude` prompts interactively when the requested model is not yet
pulled. `--yes` auto-pulls and suppresses the prompt.

## Consequences

- **New binary dependency**: When `ollama:<model>` is used (via label or
  YAML), the `ollama` CLI must be installed and on `PATH`. Fabrik does not
  auto-install it. Missing `ollama` surfaces as an `exec: "ollama": executable
  file not found in $PATH` error.
- **Model availability**: Fabrik cannot verify that a model is available before
  invocation. `--yes` triggers auto-pull on first use; if the pull fails (e.g.,
  network unavailable), the error surfaces from `ollama`.
- **Session cross-provider contamination**: If an issue switches between
  Anthropic-mode and Ollama-mode runs mid-pipeline, the prior session file may
  be stale. The existing stale-session-ID handler in `runClaude` (which removes
  the session file and retries) covers this case automatically.
- **`--model` suppression**: In Ollama mode, the `--model` flag is NOT passed
  to `claude` (it would be interpreted as a Claude API model name and fail).
  The model is passed to `ollama launch claude --model` instead.
- **Interface unchanged**: The `ClaudeInvoker` interface signature is
  unchanged. Ollama mode detection is encapsulated inside
  `InvokeClaude`/`InvokeClaudeForComments`.
