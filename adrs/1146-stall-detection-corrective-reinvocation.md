# ADR 1146: Stall Detection and Corrective Re-Invocation

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1146 — Engine-side stall detection: vary retry strategy instead of burning identical
attempts on a headless backgrounding stall

## Context

#1077 fixed the prompt-guidance half of the headless-backgrounding-stall failure mode: worker skills
now prohibit backgrounding a long-running command and waiting for a completion notification in a
headless stage. That mitigates the failure but does not close it — negative instructions in a prompt
are not reliably followed. When a worker backgrounds anyway, every retry in the cooldown loop
(`docs/state-machine.md` §7.1) re-sends the identical prompt, and (per `--resume` session continuity)
resumes a conversation that already committed to the stalled approach — so a retry that follows the
exact same plan re-derives the exact same stall.

Live evidence: `#816` (auto-bump plugin versions on cut-release) produced a turn-capped first attempt
(51/50 turns, $3.14), then two declining, non-completing retries (12/50 turns, $1.56; 3/50 turns,
$0.29) — $4.99 wasted before `MaxRetries` escalated the issue to `fabrik:paused`. The issue only
recovered after a human manually applied `fabrik:extend-turns` *and* the #1077 skill update (merged
between the failures and the recovery) landed — it then completed in 41 turns, comfortably under the
*original* 50-turn cap. The extra budget alone did not fix it; the changed guidance did. That is this
issue's thesis, demonstrated rather than argued: a retry needs a different strategy, not more room to
repeat the same one.

The issue's own text identifies two possible detection signals: (a) live detection of a backgrounded
command followed by N minutes of silence within a single invocation, and (b) a turn-capped attempt
followed by retries with monotonically decreasing turn counts across separate dispatches. (a) requires
new NDJSON tool-use parsing and a second watchdog running alongside the existing hardcoded 15-minute
inactivity timeout (`claudeInactivityTimeout`, shared by every invocation reason). (b) needs only a
comparison of data the engine already computes. The issue's own text calls (b) "cheaper to detect" and
says it "alone is a strong stall indicator even without instrumenting token idling directly" — and
`#816`'s actual shape is exactly a single instance of it.

This work also touches the same retry/`--resume` path #1081 fixed (the "successor reports work it
never performed" problem, documented as-built in `docs/stage-lifecycle.md`'s "Retry after a turn-cap
kill" section — no standalone `adrs/1081-*.md` exists).

## Decision

Implement signal (b) only — the trend-based detection — and defer signal (a) as an explicit follow-up.

**Detection (`detectAndArmStallHint`, `engine/item.go`).** Runs inside the existing incomplete-outcome
branch of `finalizeStageOutcome`, after `StageRetryIncremented` and the `count >= MaxRetries` escalation
decision, whenever `claudeRan` is true **and this attempt is not the one that triggers escalation**:

1. Read the previous incomplete attempt's `TurnsUsed` and capped status for this stage from the store.
2. Compute this attempt's own capped status (`usage.MaxTurns > 0 && usage.TurnsUsed >= usage.MaxTurns`)
   — evaluated against *this attempt's own* `MaxTurns`, so a `fabrik:extend-turns`-widened budget on a
   later attempt is handled correctly without special-casing.
3. Record this attempt's `TurnsUsed`/capped status via a new `itemstate.StageTurnUsageRecorded`
   mutation, unconditionally overwriting the previous values.
4. If the previous attempt was capped, this attempt is **not** itself capped, this attempt used more
   than zero turns but strictly fewer than the previous attempt's turns, **and this attempt stopped
   cleanly** (`err == nil`) **or was itself a classified turn-cap exit** (`turnLimited`, see below), arm
   a one-shot corrective hint (`itemstate.StallHintArmed`) and post an informational comment citing both
   turn counts.

   The "not itself capped" precondition rules out a false positive from the pre-existing progress-based
   turn-extension loop (`runInvocationWithExtension`, ADR-030): that loop can widen the effective budget
   on one dispatch (e.g. to 2×/3× `stage.MaxTurns`) without that widening persisting to the next,
   separate dispatch. Without this precondition, two consecutive turn-capped attempts could show a
   smaller absolute `TurnsUsed` on the second purely because its own budget reset lower — not because it
   is a declining, stalled retry — and would be misclassified as a stall.

**Self-limiting by construction.** The attempt that satisfies step 4's condition is, by construction, not
itself capped — so step 3's unconditional overwrite clears the arming precondition (`LastTurnsCapped`)
immediately after arming. No separate one-shot guard is needed to keep this "a single corrective
re-invocation," matching the issue's proposed fix rather than an escalation ladder — which also matches
the practical constraint that default `MaxRetries=3` leaves at most two retry slots after the first
attempt, too little room for a multi-level escalation to matter.

**Injection (`consumeStallHint`, `engine/item.go`; `buildPrompt`, `engine/claude.go`).** A new
`InvokeOptions.CorrectiveHint string` field is set, when building options for a stage's next
invocation, by `consumeStallHint`: if `StallHintPending` is set for the stage, it applies
`itemstate.StallHintConsumed` and returns a fixed hint string; otherwise it returns `""`.
`buildPrompt` gained a `correctiveHint string` parameter, prepending it as a callout block when
non-empty. The hint text is a single, stage-agnostic, hedged constant — consistent across
Implement/Review/Validate rather than varying per stage, and explicitly hedged ("if that's what
happened... if something else caused it, disregard this note") because the detection is a heuristic
with an acknowledged false-positive risk.

**State.** Three new `StageState` fields (`LastTurnsUsed map[string]int`, `LastTurnsCapped
map[string]bool`, `StallHintPending map[string]bool`), populated by the three new mutations above,
mirroring the existing `Attempts`/`PRCreationFailed` shape exactly — in-memory only, no persistence.
`StageRetryCleared` (already applied on stage completion and on unpause via `clearFailedStage`) was
extended to also delete all three new per-stage entries, so a later, unrelated incomplete run of the
same stage never inherits a stale armed hint from a long-past episode.

**Retry budget.** A detected stall still applies `StageRetryIncremented` — unlike #1119's usage-limit
exemption, the stage genuinely ran and spent real tokens; the point of this fix is to spend the
existing budget more effectively on the next attempt, not to grant unlimited extra attempts for a
condition that (unlike a usage limit) the engine is actively trying to correct.

## Rationale

### Why compare only the immediately preceding attempt, not a longer trend window?

Under default `MaxRetries=3` there are only two retry slots after the first attempt. Waiting for a
longer (e.g. three-point monotonic) trend before arming would detect the pattern at the same poll that
confirms `MaxRetries` and escalates — too late for the hint to reach any subsequent invocation. Firing
on a single capped-then-declining pair is the latest timing that still lets the hint help, and matches
`#816`'s actual two-attempt shape.

### Why not also implement the live backgrounding-then-silence signal?

It requires new live NDJSON tool-use parsing in `runClaude`/`turnCountingWriter` and a second watchdog
running alongside the existing hardcoded 15-minute inactivity timeout, which is shared across every
invocation reason (wall-time enforcement, daemon shutdown, genuine stuck sessions) — entangling a
narrow, single-purpose detector with a timeout whose scope is much broader. The issue's own text says
the trend signal "alone is a strong stall indicator," and it would have caught `#816` on its own. Filed
as a possible follow-up rather than implemented speculatively.

### Why a plain boolean in `finalizeStageOutcome` rather than a sentinel error (cf. ADR-1119)?

ADR-1119's sentinel-error pattern fits a condition detectable *inside* `interpretClaudeResult` from a
single invocation's raw output. This trend can only be computed *after the fact*, by comparing the
current attempt's `usage` against the *previous* attempt's stored data — there is no single invocation
boundary at which a sentinel could be raised. It belongs in `finalizeStageOutcome`'s existing
incomplete-path bookkeeping as a plain comparison, not a sentinel threaded through `errors.As`.

### Why does this not regress #1081?

The change is purely additive: no modification to `resume`, session-file handling, or output
accumulation. The injected hint is new prompt content built fresh by `buildPrompt` (already rebuilt
on every `--resume` retry), and the new state is bookkeeping alongside — not inside — the existing
retry-count/cooldown logic. `docs/stage-lifecycle.md` now cross-references this section from the
"Retry after a turn-cap kill" discussion to keep both readable together.

### Why does `runInvocationWithExtension` check the usage-limit suspension gate before consuming the hint?

`consumeStallHint` destructively clears `StallHintPending` as soon as it runs, regardless of whether
the resulting `InvokeOptions` are ever actually used to invoke Claude. The account-wide usage-limit
suspension gate (ADR-1119/#1120) can cause a dispatch to skip invoking Claude entirely. An earlier
version of this change built `InvokeOptions` (and thus consumed the hint) before checking that gate —
a dispatch landing while suspended would silently discard an armed corrective hint without it ever
reaching a real invocation, defeating the one-shot re-invocation for that stall episode with no
observable symptom other than the fix quietly not working. The suspension check now runs first, so a
gated dispatch leaves the hint pending for whichever dispatch actually reaches Claude next.

### Why is arming skipped on the attempt that triggers escalation?

`detectAndArmStallHint`'s comment tells the operator "the next invocation will receive a corrective
hint." If the capped-then-declining pattern is detected on the very attempt whose `count` also reaches
`MaxRetries`, `escalateFailedStage` pauses the issue immediately afterward, and a human's later
`clearFailedStage()` applies `StageRetryCleared` — which wipes `StallHintPending` before any further
invocation happens. The promised hint would never be delivered, and the comment would be actively
misleading about what already happened. `finalizeStageOutcome` now decides `willEscalate` (via
`StageRetryIncremented` and the `count >= MaxRetries` check) before calling `detectAndArmStallHint`, and
skips the call entirely when `willEscalate` is true — an earlier version of this change called
`detectAndArmStallHint` unconditionally on `claudeRan`, before the escalation decision existed, which is
exactly the ordering that produced this bug.

### Why gate arming on `err == nil || turnLimited`, and why isn't `err == nil` alone sufficient?

Found via external review (relayed manually on issue #1146 since Fabrik only consumes inline PR
review-thread comments today, not review bodies — tracked as #1189). The original implementation
called `detectAndArmStallHint` for any `claudeRan` incomplete attempt, without examining `err` at all.
`claudeRan` (`engine/item.go`) is true for *any* invocation error except a process-start failure
(`*exec.Error`/`*os.PathError`) — so a transient network failure, a git-push error surfaced as `err`,
or malformed CLI-output parsing all reach the retry branch with `completed = false` and whatever
partial `usage.TurnsUsed` happened to be parsed before the error. If that partial count is smaller than
a prior capped attempt's, the heuristic matched and armed a hint — posting a specific, confident
diagnosis ("likely backgrounded a long-running command") for what was actually an unrelated crash. This
is the same failure shape as #1183 (an automated diagnosis derived from a weak signal, trusted at face
value), and this feature's whole output is advisory prose an operator is meant to trust — a wrong-but-
plausible diagnosis is worse than none.

The fix gates arming on `err == nil || turnLimited`, reusing the `turnLimited` local (`errors.As(err,
&turnLimitErr)`, already computed earlier in `finalizeStageOutcome` for the `InvocationRecorded` write).
`err == nil` alone is **not** sufficient and was considered and rejected: a turn-cap exit — the *primary*
shape this feature exists to detect — is itself reported as a non-nil `*claudeTurnLimitError` (see
`claude.go`, ADR-1178), not `err == nil`. A bare `err == nil` gate would exclude every turn-capped
attempt from ever being recorded as `prevCapped`, silently disabling the detector for the exact `#816`
shape it was built for. `turnLimited` is the same classification `finalizeStageOutcome` already uses to
distinguish a genuine fault from an incomplete-but-resumable run for the `Errored`/`TurnLimited` fields
on `InvocationRecorded` (`Errored: err != nil && !turnLimited`) — reusing it here means both places agree
on what "clean" means, rather than defining the concept twice.

In practice this only changes behavior for the *declining* attempt in a pair: the previous (capped)
attempt is unaffected, since `turnLimited` is true for it. The declining attempt itself is never
`turnLimited` in a way that matters here — `detectAndArmStallHint` already requires the current attempt
not be capped, and a turn-limit exit is definitionally capped — so in effect the added condition amounts
to requiring the declining attempt's `err` be `nil`. Guarded against regression by
`TestProcessItem_StallDetection_NoArmOnGenericErrorWithDecliningTurns`.

### Why is the hint text generic rather than backgrounding-specific with certainty?

A legitimately-progressing multi-retry stage (e.g., a Review resolving threads one at a time) can show
declining turn counts per retry as remaining work shrinks, without being stalled — the discriminator
(requiring a turn-capped predecessor) narrows this considerably but does not eliminate it. A wrongly
confident hint could push a non-stalled retry toward an irrelevant strategy change; a hedged one costs
only a disregardable suggestion in the false-positive case.

## Consequences

**Positive:**
- `#816`-shaped incidents recover automatically within the existing retry budget, without a manual
  `fabrik:extend-turns` application or a human unpause round-trip.
- No new live parsing or additional watchdog — the signal is computed entirely from data the engine
  already had in scope, at the point it was already making a retry-vs-escalate decision.
- The corrective hint is stage-agnostic, so it generalizes to any non-read-only stage without per-stage
  tuning.

**Negative / Trade-offs:**
- False positives are possible for a legitimately-shrinking, non-stalled retry; mitigated by requiring
  a turn-capped predecessor as a precondition, by requiring the declining attempt to have stopped
  cleanly (`err == nil || turnLimited` — see Rationale), and by hedging the hint text, but not
  eliminated.
- In-memory-only state means a Fabrik restart between the capped attempt and the declining retry loses
  the trend — the same accepted restart-gap tradeoff every other `StageState` field already carries
  (see ADR-030).
- The live backgrounding-then-silence signal remains unimplemented; a stall that produces a *single*
  capped attempt with no subsequent declining retry (e.g. it stalls again at the same or a higher turn
  count) is not caught by this detector alone.

## Explicitly Out of Scope (possible follow-up)

Live detection of a backgrounded command followed by N minutes of silence within a single invocation,
and any change to the shared 15-minute inactivity watchdog, are deliberately deferred. Escalating the
corrective hint beyond a single re-invocation (an escalation ladder) is also out of scope — the default
retry budget leaves no practical room for one, per the Rationale above.
