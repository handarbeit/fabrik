# ADR 071: Turn-Cap Retry Provenance

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1081 — A turn-cap kill produces an untrustworthy retry: the successor reports work it never performed

## Context

When a stage hits `max_turns`, the Claude CLI's own `--max-turns` self-termination exits non-zero (`completed=false`, `err != nil`). This falls through the progress-based extension loop (ADR-030) — which requires `err == nil` — straight to the cooldown/retry path: a later poll re-invokes the same stage with `resume=true`, against a worktree and Claude session that already carry the killed run's partial work.

The retry mechanism itself is correct and often cheap. The problem is trust: the successor's posted comment is indistinguishable from a normal, from-scratch completion. In the reported incident, a Review stage was killed at 51/50 turns and its retry "completed" in 2 turns with a full gate table (`npm run check`, `eslint`, `prettier`, `npm run test:unit`) — four commands, each costing a turn, reported from a two-turn run. A second, independent installation corroborated the same pattern concentrated in Implement (6 of 19 runs capped, all `Completed: false`), including cases where the successor's own summary named the situation outright ("Found the implementation already complete from a prior interrupted session") while still reporting verification as first-hand. Nothing in the posted output, `history.json`, or the board state signals that a predecessor died mid-stage.

## Decision

**1. Stage-scoped capped-run signal, not item-scoped.** `StageState.LastRunCapped map[string]bool` (keyed by stage name) records whether the most recently finalized invocation of that stage was turn-capped. It is written via a new `StageCappedRunRecorded` mutation, applied unconditionally in `finalizeStageOutcome` on every finalized invocation — so a normal completion clears a stale flag, and a chain of consecutive capped runs (the reported `51/51/23/18/18` case) keeps annotating each successor, not just the first.

**2. Detection condition drops `err == nil`.** The flag is set from `usage.MaxTurns > 0 && usage.TurnsUsed >= usage.MaxTurns && !completed` — independent of `err`. Reusing the extension loop's `hitLimit` condition (which requires `err == nil`) would silently never fire for the real-world turn-cap kill, since that process exits non-zero.

**3. Comment-only annotation; nothing injected into the resumed session's prompt.** `processItem` reads `snap.StageCapped(stage.Name)` at dispatch time — before the current invocation runs — and when true, `finalizeStageOutcome` prepends a one-line callout to the posted output (shared by both the issue-comment and PR-comment paths):

> ⚠️ **Provenance notice:** the previous attempt at this stage was stopped after hitting its turn limit. This run resumed that session — treat any claims about freshly-run commands or verification accordingly.

The annotation carries no turn numbers and is skipped when `postOutput` is empty (degenerate output is never annotated).

**4. `history.json` field, not just a log line.** `InvocationRecorded.AfterCappedRun` mirrors the same pre-dispatch flag into `ItemState.LastInvocationAfterCappedRun`, forwarded by `InvocationObserver` into `tui.JobCompletedEvent`/`tui.HistoryEntry` — rendered as a dim `⚠cap` marker in the History pane and available to `fabrik watch` for free.

**5. Two log-only "suspicious shape" heuristics, exploratory and non-gating:** a successor that resumed after a capped predecessor, completed, and used under a quarter of the stage's turn budget logs a warning; and stage output naming the inherited-work situation via a small phrase list ("prior session", "interrupted session", "prior attempt", etc.) logs a separate warning. Neither writes new persisted state — both are precedented by #448's "always-on diagnostic logging, not behind a debug flag."

**6. Scoped to full-stage invocations only.** The structurally identical comment-processing retry path (`engine/comments.go`, `comment_max_turns`) shares the same mechanism but is out of scope here — comment review already has a human reading the thread who is generally aware a reply is a continuation. Extending parity there is a separate, deliberately unbundled follow-up.

## Rationale

### Why stage-scoped storage instead of reusing the existing item-scoped `ItemState.LastInvocationCompleted`/`LastTokenUsage`?

Those fields are overwritten by *any* invocation for the item, including a comment-processing run that lands between a capped run and its retry, or a different stage's own invocation. A naive read of "the last recorded invocation" before dispatching stage X could pick up an unrelated invocation rather than stage X's own capped predecessor — reproducing a milder version of the exact "untrustworthy, unverifiable output" problem this ADR closes. `StageState` already carries several per-stage maps (`Attempts`, `LastAttemptAt`, `PausedByEngine`) that follow this same stage-keyed pattern.

### Why not gate the detection on `err == nil` like the extension loop does?

Because the real-world turn-cap kill exits non-zero — the extension loop's `hitLimit` condition was designed for a different purpose (deciding whether to `--resume` within the same `processItem` call) and happens to require `err == nil` for that purpose. Copying it verbatim here would make the annotation never fire for the exact scenario the issue reports.

### Why annotate the comment instead of injecting a heads-up into the resumed session's own context?

The issue frames this explicitly as a reader-facing provenance marker, "without judging the retry's content," and lists refusing the retry or auto-raising `max_turns` as deliberately out of scope. Injecting a notice into the resumed session's prompt would be a behavior change to the retry itself — the model might over-hedge or refuse to verify at all — and isn't needed to solve the stated problem: a human reading the comment needs to know the provenance is uncertain; the model's own behavior on the retry is unconstrained by this fix.

### Why no exact turn numbers in the annotation text?

Rendering the predecessor's own turn count (e.g. "51/50") would require a second stage-scoped field capturing the predecessor's usage, reintroducing the same mis-scoping surface this ADR is designed to avoid, for no functional gain — the annotation's job is to flag discontinuity, not restate stats already visible in the predecessor's own stats footer.

### Why leave the comment-processing retry path out of scope?

The issue's examples (Review, Implement) are both full-stage runs. Comment review is a separate, structurally parallel retry path (`comments.go`, governed by `comment_max_turns`) with a human actively reading the thread in real time — the "silent, indistinguishable-from-normal" trust gap this ADR closes is weaker there. Bundling parity in would widen this change's surface (item-state model, comments.go, its own tests) without being required by the reported incidents.

## Consequences

**Positive:**
- The successor's posted comment now visibly flags when it resumed a turn-capped predecessor, closing the exact trust gap in the issue: a human reviewing the PR (or `cruise` advancing automatically) has a durable signal that verification claims in that comment may be inherited rather than freshly performed.
- `history.json` and the TUI History pane make the condition queryable rather than inferable from adjacent `TurnsUsed`/`MaxTurns` rows, for both the shipped TUI and `fabrik watch`.
- Stage-scoped storage generalizes correctly to multi-run capped chains and is immune to intervening comment-processing invocations.
- No new labels, no new config/YAML surface — purely additive to the item-state model, the invocation-finalization path, and the TUI event/history pipeline.

**Negative / Trade-offs:**
- **The two "suspicious shape" heuristics are log-only and accept false negatives by design.** The few-turn threshold (`< stage.MaxTurns/4`) and the phrase list are both deliberately conservative; broader detection is a follow-up, not a blocker, per the issue's own "optionally" framing.
- **Comment-processing retries do not get the same annotation.** A capped comment-processing run's retry carries the same underlying risk but is not flagged — an explicit, scoped gap rather than an oversight.
- **The annotation's absence of exact turn numbers** means a reader must still consult `history.json`/the History pane for the predecessor's own stats if they want them — the comment only signals that discontinuity occurred, not its magnitude.

## Related Work

- Issue #1081 (this issue) and its corroborating RallyRaffle installation report (comment thread).
- ADR-030 — Progress-Based Turn Extension: the `err == nil`-gated intra-dispatch loop this ADR's detection condition deliberately does not reuse.
- #847 — History under-counts time & cost (fixed the `history.json` append-dedup bug this ADR's new field builds on top of).
- #448 — Investigate why progress-based extension did not fire on Implement run that hit max_turns (established the "always-on diagnostic logging, not behind a debug flag" precedent this ADR's log-only heuristics follow).

**References:** [docs/state-machine.md § Turn-Cap Retry Provenance](../docs/state-machine.md), [docs/USER_GUIDE.md § History pane icon table](../docs/USER_GUIDE.md)
