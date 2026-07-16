# ADR 062: Child Board-Placement Retry Marker

**Date**: 2026-07-16
**Status**: Accepted
**Issue**: #984 — fix(engine): spawned child's initial board placement has no retry — stranded forever if it fails

## Context

`preImplement`'s `spawnChildren` (`engine/spawn.go`) creates each spawned child issue, adds it to the project board, links it as a `blockedBy` dependency of the parent, and finally sets its initial project Status to `Specify` (or the first non-Backlog, non-terminal column) via `UpdateProjectItemStatus`. That last call can fail in three ways: the call itself errors (network/API error, rate limit), `e.statusField` is `nil` (status-field metadata never populated), or no suitable status option exists on the board. In all three cases the original code logged a warning and moved on to the next child — no marker, no retry, no escalation.

Because `itemMayNeedWork`/`itemNeedsWork` (`engine/item.go`) both return `false` immediately when `stages.FindStage` finds no configured stage matching an item's board column, a child stranded in an unmatched column — typically `Backlog`, wherever GitHub defaults a newly-added project item with no explicit Status — is **never dispatched, ever**. This is a permanent, silent stall: the parent's `fabrik:children-spawned` idempotency guard is already set once `spawnChildren` returns successfully (spawning itself is not considered to have failed), and the parent is typically also blocked on the stranded child's closure via `blockedBy`/`checkDependencies` — so the parent stalls too, with no label or comment on either issue explaining why.

ADR-060 fixed a structurally similar problem (the no-work-needed Done-move losing track of itself on failure) with a durable marker (`fabrik:awaiting-done`) plus an `item.Status`-agnostic settle scan plus bounded-retry escalation. ADR-060's own "Sibling Audit" section named this exact bug as a known, deliberately-deferred follow-up. This ADR applies that same pattern here — with one structural adjustment ADR-060 did not need, described below.

## Decision

Write a new durable label, `fabrik:awaiting-placement`, on the **child** issue (not the parent) at the `UpdateProjectItemStatus` call site in `spawnChildren`, covering all three failure branches. The child, its board item, and its `blockedBy` link already exist successfully by the time this call is attempted — unlike `spawnChildren`'s other failure branches (invalid repo, `CreateIssue`, `AddProjectV2ItemById`, `AddBlockedByIssue`), which are fatal and abort the whole spawn loop by pausing the *parent*, this is a recoverable-in-place condition on the *child* and must not reuse that abort-and-pause path.

A new file, `engine/spawn_settle.go`, holds the settle/retry/escalation logic, mirroring `engine/no_work_needed_settle.go`'s shape: a dedicated retry-counter constant, an idempotent settle function, a retry-recording function that escalates at `MaxRetries`, an escalation function, and a marker-clear function.

### The one place this diverges from ADR-060: settle-scan sourcing

`fabrik:awaiting-done` is written on an item that is, by construction, still sitting in the column of the *real, matched* stage that emitted the decision — `stages.FindStage` always resolves a non-nil stage for it, so it passes `itemMayNeedWork`/`itemNeedsWork`'s `stage == nil` guard and is admitted into `deepFetchCandidates`, where the existing `poll.go` no-work-needed settle scan (sourced from `deepFetchCandidates`) finds it.

A child whose placement failed is different in kind: by definition it is sitting in a column with **no matching configured stage** — typically `Backlog`. `stages.FindStage` returns `nil` for it, and both `itemMayNeedWork` and `itemNeedsWork` return `false` at their `stage == nil` check, before either function even inspects labels. A settle scan built the same way as the no-work-needed one — sourced from `deepFetchCandidates` — would **never see this item**, because it never reaches `deepFetchCandidates` in the first place. Shipping that version would compile, could even pass a unit test built against a hand-constructed `deepFetchCandidates` fixture, and then silently never fire against a real board — reproducing a milder version of the exact bug this fix exists to close.

The child board-placement settle scan is therefore sourced directly from `board.Items`, added to `poll.go` immediately after `cleanupClosedIssueTransientLabels` (a call site that already iterates all of `board.Items` unconditionally every cycle, independent of `repoFilter`). The shallow board query already includes everything an ordinary retry pass needs — `ItemID`, `Status`, `Repo`, `Number`, `Labels` — with no deep fetch required. `itemMayNeedWork`/`itemNeedsWork` are **not modified**: this marker's retry work has nothing to do with stage dispatch (there is no stage to dispatch — that is the whole point), so routing it through the dispatch pre-filter would solve an already-solved problem the wrong way.

### Retry counting and escalation

Failed settle passes are counted against the existing `itemstate.StageRetryIncremented`/`Attempts`/`e.cfg.MaxRetries` mechanism, keyed by a dedicated constant, `"__child_placement__"` — double-underscore-wrapped like ADR-060's `"__no_work_needed__"`, unrepresentable as a real YAML stage `name:`, so it can never collide with a configured stage's own counter or with `"__no_work_needed__"`. Once `MaxRetries` is reached, `escalateChildPlacementFailure` fires: `fabrik:paused` is added to the **child**, `fabrik:awaiting-placement` is removed, an explanatory comment with manual recovery steps is posted on the **child**, and `itemstate.EnginePaused` is applied — mirroring `escalateNoWorkNeededFailure`/`escalateFailedStage` exactly for the child's own half of the story.

### The other new thing ADR-060 never needed: notifying the parent

`escalateNoWorkNeededFailure` posts its escalation comment on the same issue it's escalating. This fix additionally needs a **best-effort** comment on a *different* issue — the parent — since the parent has no other visibility into why it remains permanently blocked (via `blockedBy`) on a child that will never close. There is no structured child→parent link in the schema; the only edge is the free-text back-reference `childFooter` writes into the child's `Body` at spawn time. `notifyParentOfStalledChild` therefore performs a **lazy, escalation-only** `FetchItemDetails` call to read the child's `Body` (the only deep fetch anywhere in this fix — every ordinary settle pass uses only shallow `board.Items` fields), regex-parses the parent's `owner/repo#number` out of the footer via `parseParentFromChildBody`, and posts a best-effort comment on the parent. Every failure along this path — the deep fetch, the regex match, the comment post — is logged and swallowed; it runs last, after the child's own pause-and-comment escalation has already fully completed, so it can never block or fail the child's own recovery.

A parameterized label carrying the parent link verbatim (set alongside `fabrik:sub-issue` at spawn time) would be more robust against a human editing the child's body and removing the footer, and cheaper (no deep-fetch needed at escalation). It was rejected: the spec explicitly frames parent notification as best-effort only, and adding a new label kind for a once-per-child, escalation-only lookup is disproportionate to a mechanism that is allowed to simply fail quietly.

### Closed-child short-circuit

Unlike the no-work-needed case — where the Done-move is itself the remaining required work even on a closed issue, so `settleNoWorkNeeded` still completes it — a closed child needs no further board dispatch: the only purpose of correct placement was to let the pipeline process the child, which no longer applies once it is closed (manually, or resolved out-of-band). The settle scan therefore short-circuits: if `item.IsClosed` is observed on a child carrying the marker, `clearChildPlacementMarker` is called directly — no placement attempt, no retry increment, no escalation, no pause, no comment. This is a new decision beyond ADR-060's own precedent, made explicit here rather than left as an accidental side effect of reusing `settleNoWorkNeeded`'s shape verbatim (which does not have this branch, correctly, for its own different invariant).

### No change to `spawnChildren`'s own idempotency guard

`fabrik:children-spawned` continues to be added to the **parent** unconditionally once all children are created, added to the board, and linked, regardless of each individual child's board-placement outcome. This fix only makes the child's own placement durable and recoverable; it does not make the parent's guard conditional on it, per the issue's explicit scope.

## Rationale

### Why a durable GitHub label, not an `itemstate.Store` mutation?

Identical rationale to ADR-060: `itemstate.Store` does not survive an engine restart, and there is no idempotent artifact to safely redo here — a spawned child's board placement, once failed, has no side effect to reproduce on a Claude re-run the way a stage's worktree commits do. If the marker were in-memory only and the engine restarted mid-retry, the decision would be lost exactly as if the marker never existed, silently resurrecting the exact permanent-stall bug this ADR fixes. The marker must therefore be a GitHub label.

### Why generalize the ADR-060 machinery rather than build something new?

`itemstate.StageRetryIncremented`/`Attempts`/`StageRetryCleared`/`EnginePaused` are already generic, reusable via a dedicated constant, with zero changes needed to the `itemstate` package. This fix is a second instance of the same pattern family ADR-060 established — durable marker as the first mutation, `item.Status`-agnostic settle scan, dedicated non-colliding retry-counter constant, bounded escalation to `fabrik:paused` plus an explanatory comment — not a new abstraction.

### Why is the marker never removed by the closed-issue defensive sweep (`cleanupClosedIssueTransientLabels`)?

Same reasoning as ADR-060's `fabrik:awaiting-done` exclusion: if the sweep stripped `fabrik:awaiting-placement` from a closed child before the settle scan's own closed-child short-circuit had a chance to run its deliberate, observable clear path (`clearChildPlacementMarker`), a closed child mid-retry (below `MaxRetries`) would lose the marker via an entirely different, less-visible code path. Both the sweep and the settle scan's short-circuit run every poll, so the practical window is narrow, but excluding the marker keeps its removal paths exhaustively enumerable — exactly the property ADR-060 established for `fabrik:awaiting-done`.

### Why not extend `itemMayNeedWork`/`itemNeedsWork` with an early marker-bypass instead (the alternative sourcing option)?

This was considered and rejected. `itemMayNeedWork` is deliberately kept cheap and shallow — its own comment states "don't check labels here" for anything beyond the minimal pre-filter needed to decide whether a deep fetch is worthwhile. Adding a marker-specific bypass before its `stage == nil` guard would be a conceptual mismatch for a function whose contract is "no matching stage = nothing to do here": this marker's retry work is not stage dispatch and has no business flowing through the dispatch pre-filter at all. Sourcing the settle scan from `board.Items` directly is a strictly better fit, and correctly leaves `item.go` untouched.

## Consequences

**Positive:**
- A spawned child that fails its initial board placement can no longer be permanently, silently stranded. The failure is now durably recorded, retried every poll independent of board column, and eventually surfaced to a human on both the child and (best-effort) the parent.
- The fix is almost entirely a generalization of ADR-060's already-proven machinery — no new `itemstate.Mutation` type, no new configuration surface, no changes to `blockedBy`/`checkDependencies` resolution.
- The one genuinely new mechanism (best-effort parent notification via body-footer regex) is scoped tightly to escalation only, and is designed to fail safely and invisibly rather than risk destabilizing the child's own recovery.

**Negative / Trade-offs:**
- Parent-link recovery is fragile by design: a human editing the child's body and removing the `childFooter` back-reference breaks parent notification silently. This is an accepted trade-off given the spec's explicit "best-effort" framing for this specific step; a more robust parameterized-label alternative was considered and rejected as disproportionate (see Decision).
- The settle scan adds one label-set membership check per poll for every item on the board (not just candidates already headed toward a deep fetch), since it must scan `board.Items` directly rather than piggyback on `deepFetchCandidates`. This is a small, constant-time cost per poll cycle, bounded by total board size, and is the same shape of cost the existing `cleanupClosedIssueTransientLabels`/`cleanupClosedIssueLocks` sweeps already pay every poll.
- The interim landing behavior for a child whose placement hasn't yet succeeded is unchanged from before this fix (it stays wherever GitHub defaulted it, typically `Backlog`, until the settle scan succeeds or escalates) — this is intentional and explicitly out of scope; only the "never noticed, never retried" failure mode is fixed.

## Related Work

- [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md) — the direct precedent this ADR reuses, including the sibling-audit entry that named this exact bug as a deferred follow-up.

**References:** [docs/state-machine.md §6.9](../docs/state-machine.md), [docs/state-machine.md §6.7 (Pre-Implement Spawn Path)](../docs/state-machine.md)
