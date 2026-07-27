# ADR 1183: Structural Claude Usage-Limit Detection and Restart-Free Recovery

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1183 — Usage-limit detection false-positives on stage output prose, suspending Claude
dispatch account-wide

## Context

ADR-1119 detected a genuine Claude account usage-limit exit by matching a fixed phrase
(`usageLimitHitRE`, `(?i)hit your \S+ limit`) against the **raw invocation output bytes** — not the
parsed `claudeResponse.Result` field alone, since it was unconfirmed at the time whether a real
occurrence landed there, in `claudeResponse.Errors[]`, or in non-JSON plain stdout. ADR-1119's own
Rationale explicitly noted this: "It is unconfirmed whether a real usage-limit exit populates
`claudeResponse.Result`... Scanning the raw bytes directly is correct under all three candidate shapes."
That scan necessarily also matched text the assistant itself had written, because raw invocation output
is not exclusively the CLI's own error text — it includes everything the model produced during the run.

This fired for real on 2026-07-27, the detection's first day live. Issue #1178's Implement stage was
writing code and tests *about* usage-limit detection, and quoted #1084's example message
(`You've hit your session limit · resets 10:20pm (America/Edmonton)`) as fixture text four times, plus
`America/Edmonton` a fifth time. The invocation's own terminal state was an ordinary turn cap
(`"subtype":"error_max_turns"`, `"terminal_reason":"max_turns"`, 51 turns, $2.28 spent) — the opposite
of a genuine usage-limit exit, which fails immediately at ~0 turns and $0.00. `usageLimitHitRE` matched
the fixture text inside the stage's own output anyway, `usageLimitResetRE` parsed a reset time out of
the same fixture text, and Fabrik suspended Claude dispatch **account-wide** — every repo, every issue
— for the parsed duration (~11 hours), based on a timezone the operator does not live in. PR #1184
landed same-day as a targeted stop-gap: an exclusion gate refusing to classify an invocation as a
usage-limit exit when `TurnsUsed > 0 && CostUSD > 0` (a genuine exit cannot have both). That gate is
real defense-in-depth and is kept, but it does not address the root cause: detection was reading
assistant-authored prose as if it were CLI-authored signal.

This is the same class of failure #1088 already named for `isBotServiceNotice`: "patterns would collide
with this repo's own test fixtures." Fabrik's own repository is unusually exposed to it — its issues,
specs, ADRs, tests, and skill files are full of the exact strings its own detectors look for, because
Fabrik documents and tests itself using its own domain vocabulary.

Separately, issue #1178 (landed the same day, closed before this ADR) added `Subtype` and
`TerminalReason` capture to `claudeResponse` to support structural turn-cap classification
(`resp.Subtype == "error_max_turns"`). That capture is reused here.

## Decision

### 1. Detect exclusively from the CLI's structured result object

`classifyUsageLimitExit(resp claudeResponse, usage TokenUsage) (msg string, detected bool)`
(`engine/claude.go`) replaces `detectUsageLimitExit`. It checks a single field:

```go
resp.TerminalReason == "blocking_limit"
```

`"blocking_limit"` (the `usageLimitTerminalReason` constant) is sourced from the officially published
`@anthropic-ai/claude-agent-sdk` TypeScript type definitions and changelog (checked at package version
0.3.220): `TerminalReason` is documented as `'blocking_limit' | 'rapid_refill_breaker' |
'prompt_too_long' | ... | 'max_turns' | ...`, and the SDK's own changelog entry introducing the field
(v0.2.91) names `blocking_limit` in the same sentence as `max_turns` — the value #1178 already proved
this exact installed CLI version (`2.1.220`) emits on this exact result-object shape. This is the CLI's
own structural classification of *why the query loop terminated*, not text the model wrote.

`classifyUsageLimitExit` is called from `interpretClaudeResult` only when a result object actually
parsed (`ok == true`), in the same position the structural turn-cap check already occupies: after the
stale-session check, after the turn-cap check, before the generic-error fallback. An invocation whose
JSON did not parse (`ok == false` — killed mid-stream, truncated output) has no structured payload to
trust and is **never** classified as a usage-limit exit by any means; it falls through unchanged to
existing generic-failure/timeout handling.

The `TurnsUsed > 0 && CostUSD > 0` exclusion gate from #1184 is kept and reused here as a
belt-and-suspenders check, even though it is not load-bearing the same way it was for prose (a dedicated
`terminal_reason` value cannot collide with fixture text living in a different field, `result`) — it is
cheap, already tested, and adds a second independent signal against an unconfirmed field value behaving
unexpectedly.

`usageLimitHitRE`, `usageLimitResetRE`, and `detectUsageLimitExit` are **deleted**, not demoted to a
diagnostic-only signal. The issue's Requirement 2 — "Never classify from the assistant's own output
text... must come from the CLI's structured result object" — reads as absolute, not "prefer structural,
fall back to prose." Keeping the regex as an unused fallback would leave the exact mechanism that caused
the incident sitting in the codebase, one accidental re-wire away from firing again.

A diagnostic-only `claudeLog` line records any other non-empty `resp.TerminalReason` seen on an error
exit that doesn't match `"blocking_limit"` — most notably `"rapid_refill_breaker"` (see below) — so a
real-world sighting is auditable without affecting classification.

### 2. `rapid_refill_breaker` is deliberately excluded

The SDK's `TerminalReason` enum names a second limit-adjacent value, `rapid_refill_breaker`, with no
further documentation distinguishing it from `blocking_limit`. It plausibly names a short-lived burst
throttle rather than the multi-hour account exhaustion this ADR is about. Treating it identically to
`blocking_limit` risks manufacturing a second false-positive class of exactly the kind this issue exists
to fix — an unconfirmed, possibly-transient condition triggering the same heavyweight account-wide,
hour-long suspension. It falls through to the generic-error path (existing retry/cooldown machinery),
a safe default either way, and is captured by the diagnostic log above if it is ever observed in
practice.

### 3. Reset time is always the fallback backoff, never parsed from prose

`claudeUsageLimitError.ResetTime` is always `""` under structural detection — `classifyUsageLimitExit`
has no reset-time fragment to extract, because there is none in `resp.TerminalReason`, a bare string.
`computeUsageLimitResetDeadline` (`engine/usage_limit_backoff.go`, unchanged by this ADR) therefore
always takes its existing fallback path: a fixed `claudeUsageLimitFallbackBackoff` (1 hour). This
directly satisfies the issue's requirement — "If the reset time is not present in the structured
payload, fall back to a bounded conservative backoff rather than honouring an arbitrary timezone string
found in output" — with no new parsing surface. `parseUsageLimitResetTime`/
`computeUsageLimitResetDeadline` remain in place, correct and tested, simply unexercised by real
detection now.

A higher-fidelity structural reset time is available in principle: the CLI's NDJSON stream carries a
separate `"type":"rate_limit_event"` message with `rate_limit_info.resetsAt` as a numeric epoch — no
timezone-string parsing, and it cannot be confused with assistant-authored fixture text because it lives
in a distinct message type, never inside `result` text. Scanning for it is deliberately **out of
scope** here: it is additional new parsing surface the issue's Definition of Done does not require, and
the 1-hour fallback is a safe, conservative behavior in the meantime. Left as natural, well-scoped
follow-up work.

### 4. Restart-free clear: a command label, not a TUI keybinding or a re-probe

Three alternatives were considered for making an account-wide suspension recoverable without an engine
restart:

- **A new TUI keybinding** (mirroring the existing `ctrl+s`/`StopRequest` confirm-then-send-on-channel
  pattern), paired with the existing `ClaudeUsageLimitBannerComponent` display.
- **An automatic periodic re-probe**: let exactly one invocation through the suspension gate every N
  minutes, clearing immediately via the existing early-clear-on-success path in `clearClaudeSuspension`.
- **An operator-applied label**, read by a per-poll settle scan.

The label was chosen. Fabrik already has an established idiom for exactly this shape — "a label is a
command" — via `fabrik:revalidate`: an operator applies a label, a per-poll settle scan reads it, acts,
and removes it. A new `fabrik:clear-claude-limit` label, applicable to **any open board item** (not
necessarily one carrying `fabrik:claude-limit` — the suspension it clears is account-wide, not
per-issue), is read every poll by `settleClaudeLimitClearRequests` (`engine/claude_limit_settle.go`):
if any open item carries it, the scan calls `clearClaudeSuspension` once and removes the label from
every carrying item. This requires no new IPC, channel, or TUI surface, and reuses
`clearClaudeSuspension`'s existing lock discipline (`claudeSuspendMu`) and TUI event emission
(`tui.ClaudeUsageLimitAlertEvent{Suspended: false}`) unchanged.

The re-probe alternative was rejected for this scope: none of the three existing suspension gate call
sites (`item.go`, `comments.go`, `merge_train.go`) are issue-independent — each is scoped to real
dispatch work — so a periodic probe would need a new kind of lightweight, issue-independent Claude
invocation this codebase does not have today. That is real, but separately-scoped, follow-up work. The
TUI-keybinding alternative was rejected because it requires an operator to be actively attached to the
TUI at the moment they want to clear the suspension; the label works identically whether the operator is
watching the TUI, using the CLI, or interacting purely through GitHub.

### 5. Account-wide label sweep for `fabrik:claude-limit`

Before this ADR, `fabrik:claude-limit` cleared only per-issue, on that issue's own next invocation that
is not itself classified as a usage-limit exit (`engine/item.go`, unchanged by this ADR). An issue that
is labelled and then paused, blocked, or simply far down the dispatch queue keeps the label indefinitely
after the account-wide suspension has already ended — the board reads as though a limit is still in
force, even though dispatch was never actually gated by the label (it's gated by `claudeSuspendedUntil`
directly). Three issues needed manual clearing after the 2026-07-27 incident for exactly this reason.
This is the same durable-state leak pattern as #1135's orphaned `stage:*:in_progress` labels.

`settleClaudeLimitLabelSweep` (`engine/claude_limit_settle.go`), a second per-poll settle scan in the
same file, closes the gap: once `claudeSuspendedUntilTime` reports no active suspension, it
best-effort-removes `fabrik:claude-limit` from every open item that still carries it, regardless of
whether that item is ever dispatched again. It is scoped to open items only —
`cleanupClosedIssueTransientLabels` already sweeps `fabrik:claude-limit` (and now
`fabrik:clear-claude-limit`, added to `transientLifecycleLabels`) from closed issues every poll.

Both new scans are wired into `poll.go` immediately after the existing `settleNonDefaultBaseCloses`
call, clear-requests scan first, so an operator's clear and the resulting account-wide sweep can land in
the same poll cycle.

### 6. No retry/escalate machinery for either new settle scan

Unlike `settleMergeTrainMemberCloses`/`settleNonDefaultBaseCloses`, which guard an outstanding mutation
with real consequences (an issue that should be closed staying open) and therefore use the
`recordSettleRetry`/`escalateSettle` counter-and-pause machinery, both new scans use simple best-effort
removal with a warning log on failure. `fabrik:clear-claude-limit` is a one-shot command — a failed
removal just leaves it for the next poll to consume again, re-running the (idempotent) clear.
`fabrik:claude-limit`'s account-wide sweep is purely cosmetic — dispatch is gated by
`claudeSuspendedUntil`, never by the label — so a failed `RemoveLabelFromIssue` call self-heals on the
next poll with no operator-visible consequence in the meantime. Adding the full retry/escalate machinery
to either would be disproportionate to what it protects.

## Consequences

**Positive:**
- A stage whose output merely mentions usage-limit phrasing can no longer trigger an account-wide
  suspension — the incident class this issue exists to fix cannot recur through this mechanism, because
  detection no longer reads any text the assistant produced.
- A genuine usage-limit exit is detected from a single, unambiguous CLI-reported field, with an audit
  trail (`claudeLog`) naming the exact field and value that triggered classification.
- An account-wide false positive (from this or any other cause) is recoverable in one poll cycle via a
  label, with no engine restart and no loss of the board cache, observers, or `mayNeedWork` state a
  SIGHUP restart would drop.
- `fabrik:claude-limit` no longer lingers on paused/blocked/queued issues after the suspension it
  described has already ended.

**Negative / Trade-offs:**
- **Unverified exact value.** `"blocking_limit"` is sourced from the Agent SDK's published TypeScript
  schema and changelog, not from a Fabrik-captured real occurrence — this repository's history contains
  no confirmed genuine usage-limit exit to test against. If the real value differs, or the field is
  absent on this specific exit path despite being present for `max_turns`, the failure mode is a
  **silent false negative** (a genuine usage-limit exit is treated as an ordinary failure and consumes
  retry budget) — not a recurrence of the severe account-wide false-positive this ADR exists to prevent.
  The diagnostic log for unmatched non-empty `terminal_reason` values is the intended detection/
  correction path if this surfaces.
- **Narrower unparseable-JSON regression window.** A genuine usage-limit exit that happens to produce
  unparseable JSON (process killed mid-stream, truncated output) is no longer classified as a
  usage-limit exit by any means — it falls through to ordinary failure/timeout handling and consumes
  retry budget. This window already existed today only when `runErr != nil`; narrowing it further to
  also exclude the `ok == false` case is an accepted, deliberate trade given the severity of the
  false-positive being fixed.
- **Sweep/activate race.** `settleClaudeLimitLabelSweep` and a concurrently-activating real hit both
  read/act on `claudeSuspendedUntilTime`/the label around the same moment; a narrow window could see the
  sweep remove a label an instant before a fresh hit re-adds it, or vice versa. Both paths are
  idempotent and self-heal on the next poll either way — covered by a `go test -race` concurrency test,
  no functional impact expected.
- **Operator-label scope.** `fabrik:clear-claude-limit` can be applied to any open item, not only one
  already carrying `fabrik:claude-limit` — intentional, since the suspension is account-wide, but a
  point operators need to understand (documented in `docs/USER_GUIDE.md`).

## Explicitly Out of Scope

Scanning the CLI's structural `rate_limit_event`/`resetsAt` NDJSON message for a higher-fidelity reset
time (see Decision §3) — deferred, well-scoped future work. Cross-process coordination between multiple
Fabrik instances sharing one account remains out of scope, unchanged from ADR-1120. An automatic
re-probe mechanism (see Decision §4) was considered and rejected for this issue's scope, not merely
deferred — it would require a new class of issue-independent Claude invocation this codebase does not
have.

Elevating the diagnostic-only "unmatched `terminal_reason`" log line (Decision §1) to an active TUI
alert or metrics counter, raised as PR review feedback on the "Unverified exact value" trade-off above —
deferred, not implemented. The log line fires for *any* non-empty `terminal_reason` other than
`"blocking_limit"` reached on an error exit (i.e. every generic error exit other than a turn cap):
`model_error`, `api_error`, `aborted_tools`, `rapid_refill_breaker`, and the rest of the SDK's
`TerminalReason` enum all pass through it. A blanket alert on that signal would fire on most ordinary
failures, which have nothing to do with a usage limit — noise, not diagnosis. A narrower version scoped
to a specific value already named as limit-adjacent (e.g. `rapid_refill_breaker` alone) is plausible
future work, but is a separate design decision from this ADR's scope and was not made here.

**References:** ADR-1119 (`1119-claude-usage-limit-detection.md`) — the original prose-matching
detection this ADR replaces; see its superseded-in-part note. ADR-1120
(`1120-claude-usage-limit-backoff-and-suspension.md`) — the account-wide suspension/backoff machinery
this ADR builds on unchanged; see its extended-by note. #1088 — the analogous prose-vs-fixture collision
previously flagged for `isBotServiceNotice`, the precedent this ADR's root cause matches. #1135 — the
orphaned `stage:*:in_progress` label leak, the precedent for the account-wide label sweep in Decision
§5. #1178 — added the `Subtype`/`TerminalReason` capture this ADR's structural detection depends on.
#1184 — the same-day stop-gap exclusion gate this ADR keeps and reuses. `docs/state-machine.md` §7.3 —
the as-built description of both detection and suspension after this ADR.
