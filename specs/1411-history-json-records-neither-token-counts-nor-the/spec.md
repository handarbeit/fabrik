## Problem

`history.json` records neither the token counts nor the model that actually served an invocation, which makes it impossible to reconstruct cost or attribute work to a model after the fact.

Two distinct defects, same struct and same write path:

**1. Token counts are computed, logged, then dropped.** `tokenUsageFromResponse` (`engine/claude.go:1850-1863`) sums `InputTokens`, `OutputTokens`, `CacheCreationTokens` and `CacheReadTokens` across `ModelUsage`, and `formatStatsLogLine` (`engine/claude.go:1624-1638`) writes them to `fabrik.log`. But `InvocationObserver.OnChange` (`engine/observers.go:88-115`) copies only `TurnsUsed`, `MaxTurns` and `CostUSD` into `tui.JobCompletedEvent`, so `tui.HistoryEntry` (`tui/model.go:14-31`) has no token fields at all. The data exists upstream (and already lives on `itemstate.TokenUsage` at `internal/itemstate/itemstate.go:317-326`, which has all four fields) and is discarded at the history boundary.

**2. `StageModel` is a static config label, not the effective or resolved model.** `engine/observers.go:96-99` sets it from `stages.FindStage(...).Model` — the stage YAML string. It therefore:
- does not reflect the per-issue `model:<name>` label override (which genuinely changes what is invoked, via `extractModelOverride`, `engine/item.go:1922+`). An issue run with `model:opus` on a stage configured `sonnet` is recorded as `sonnet`.
- cannot reflect what a gateway actually served.
- cannot reflect which concrete model an **alias** resolved to. Stage configs commonly pin aliases (`model: opus`, `model: sonnet`), and Claude Code resolves an alias to its current latest model. When a new model release moves an alias, every run silently changes model with no config change, and `history.json` still reads `"opus"` / `"sonnet"` on both sides of the change.

Reported by @bdueck in #1385 and discussion #1386, verified against the code. Their observation: "history.json records turns honestly, so this is the portable one" — turns are currently the *only* trustworthy usage figure in history.

Additional evidence from @jmatthewpryor (on this issue): two model releases moved a production pipeline's aliases with no config change (`opus`: `claude-opus-5` → `claude-opus-5-5`; `sonnet`: `claude-sonnet-5` → `claude-sonnet-5-5`, the latter at 2026-09-28 22:35 → 22:36 UTC). `history.json` recorded `StageModel: "opus"` / `"sonnet"` on both sides. Per-run Implement cost moved from $13.08 to $10.51 and turns from 97 to 68 across the change, which confounded an unrelated before/after efficiency comparison; the effects could only be separated by grepping worker transcripts.

## Summary

Extend `history.json`'s per-invocation record to carry the four token counts Fabrik already computes internally, and to record two distinct model values: the **requested model** (the resolved `model:<name>` label override, or stage config when no override is present — as written, alias or full id) and the **served model** (the concrete, resolved model id reported by the Claude response, e.g. `claude-opus-5-5` when the requested value was the alias `opus`). Keeping both makes an alias moving to a new model, or a gateway rewriting the model, visible in the engine's own history. This makes `history.json` a complete, self-consistent usage record without requiring cost/pricing logic, which is explicitly out of scope here.

## Requirements

- Add `InputTokens`, `OutputTokens`, `CacheCreationTokens`, `CacheReadTokens` to `tui.HistoryEntry` and `tui.JobCompletedEvent`, and carry them through `InvocationObserver.OnChange` from `st.LastTokenUsage` (already populated with all four fields).
- Record the **effective requested model** on the event: resolve the same precedence the invoker uses (`model:<name>` label override > stage YAML), not stage config alone. The value is recorded as written — an alias stays an alias, a full model id stays a full id (a full id in a stage's `model:` is passed to `claude --model` verbatim today, so this is a supported configuration).
- Where the Claude response reports the model(s) that actually served the invocation (`ModelUsage` map keys carry model identifiers), record that as a separate, authoritative **served model** value — the **resolved** concrete model id, not the alias — and never overwrite the requested/configured value. Requested-vs-served is the distinction that makes reconciliation possible on gateway setups and after alias moves.
- Preserve backward compatibility when loading older `history.json` files: missing new fields must decode as their zero values, and `LoadHistory` must not error or drop entries because of the new fields.

## Scope

**In:** `engine/observers.go`, `tui/model.go`, `tui/events.go`, `tui/history.go`, and any test fixtures for history serialization.

**Out:**
- Cost computation and rate cards — that is the dependent issue. This change only makes the underlying data available.
- The TUI's rendering of the new fields — optional, may be deferred.
- A log line (or any other notification) when the engine first sees a new resolved model id for a stage, i.e. an alias → id change. Recording the served model in each `history.json` entry is sufficient to see an alias move after the fact. No follow-up issue is planned.
- Closing #1385 or #1386 — do not use closing keywords for either in the PR body; the community report is closed manually once a release ships.

## Acceptance Criteria

- [ ] `history.json` entries contain input, output, cache-creation and cache-read token counts for new invocations.
- [ ] The recorded token counts match what `formatStatsLogLine` emits to `fabrik.log` for the same invocation.
- [ ] An issue carrying `model:opus` on a stage configured `model: sonnet` records the effective requested model, not `sonnet`.
- [ ] Requested model and served model are distinguishable in the record (a gateway rewriting the model, or an alias resolving to a concrete id, does not silently overwrite the requested value).
- [ ] A run whose requested model is an alias (e.g. `opus`) records the resolved concrete id (e.g. `claude-opus-5-5`) as the served model, so an alias moving to a newer model is visible as a change in the served value across runs with an unchanged requested value.
- [ ] An existing `history.json` written before this change still loads without error.
- [ ] Tests cover: token fields round-tripping to disk, effective-model resolution under a `model:` label, served-model recording distinct from the requested value, and decoding a pre-change history file.

## Prior Art / Context

- Community report #1385 and discussion #1386 (measurements and the reconciliation argument).
- Additional field evidence from @jmatthewpryor on this issue: alias resolution changed models across two releases with `history.json` unable to show it; resolved ids were recoverable only from worker transcripts (`message.model`).
- Passthrough of full model ids is confirmed by source (`engine/claude.go:1091-1096` at v0.0.82: a `model:` label override takes precedence, otherwise `--model <stage.Model>`; `stages/stages.go:33` is a plain string with no alias table or validation). Operators can therefore pin full ids today to make model changes deliberate, but that is a workaround — it doesn't help anyone still using aliases, and history should still show what actually served each run.
- `internal/itemstate/itemstate.go:317-326` — `TokenUsage` already carries all four token fields; confirmed present and populated on `st.LastTokenUsage`.
- Confirmed via codebase inspection: `tokenUsageFromResponse` and `formatStatsLogLine` already compute/log the values described above; `InvocationObserver.OnChange` is the sole point where they are currently dropped before reaching the TUI/history layer.

## Risks / Dependencies

- **Schema change is additive-only.** Old `history.json` files must still decode; verify this with a test rather than assuming Go's zero-value JSON decoding is sufficient in context (e.g. no custom `UnmarshalJSON` on `HistoryEntry` that would reject unknown/missing fields).
- **Served-model determination depends on the CLI's reporting.** If a gateway doesn't surface a distinguishable identifier, record what's available and treat absent as unknown rather than defaulting to the requested model. Research should verify that the `ModelUsage` keys carry the *resolved* id for an alias-configured run (rather than echoing the alias), since the resolved-id requirement depends on it; the alternative source noted by the reporter is the `system/init` model.
- **`ModelUsage` can contain more than one key per invocation** (`engine/claude.go:1024-1029` is `map[string]struct{...}`, keyed by model identifier — a single Claude Code session can report usage under multiple models, e.g. when a subagent runs under a different model than the top-level invocation). The issue's suggested fix treats "served model" as a single value but doesn't specify how to resolve multiple `ModelUsage` keys into one field. This is left as a technical decision for Research/Plan (e.g. record only when exactly one key is present and treat multi-key sessions as "unknown"/"mixed", or record all keys) — no user-facing requirement hinges on the choice, since `tokenUsageFromResponse` already sums token counts across all keys regardless of how many models are present.
- No dependencies; this issue is itself the prerequisite for the cost-pricing work, which @bdueck explicitly identified as "worth landing alone."

---

Reported by @bdueck; resolved-model evidence from @jmatthewpryor. Do not use closing keywords for #1385 or #1386 in the PR — the community report is closed manually once a release ships.
