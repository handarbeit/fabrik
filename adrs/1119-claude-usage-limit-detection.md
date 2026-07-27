# ADR 1119: Claude Usage-Limit Detection and Retry Exemption

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1119 — Detect Claude usage-limit exhaustion and stop misattributing it as a stage failure

## Context

When the underlying Claude account hits its usage limit, the `claude` CLI exits immediately with a
message naming the reset time (e.g. `You've hit your session limit · resets 10:20pm
(America/Edmonton)`). Before this change, Fabrik had no detection for this condition anywhere: the
invocation was treated as an ordinary stage failure. Because the process started and exited non-zero
(not a start failure), `claudeRan` computed to `true`, `StageRetryIncremented` fired, and the retry
budget (`max_retries`) was spent on a condition that was never a genuine failure. The cooldown between
retries (`PollSeconds * 10`, 5 minutes by default) is far shorter than a usage-limit reset window
(hours), so all retries burned within minutes and the issue was durably paused with a "stage failed N
times" comment — misattributing an account-wide outage as a branch-specific failure. Reported by
@bdueck in #1084 with `history.json` evidence (`1/150 turns, $0.00, failed`, twice per issue across two
issues).

Because the limit is account-wide, every concurrent worker hits it in the same poll window, so
`max_concurrent` workers each independently burn their own retry budget and pause — turning one outage
into N issues needing manual recovery.

## Decision

Detection is split across two layers, matching where the relevant data is in scope:

1. **`engine/claude.go`** — `detectUsageLimitExit(rawOutput []byte)` scans the raw NDJSON stdout bytes
   (not the parsed `claudeResponse.Result`, and not the already-collapsed `text` variable) for
   Anthropic's usage-limit exit message, via two narrow regexes (`usageLimitHitRE` for the required
   phrase, `usageLimitResetRE` for an optional reset-time fragment used only to enrich the comment).
   When Claude exits non-zero (`runErr != nil`) and the output carries no `FABRIK_STAGE_COMPLETE`
   marker, `interpretClaudeResult` returns a new `*claudeUsageLimitError{Message, ResetTime}` sentinel
   instead of the generic `"claude exited with error: %w"` wrapper.
2. **`engine/item.go`** — `finalizeStageOutcome` detects the sentinel via `errors.As` immediately after
   the existing engine-shutdown guard, and routes to a new `handleUsageLimitExit`, which:
   - Applies `itemstate.StageAttempted` (so the normal dispatch cooldown applies via `LastAttemptAt`),
     but never `StageRetryIncremented` — the stage never ran, so nothing should count against
     `max_retries`.
   - Applies neither `stage:<name>:failed` nor `fabrik:paused`.
   - Posts an explanatory comment (naming the condition and the reset time when parsed) and applies
     `fabrik:claude-limit`, gated on `!hasLabel(item.Labels, "fabrik:claude-limit")` — the label's
     presence is itself the once-per-episode idempotency guard, identical to the `fabrik:awaiting-ci`
     and `fabrik:bot-reprompted` gate-label idiom already in the codebase.
   - Returns immediately — no `commitWIP`, no push, no `markCommentsSeenByStage` (nothing was
     produced).

   Immediately after that branch, any invocation that reached this point (i.e. was *not* itself
   classified as a usage-limit exit — success, blocked-on-input, no-work-needed, genuine
   failure/retry, or PR-creation failure) unconditionally clears `fabrik:claude-limit` if present.

`fabrik:claude-limit` is also added to `transientLifecycleLabels` (`engine/poll.go`) so an issue
mid-retry-loop against the limit is never misclassified as terminal, and is swept from closed issues
like the other transient gate labels.

## Rationale

> **Superseded in part by [ADR-1183](1183-structural-claude-usage-limit-detection.md).** The
> "Why message-matching rather than a structural field" rationale immediately below states that "no
> structural field reliably discriminates a usage-limit exit... today." That premise held when this
> ADR was accepted, but became false once #1178 added `terminal_reason` capture to `claudeResponse`:
> the CLI's own result object carries a `terminal_reason` value (`"blocking_limit"`) for a genuine
> usage-limit exit, distinct from the generic `error_during_execution` subtype this ADR's Decision
> section describes. The prose-matching detection this ADR documents (`detectUsageLimitExit`,
> `usageLimitHitRE`, `usageLimitResetRE`) was deleted and replaced with structural detection
> (`classifyUsageLimitExit`) after it produced an ~11-hour account-wide false-positive suspension by
> matching a stage's own output prose (#1183). The rest of this ADR — the `StageAttempted`-without-
> `StageRetryIncremented` split, the once-per-episode label idiom, the auto-clear-on-any-non-limit-
> invocation rule — remains accurate and unchanged.

### Why detect on raw bytes rather than the parsed `Result` field?

It is unconfirmed whether a real usage-limit exit populates `claudeResponse.Result`, only
`claudeResponse.Errors[]` (the existing stale-session fixture's shape — a shape that today never
reaches the collapsed `text` variable at all), or non-JSON plain stdout. Scanning the raw bytes
directly is correct under all three candidate shapes without needing to know in advance which one a
real occurrence takes — the single highest-severity risk identified during Research.

### Why message-matching rather than a structural field (`IsError`/`Subtype`)?

No structural field reliably discriminates a usage-limit exit from any other `is_error` response
today — the same `error_during_execution` subtype is shared with the pre-existing stale-session
condition. Message-matching is therefore the primary signal, per the issue's explicit fallback
instruction. The turn-count/cost shape (`~1 turn, $0.00`) the original report highlighted is logged
alongside purely for diagnostic corroboration and never gates the boolean — a genuine immediate stage
failure (bad prompt, permission error) can share that same shape, and gating on it would misclassify
real failures as quota exhaustion.

### Why a new sentinel error type rather than a plain string check?

`errors.As(err, &claudeUsageLimitError{})` lets a test's mocked `ClaudeInvoker.Invoke` construct and
return the condition directly, satisfying the issue's testing requirement (mock at the `ClaudeInvoker`
interface boundary, never shell out to a real `claude`) without needing raw-JSON fixtures at the
integration-test level. The raw-JSON detection path is exercised separately, at the
`detectUsageLimitExit`/`interpretClaudeResult` unit level.

### The `StageAttempted`-without-`StageRetryIncremented` split is not a new invention

`handleBoundaryViolation` (added for the worktree-boundary-violation condition) already established
exactly this split: record the attempt so the dispatch cooldown applies, but never increment the
retry counter, because `internal/itemstate` tracks `LastAttemptAt` and `Attempts` as fully independent
fields (`StageAttempted` only ever writes the former; `StageRetryIncremented`/`StageRetryCleared` own
the latter). This ADR names the pattern explicitly so a third use site does not have to re-derive it
from inline comments: **a condition where Claude did not produce a usable outcome, but retrying
immediately would be harmful, gets `StageAttempted` (cooldown) without `StageRetryIncremented`
(budget).** `handleBoundaryViolation` additionally applies `stage:<name>:failed` + `fabrik:paused`
because a boundary violation is a permanent, human-review-requiring condition; `handleUsageLimitExit`
deliberately does neither, because a usage limit is transient and self-resolving.

### Why does the label auto-clear on *any* non-limit invocation, not only a successful one?

The acceptance criterion is "cleared on the next successful invocation," but clearing only on success
would leave the label (and its "we are currently limited" implication) set through an intervening
genuine failure. The broader rule — clear on any invocation not itself classified as a limit hit —
correctly signals "not currently rate-limited," and a subsequent genuine failure is still tracked
fully independently via the untouched `stage:<name>:failed`/`MaxRetries` machinery. This was the
explicit tradeoff recommended during Research and adopted as-is.

### Why no new cooldown constant?

Backing off until the stated reset time is explicitly out of scope (tracked as a follow-up issue this
one blocks). Reusing the existing `PollSeconds*10` dispatch cooldown via `StageAttempted` costs nothing
new and is exactly what `handleBoundaryViolation` already does for its own unrelated condition.

## Consequences

**Positive:**
- A usage-limit exit no longer consumes `max_retries` budget; a genuinely transient failure that
  happens to follow one still has the full budget available.
- Concurrent issues hitting the same account-wide limit each get a clear, distinctly labeled signal
  instead of being independently misdiagnosed as three-strikes stage failures.
- The `StageAttempted`-without-`StageRetryIncremented` pattern now has one canonical reference instead
  of being re-derived from comments at each of its two (now three, counting this ADR itself as
  documentation) use sites.

**Negative / Trade-offs:**
- With no ceiling on retries for this condition (by design — see Scope below), a misdetection or an
  unusually long-lived condition (e.g. multi-day) retries indefinitely rather than ever escalating for
  human review. This is an accepted, explicit limitation until the backoff-to-reset follow-up lands,
  which is the natural place to add a hard ceiling.
- The reset-time extraction is best-effort prose parsing over Claude's own human-readable string, not
  a machine timestamp; a format shift degrades gracefully (the comment omits the reset time) rather
  than blocking detection, but does not fail loudly either.
- During a multi-hour outage, each affected issue still produces one wasted Claude CLI invocation
  roughly every `PollSeconds*10` (5 minutes by default) until the backoff-to-reset follow-up lands.

## Explicitly Out of Scope (follow-up issue)

Backing off until the stated reset time, suspending dispatch account-wide when one worker sees the
limit, and a dedicated escalation/safety-net path for persistent misdetection are all deliberately
deferred to the follow-up issue this one blocks. `max_retries` semantics for genuine failures and
GitHub's own (unrelated) rate-limit handling are untouched.

**References:** `handleBoundaryViolation` (`engine/item.go`) is the pre-existing sibling use of the
`StageAttempted`-without-`StageRetryIncremented` pattern this ADR names; no standalone ADR previously
documented that condition.
