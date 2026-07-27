# ADR 1120: Account-Wide Claude Usage-Limit Backoff and Suspension

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1120 — Back off to the usage-limit reset time and suspend Claude dispatch account-wide

## Context

ADR-1119 gave Fabrik detection for a Claude account usage-limit exit (`claudeUsageLimitError{Message,
ResetTime}`), excluded it from `max_retries`, and applied a `fabrik:claude-limit` label. It deliberately
left two costs unaddressed:

1. `handleUsageLimitExit` applies `itemstate.StageAttempted`, so the affected issue retries on the
   normal dispatch cooldown (`PollSeconds * 10`, 5 minutes by default) — against a limit that resets in
   hours. Every one of those retries is guaranteed to fail.
2. The limit is account-wide, not per-issue. With `max_concurrent > 1`, every in-flight item
   independently rediscovers the same limit inside the same window and waits out its own cooldown —
   reported by @bdueck in #1084 across two issues simultaneously.

`claudeUsageLimitError.ResetTime` already carries the CLI's own reset-time fragment (e.g. `"10:20pm
(America/Edmonton)"`) — a 12-hour clock time plus an IANA zone, no date. Fabrik already solves an
analogous problem for the unrelated GitHub GraphQL/REST budget (`engine/backoff.go`,
`engine/terminal.go`, `tui.RateLimitAlertEvent`); this issue borrows that shape but not its
implementation, because the two conditions differ in what they gate: the GraphQL budget is spent by the
entire poll cycle, so its pause sits at the top of `doPollCycle`, whereas the Claude usage limit is spent
only by Claude invocations, and unrelated engine work (board polling, settle scans, label
reconciliation) must keep running while Claude dispatch is suspended.

## Decision

1. **Reset-time parsing** (`engine/usage_limit_backoff.go`): `parseUsageLimitResetTime(resetTime string,
   now time.Time) (time.Time, bool)` splits the `"<clock> (<zone>)"` fragment, resolves the zone via
   `time.LoadLocation`, parses the clock via the `"3:04pm"` reference layout, and rolls the result to the
   next day if it isn't after `now` in that zone (the fragment carries no date, so "already past" only
   ever means "meant tomorrow"). `computeUsageLimitResetDeadline` wraps this and falls back to
   `now.Add(claudeUsageLimitFallbackBackoff)` — a fixed **1 hour** — when `resetTime` is empty or doesn't
   parse, reporting which path was taken so callers can log it.

2. **Account-wide suspension state**: a single `claudeSuspendedUntil time.Time` field on `Engine`,
   guarded by its own `claudeSuspendMu` (not the general-purpose `e.mu`, to keep the critical section
   independent of unrelated engine state). A zero value unambiguously means "not suspended"; the entire
   check is `now.Before(claudeSuspendedUntil)`. `activateClaudeSuspension(issueNumber, resetTimeRaw,
   now)` computes the deadline and only updates the field if no suspension is active or the new deadline
   is later — concurrent workers reporting the same or different reset times converge on the latest one,
   never shortening an active window. `clearClaudeSuspension(reason)` clears it, called only when an
   invocation actually *succeeds* (`err == nil`) — not merely when it returns something other than a
   `claudeUsageLimitError`. A generic, unrelated error is not evidence the account-wide limit has
   cleared, and clearing on it unconditionally would race with a concurrently-running worker: worker A
   can start an invocation before any suspension is active, worker B can detect the limit and activate
   the suspension while A's invocation is still in flight, and A's invocation can then return an
   unrelated error moments later — clearing on any-non-limit-error would wipe out B's just-activated
   suspension before it ever gated anything. Both `activateClaudeSuspension` and `clearClaudeSuspension`
   log and emit `tui.ClaudeUsageLimitAlertEvent` only on an actual state change, the same non-spamming
   idiom already used for `fabrik:claude-limit`/`fabrik:awaiting-ci`.

3. **Checked lazily at invocation time, not polled by a ticker.** `claudeSuspendedUntilTime(now)` is a
   cheap read evaluated at each of the three places a Claude invocation can actually be attempted:
   `runInvocationWithExtension` (`engine/item.go`, stage runs), `processComments`/
   `runCommentExtensionLoop` (`engine/comments.go`, comment review), and `resolveConflictWithClaude`
   (`engine/merge_train.go`, inline merge-train conflict resolution). Once `now` reaches the deadline,
   the very next dispatch attempt — which was going to happen anyway on the normal poll cadence —
   proceeds normally. There is no second wake mechanism.

4. **Gate + detect inline at each of the three call sites, not a shared `ClaudeInvoker` wrapper.** A
   uniform wrapper returning one sentinel for "just detected" and "already suspended" would still leave
   each caller needing bespoke handling of *what happens when Claude doesn't run* — skip the comment
   circuit-breaker count, don't eject a merge-train member — so the wrapper would just relay those
   concerns back out anyway. Handling each site explicitly keeps the blast radius of each change small
   and independently reviewable, and reuses the existing `claudeUsageLimitError` sentinel for both the
   fresh-detection and gated-already-suspended cases, so `finalizeStageOutcome`'s existing
   `errors.As` → `handleUsageLimitExit` routing from #1119 needed no changes beyond the comment-copy fix
   below.

5. **`comments.go` and `merge_train.go` now detect the sentinel, not just gate on it.** Before this
   issue, neither path unwrapped `claudeUsageLimitError` — a hit there was indistinguishable from an
   ordinary error to its caller. `processComments` now returns immediately (before any reaction, label,
   or worktree side effect) when the gate reports suspended, and skips `checkCommentBreaker` when the
   error it got back from `runCommentExtensionLoop` is a usage-limit hit — an account-wide condition is
   not evidence this issue's comment thread made no progress.

6. **Merge-train: a usage-limit hit is a fatal `assembleTrialBranch` error, not a per-member ejection.**
   `resolveConflictWithClaude`'s signature changed from `bool` to `(bool, error)`. Previously, returning
   `false` for *any* reason — including a real API error — caused `git merge --abort` + `ejectMember`.
   That's correct for a genuinely unresolvable conflict but was a real correctness bug for an
   account-wide usage limit: ejecting a healthy member because the account ran dry. `assembleTrialBranch`
   now distinguishes the two: `(false, nil)` still ejects; `(false, non-nil)` aborts the merge and
   propagates the error, which `assembleAndValidate`'s existing fatal-error path already handles (log,
   clean up trial artifacts, return — retried on the train's next natural cycle, no member punished).

7. **TUI**: a new `ClaudeUsageLimitAlertEvent{Suspended bool, Reset time.Time}` (`tui/events.go`) drives a
   new, fully independent `ClaudeUsageLimitBannerComponent` (`tui/usage_limit_banner.go`) mirroring
   `AlertBannerComponent`'s `Update`/`Height`/`View` shape. It is a second component, not an extension of
   `AlertBannerComponent`, because the GitHub rate-limit and Claude usage-limit conditions are unrelated
   and can be active simultaneously — a single component would need internal priority/stacking logic for
   two independent triggers, whereas two components each independently contributing to the layout height
   budget (exactly like `alert` already does) is simpler and matches the existing composition pattern in
   `model.go`. The banner self-clears on a `TickEvent` once `now` passes the reset, rather than requiring
   the engine to emit an explicit "resumed" event at exactly that moment — driving the visual expiry off
   the already-existing per-second tick fan-out is simpler than adding a timer goroutine purely for TUI
   cosmetics.

8. **Comment-copy fix**: `handleUsageLimitExit`'s explanatory comment no longer says "Fabrik will keep
   retrying on the normal poll cooldown" — it now names the account-wide suspension and describes
   automatic resume at the reset time or on the next successful invocation, whichever comes first.

## Rationale

### Why a single `time.Time` deadline rather than a boolean + reset pair?

A zero value is an unambiguous "not suspended" sentinel, so the entire visibility check is one
comparison. Extending an active suspension when a second worker reports a later reset is `if
new.After(current) { current = new }` — there is no separate "active" flag that could drift out of sync
with the deadline it's supposed to describe.

### Why not gate at the `doPollCycle` layer, mirroring the GitHub rate-limit pause exactly?

The GraphQL/REST budget is spent by the poll cycle itself (fetch + dispatch + mutations), so pausing the
whole work phase is the correct unit of suspension for that condition. The Claude usage limit is spent
only by Claude invocations; gating the whole poll cycle would also stop settle scans, label
reconciliation, and board polling for no reason — explicitly out of scope per the issue. The gate
therefore has to sit at (or near) the three actual invocation call sites instead.

### Why is the fixed fallback exactly 1 hour?

An order of magnitude longer than the existing 5-minute dispatch cooldown, so a hit still meaningfully
reduces hammering even with no parseable reset time, without over-committing to a duration that would
strand the engine long after a real (shorter) limit already cleared. Weekly-limit hits — which likely
don't carry a same-day `HH:MMam/pm` reset fragment at all — simply re-arm this same 1-hour fallback
repeatedly until the account recovers. That's a guess for the weekly case (it may not resolve for days),
but it's still strictly better than the current 5-minute-cooldown hammering, and a fixed conservative
backoff (not perfect knowledge of weekly-reset timing) is what the issue asked for.

### Why does the banner self-clear on a tick instead of waiting for the engine to say so?

If literally no item is ever up for dispatch during the suspension window, the engine's own
`claudeSuspendedUntilTime` check never runs, so it never proactively emits a "resumed" event either — it
only resolves the next time something asks. Driving the banner's own expiry off the already-existing
per-second `TickEvent` (which every component already receives) means the TUI can accurately reflect
"the reset time has passed" even in that scenario, without adding a dedicated wake-up mechanism to the
engine purely to keep a status line honest. This can make the banner clear visually slightly ahead of the
engine's own internal state changing — harmless, since the engine always compares
`claudeSuspendedUntil` against live `time.Now()` rather than trusting any cached "is it active" flag.

### Why fatal-error-not-ejection for merge-train, rather than leaving the existing eject-on-false behavior?

The existing behavior conflated "Claude couldn't resolve this conflict" with "Claude couldn't be invoked
at all" — both returned `false`. An account-wide usage limit says nothing about whether the *specific*
member's conflict is resolvable; ejecting it anyway would punish an innocent member for an unrelated
outage and could dissolve a batch that would have landed cleanly once the limit cleared. Propagating a
fatal error instead reuses the trial-assembly retry path that already exists for other setup failures
(`EnsureTrainWorktreeAt`, `PushTrainBranch`), so no new recovery mechanism was needed.

## Consequences

**Positive:**
- A usage-limit window now costs one detection and one automatic resume, not `max_concurrent`
  independent discoveries each waiting out its own 5-minute cooldown.
- The account-wide suspension is checked lazily with no additional goroutine, ticker, or polling loop —
  it piggybacks entirely on invocation attempts and dispatch cadence that already exist.
- Merge-train no longer ejects healthy members during an account-wide outage.
- The GitHub rate-limit and Claude usage-limit conditions remain visibly and operationally distinct in
  logs, labels, and the TUI, per the naming caution in the issue.

**Negative / Trade-offs:**
- The suspension is per-process (`Engine`-scoped state) — a fleet of Fabrik instances sharing one Claude
  account will each independently discover and suspend on the limit. Cross-process coordination is an
  explicit non-goal of this issue.
- The 1-hour fallback is a guess for the weekly-limit case, which may run considerably longer; the engine
  will simply re-arm the fallback repeatedly rather than ever waiting the correct, longer duration.
- No ceiling was added on repeated usage-limit hits (unchanged from ADR-1119) — a persistently
  misdetected condition still retries indefinitely rather than escalating for human review, now spaced
  out by the suspension window rather than the 5-minute cooldown.
- The TUI banner's self-clear-on-tick can run slightly ahead of the engine's own suspension state when no
  item is dispatched during the window — a cosmetic-only lag, not a correctness issue (see Rationale
  above). A future contributor might be tempted to "fix" this with a ticker; it is intentional.

## Explicitly Out of Scope

A dedicated escalation/safety-net path for persistent or repeated misdetection remains deferred, per
ADR-1119's own explicit deferral — this issue adds the backoff/suspension half of that ADR's follow-up
list but not the escalation half. Cross-process coordination between multiple Fabrik instances sharing
one account is a known, accepted gap (see Consequences). `max_retries` semantics for genuine failures and
GitHub's own (unrelated) rate-limit handling are untouched.

> **Extended by [ADR-1183](1183-structural-claude-usage-limit-detection.md).** That deferred
> escalation/safety-net path is now partially delivered: `fabrik:clear-claude-limit` gives an operator a
> restart-free way to end a suspension believed to be a false positive, and `resetTimeRaw` is now always
> `""` in practice (structural detection carries no reset-time fragment to parse), so
> `activateClaudeSuspension`/`clearClaudeSuspension` and the reset-parsing machinery this ADR describes
> are otherwise unchanged and remain accurate — only the caller-supplied `resetTimeRaw` value changed,
> not this ADR's design.

**References:** ADR-1119 (`1119-claude-usage-limit-detection.md`) — the detection and retry-exemption
work this ADR builds on. `engine/backoff.go` (`shouldPauseForRESTRateLimit`, `nextRateLimitLow`),
`engine/terminal.go` (`runProbeAndDeepFetch`) — the GitHub rate-limit suspension pattern this ADR mirrors
in shape, deliberately not merges with. `docs/state-machine.md` §7.3 — the as-built description of both
the per-issue exemption (ADR-1119) and this account-wide suspension.
