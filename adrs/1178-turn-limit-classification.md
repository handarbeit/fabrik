# ADR 1178: Turn-Limit Classification via CLI Subtype, Not Exit Code

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1178 — TUI renders turn-budget exhaustion as "(error)" when it is an incomplete, resumable run

## Context

A stage invocation that exhausts its `max_turns` budget causes the `claude` CLI to self-terminate
with a non-zero exit. Before this change, `finalizeStageOutcome`/`processComments` collapsed every
non-nil invocation error — a genuine fault (bad prompt, dead session, permission error) and a
turn-cap exit alike — into `InvocationRecorded{Errored: true}`. That fed `ItemState.LastInvocationErrored`,
then `tui.JobCompletedEvent{Success: false}`, and `tui/history.go`'s `!he.Success` branch rendered it
as `✗ (error)` — indistinguishable from a real failure. A turn-cap exit is not a fault: it is an
incomplete run that made real progress and resumes in the same Claude session on the next attempt
(session-ID capture already survives a max-turns kill specifically to enable this — see #1081).
Reading several capped runs as red errors drives the wrong diagnosis (investigate a fault) instead of
the right one (raise the budget). #1114 was the concrete trigger: five capped Implement runs all
rendered as errors, totalling $21.77 in the History pane.

The CLI's own result JSON already discriminates the two cases structurally, via `subtype`:

```json
{"subtype": "error_max_turns", "terminal_reason": "max_turns", "num_turns": 51, "is_error": true}
```

versus a genuine fault (the #1128 stale-session case):

```json
{"subtype": "error_during_execution", "num_turns": 0, "is_error": true}
```

`claudeResponse.Subtype` was already captured on the struct (added for #1128's stale-session check),
but discarded for every other classification purpose — the generic non-zero-exit path never
consulted it. A turn-count heuristic (`MaxTurns > 0 && TurnsUsed >= MaxTurns && !Completed`) was
considered and rejected: the CLI's own turn accounting can report a count past the configured cap
(observed `num_turns: 51` against `max_turns: 50`), so a `>=` comparison rides on slightly odd
arithmetic and can't cleanly distinguish "hit the cap" from "legitimately used the last turn and
finished."

## Decision

Detection and threading mirror ADR-1119's `claudeUsageLimitError` pattern structurally, with one
deliberate control-flow divergence:

1. **`engine/claude.go`** — `claudeResponse` gains `TerminalReason string`. A new
   `claudeTurnLimitError{TerminalReason, NumTurns}` sentinel is returned from `interpretClaudeResult`
   when `runErr != nil`, the result JSON parsed (`ok`), and `resp.Subtype == "error_max_turns"`. This
   check sits immediately before the existing `detectUsageLimitExit` check — after the
   engine-shutdown guard and the `stageCompleteRE` completed-despite-error carve-out, so neither
   existing carve-out is disturbed.
2. **`engine/item.go` (`finalizeStageOutcome`) and `engine/comments.go` (comment-processing path)** —
   both detect the sentinel via `errors.As(err, &turnLimitErr)`, **without an early return**. The
   local `err` is left untouched for every other purpose (retry/escalation control flow,
   `StageRetryIncremented`, `MaxRetries`, `commitWIP`, branch push) — only the `InvocationRecorded`
   write changes: `Errored: err != nil && !turnLimited`, `TurnLimited: turnLimited`.
3. **`internal/itemstate`** — `InvocationRecorded.TurnLimited` / `ItemState.LastInvocationTurnLimited`
   thread the classification through the store, applied in `store.go` identically to the other
   `InvocationRecorded` fields.
4. **`engine/observers.go` → `tui`** — `InvocationObserver.OnChange` passes `TurnLimited` into
   `tui.JobCompletedEvent`; `HistoryEntry` and `DetailItem` carry it through to render. Because
   `Errored` (hence `Success`) is now `false` for a turn-cap exit, the existing `!he.Completed`
   branch is reached instead of `!he.Success` — no branch reordering was needed, only a sub-branch
   within `!he.Completed` choosing `(turn limit)` (when `TurnLimited`) vs. `(retry)` (otherwise),
   reusing the same `↻` icon. `tui/detail.go`'s `statusStr` gets the analogous
   `"incomplete (turn limit)"` vs. `"incomplete"` distinction, since it renders the same
   `HistoryEntry` data one keystroke away and would otherwise still show "error" for a capped run.

## Rationale

### Why a sentinel error, not a widened `ClaudeInvoker` return signature?

Both call sites already propagate the raw `error` from `interpretClaudeResult` unchanged; a sentinel
costs zero interface or mock changes across the test suite, and `errors.As` at the call site is the
same idiom `claudeUsageLimitError` already established.

### Why does `claudeTurnLimitError` *not* early-return, unlike `claudeUsageLimitError`?

`claudeUsageLimitError` represents a stage that never ran — the account was locked out before Claude
did any work, so skipping `StageAttempted`'s cooldown-only path is correct and nothing else should
happen. A turn-cap exit is the opposite: real work happened, the process actually ran to its budget
limit, and the *existing* retry/escalation pipeline (outer cooldown-retry via `resume=true`,
consuming `MaxRetries` budget across separate `processItem` dispatches) is how a genuine turn-cap
already recovers today — confirmed pre-existing and deliberate via #1081's closing comment ("the
retry is the same Claude session... someone deliberately made session-ID capture survive a turn-cap
kill specifically so the retry could resume") and #448's reflog evidence. This ADR does not touch
that pipeline. `claudeTurnLimitError` therefore only ever changes what feeds the `InvocationRecorded`
write; every other consequence of `err != nil` is left exactly as it was before this change.

### Why `Subtype` alone gates the branch, with `TerminalReason` captured but not required

`Subtype` already has a proven parse path (used for #1128's `error_during_execution` check);
`terminal_reason` is captured at negligible cost per the issue's own note that it's wanted for future
consumers, but adding a second condition to this one decision point buys nothing for #1178's scope.

### `Success`/`Errored` vs. `Completed`: now two independent axes

Before this change, a turn-cap exit made `Success == false` imply "something went wrong." After this
change, `Success`/`Errored` mean "genuine fault" and `Completed` independently means "no
`FABRIK_STAGE_COMPLETE` marker was seen." A turn-cap exit is `Errored: false, Completed: false,
TurnLimited: true` — incomplete but not a fault. A future reader of `finalizeStageOutcome` should not
assume these two fields still move together the way they did before #1178.

## Consequences

**Positive:**
- The TUI History pane and detail panel render a turn-cap exit as `↻ (turn limit)` /
  `incomplete (turn limit)` instead of `✗ (error)`, matching what actually happened.
- The distinction is available to any future consumer of `ItemState`/`history.json`, not just
  re-derived locally in the TUI (the issue's explicit requirement).
- No change to retry/escalation semantics for a turn-cap exit — `MaxRetries`, cooldown, and the
  outer resume path behave exactly as before.

**Negative / Trade-offs:**
- The in-process progress-based turn-extension loop (`item.go`'s `hitLimit` check, ADR-030) still
  requires `err == nil` to engage, and a genuine CLI-classified `error_max_turns` exit always has
  `err != nil` — so that faster in-process extension mechanism structurally never fires for the
  exact condition this ADR makes visible. Recovery still happens only via the slower outer
  cooldown/retry path. This is a pre-existing gap, not introduced or worsened by this change, and is
  explicitly left unaddressed here — see Explicitly Out of Scope.
- `history.json` entries written before this change default `TurnLimited` to `false` on load (zero
  value) — old capped runs remain displayed as errors retroactively; no migration was attempted since
  they were, in fact, indistinguishable at write time.

## Explicitly Out of Scope (deferred)

- Revisiting the `err == nil` gate on the in-process turn-extension loop so a genuine turn-cap could
  extend in-process instead of via the outer retry path — flagged during Research/Plan as a natural
  follow-up, but a distinct behavioral change from this issue's rendering-only scope.
- Generalizing `subtype`/`terminal_reason` capture into a first-class, general-purpose
  invocation-failure-classification mechanism for other consumers (e.g. replacing #1122's `errors[]`
  string-matching, or formalizing #1128's `error_during_execution` detection as a named condition
  rather than an inline check). `claudeTurnLimitError` is now a second bespoke sentinel following the
  same shape as `claudeUsageLimitError`; a third such condition would be a good trigger to extract the
  shared pattern, but this ADR does not do so.

**References:** ADR-1119 (`claudeUsageLimitError`) is the direct structural precedent for the
sentinel-error idiom this ADR reuses; ADR-030 documents the progress-based turn-extension loop whose
`err == nil` gate this ADR's Negative section flags but does not change.
