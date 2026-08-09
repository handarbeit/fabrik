# ADR 1422: Terminal-Advance Escalation and Settle Scan

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1422 — A terminal advance that cannot resolve its Status option strands the issue with
no owner and no signal

## Context

`advanceToNextStage` (`engine/stages.go`) is the single mutation that moves a project board item's
Status forward. It is called from two terminal-advance sites: `advanceValidateTerminalItem`'s
merged-PR path (`engine/pr_terminal_advance.go`, shared by the open-item owner
`runValidatePRTerminalAdvance` and the closed-item owner `settleClosedValidateAdvance`, ADR-1387) and
`advanceConvergedPRToDone` (`engine/merge_gate.go`, reached via `checkAutoMergeConvergence`). Before
this fix, a failure at either site — most commonly `advanceToNextStage`'s own
`"no status option %q found on project board (available: %v)"` error, when the target stage named by
`stages.NextStage` has no matching column on the operator's board — only logged a warning and
returned:

```go
// engine/pr_terminal_advance.go
e.logf(item.Number, "warn", "pr-terminal: could not advance to Done: %v\n", aerr)
```

The issue stayed open with every stage complete and a merged, green PR: nothing red, nothing paused,
nothing requesting attention. Because native dependency edges clear on **close**, not on **merge**,
every downstream issue that listed the stranded issue as a blocker kept holding `fabrik:blocked`,
waiting on work that had already shipped. Reported by @bdueck in #1082: three merged issues stranded
five downstream slices, and the stall was found only by reading `fabrik.log` directly, after it had
been in effect for some time — the most expensive shape of stall Fabrik can produce, since the
natural operator response to "quietly working" is to wait longer.

Research for this issue found the situation was not quite "never retried" — `advanceValidateTerminalItem`'s
closed-item half is already `board.Items`-sourced via `settleClosedValidateAdvance` (ADR-1387), and
`advanceConvergedPRToDone`'s admission-gated path mostly keeps retrying as long as
`stage:Validate:complete` stays present, since a closed item carrying that label is admitted to
`deepFetchCandidates` post-ADR-1387. But "mostly retries, and invisible either way" is not a
guaranteed recovery path: anything else in the admission pipeline (an earlier Phase-1 handler
claiming the item, cooldown/dedup logic, write-access gating) can starve `advanceConvergedPRToDone`
on a given poll, with zero signal either way — exactly the "admission-gated retry is fragile even
when it mostly fires" shape ADR-1270 already documented for `fabrik:awaiting-ci`.

This is the seventh instance of the "dedicated settle scan" pattern already established in this
codebase: `settleAwaitingCIScan` (ADR-1270), `settleNoWorkNeeded` (ADR-060),
`settleMergeTrainMemberCloses` (ADR-061), `settleChildPlacement` (ADR-062),
`settleNonDefaultBaseCloses` (ADR-1097), and `settleQueuedReviewFindings` (ADR-1208) all follow the
same shape: durable label → per-poll scan sourced from `board.Items` → bounded retries via
`e.cfg.MaxRetries` → `fabrik:paused` + explanatory comment on exhaustion.

## Decision

Add a new durable label, `fabrik:awaiting-advance`, written **only in the failure branch** of a
shared wrapper, `recordAdvanceOutcome`, that both terminal-advance call sites now route through in
place of calling `advanceToNextStage` directly. `advanceToNextStage` is the *last* mutation at both
call sites — every other side effect (gate-label clearing, completion-label filling,
`fabrik:auto-merge-enabled` removal) has already landed by the time it can fail — so this mirrors
ADR-1097's `fabrik:awaiting-close` shape (single at-risk call, marker on failure only), not ADR-060's
`fabrik:awaiting-done` (multi-call chain, marker written unconditionally first).

**One shared label/counter/scan for both call sites, not two.** Both failures are the identical
"final board-Status move failed" shape with identical recovery — a bare retry of
`advanceToNextStage`, since `stages.FindStage(e.cfg.Stages, item.Status)` always resolves back to the
same stage the original caller had (the move is exactly what didn't happen). A single owner avoids
duplicating the label/comment/escalation machinery for no behavioral difference.

**First-failure comment, not just an escalation comment (R1, R5).** Unlike `fabrik:awaiting-close`,
which only comments at escalation, `markAdvanceFailureOutstanding` posts a one-time explanatory
comment on the very first failure — naming the failing stage and embedding the underlying error
verbatim, gated on the label's own prior absence (mirroring `handleUsageLimitExit`'s
`fabrik:claude-limit` shape). `advanceToNextStage`'s own "no status option" error text already names
the missing option and the options that do exist, so no new typed-error plumbing is needed. Every
failing pass still counts toward the retry budget, but only the absent→present transition posts a
comment.

The retry mechanism (`engine/advance_settle.go`) is a self-contained scan, **not** wired into
`itemMayNeedWork`/`itemNeedsWork` or `deepFetchCandidates`, and **not** added to
`transientLifecycleLabels`:

- `settleAwaitingAdvanceScan` (called from `poll()` in `engine/poll.go`, immediately after
  `settleClosedValidateAdvance`) runs unconditionally every poll. It iterates the **raw
  `board.Items`**, checking only `hasLabel(item, "fabrik:awaiting-advance")`, skipping items carrying
  `fabrik:paused`, and skipping items already present in this poll's shared `advancedItems` dedup
  map.
- It shares that `advancedItems` map with `runValidatePRTerminalAdvance`/`settleClosedValidateAdvance`
  (ADR-1387), which run immediately before it in the same `poll()` pass: for a closed item stuck at
  Validate, that pair already retries `advanceToNextStage` unconditionally every poll and will have
  already marked the item, so this scan is a harmless no-op there. It is the exclusive retry-owner
  only for the two genuine gaps: an open item admission-gated out of `runValidatePRTerminalAdvance`'s
  `deepFetchCandidates` source, and `advanceConvergedPRToDone`'s path.
- Retries reuse the existing generic `recordSettleRetry`/`clearSettleMarker` helpers
  (`engine/settle.go`), keyed by a dedicated constant, `"__awaiting_advance__"` (same
  double-underscore-wrapped, YAML-unrepresentable shape as its siblings). Once `MaxRetries` is
  reached, `escalateAwaitingAdvanceFailure` fires: `fabrik:paused` is added and an explanatory comment
  naming the attempt count with the manual-fix instruction is posted — but, unlike every sibling
  settle scan, `fabrik:awaiting-advance` is **not** removed (see "Why does escalation not remove the
  marker" below); `escalateAwaitingAdvanceFailure` therefore does not call the shared `escalateSettle`
  helper, inlining the same pause/comment/`EnginePaused` steps without the marker removal.
- Because the marker survives escalation, its retry counter must be reset explicitly once an operator
  removes `fabrik:paused` — otherwise the very next failure would immediately re-escalate instead of
  getting a fresh `MaxRetries` budget. Both retry owners do this: `advanceValidateTerminalItem`'s
  guard, `awaitingAdvanceStuckOrReset`, resets the counter (via `StageRetryCleared`) the moment it
  observes the item is no longer paused, before proceeding; `settleAwaitingAdvanceScan` calls the same
  reset logic (`awaitingAdvanceResetIfUnpaused`) directly, since its own pre-filter already excludes
  paused items.

No changes to `itemMayNeedWork`, `itemNeedsWork`, or `transientLifecycleLabels` are needed or made.

**No silent fallback to Done (R4).** #1082 suggested falling back to `Done` when the intermediate
option is absent. Rejected as the primary mechanism: it would paper over a misconfigured board and
hide this exact bug entirely. No fallback of any kind is implemented.

## Rationale

### Why a durable GitHub label, not an `itemstate.Store` mutation?

Same reasoning as every prior settle scan in this family: `itemstate.Store` does not survive a
restart, and there is nothing idempotent-by-replay here except the status-move call itself. An
in-memory-only marker would silently lose the outstanding-advance decision across a restart —
exactly the failure mode #1422 reports.

### Why write the marker only on failure, not unconditionally beforehand?

Identical to ADR-1097/ADR-061's reasoning: there is exactly one at-risk call
(`advanceToNextStage`'s `UpdateProjectItemStatus`), not a multi-call chain like ADR-060's. Writing
the marker unconditionally before it would mean adding-then-immediately-removing a label on every
successful advance (the overwhelmingly common case) for no correctness benefit.

### Why a single shared mechanism for two call sites instead of two independent ones?

Both `advanceValidateTerminalItem` and `advanceConvergedPRToDone` fail at the exact same call
(`advanceToNextStage`) with the exact same recovery (retry the bare call once the board is fixed).
There is no caller-specific state to preserve across the retry — the retry needs only `board`,
`item`, and the stage resolved fresh from `item.Status`. Two independent mechanisms would duplicate
the label/comment/escalation logic for zero behavioral difference and create two counters an operator
would need to reason about separately for what is, from the board's perspective, one failure class.

### Why a `board.Items` scan instead of a `deepFetchCandidates`-based settle-scan shape?

Same distinction ADR-1097/ADR-061/ADR-1270 already drew: by the time `advanceToNextStage` can fail at
either call site, the item's own stage is already gate-complete (`stage:<X>:complete`, or the
gate-checked labels `advanceValidateTerminalItem` fills first, are already present) — there is no
per-stage redispatch risk this marker needs to guard against. Research confirmed
`advanceConvergedPRToDone`'s path is reachable only through the admission-gated Phase 1 catch-up
chain, which is precisely the fragility a `board.Items`-sourced scan exists to bypass.

### Why post a comment on first failure, when ADR-1097's `fabrik:awaiting-close` only comments at escalation?

`fabrik:awaiting-close` covers a narrower failure surface (`CloseIssue`, typically rate-limiting or a
transient blip) that self-resolves quickly in the overwhelming majority of cases — commenting on every
first failure there would be noisy relative to the signal. `fabrik:awaiting-advance`'s dominant real-world
cause (a missing board Status option) does *not* self-resolve without operator action — the whole point
of #1422 is that nothing currently tells the operator anything is wrong. R1 explicitly requires
first-failure visibility "where the operator is already looking," not just at exhaustion.

### Why does escalation not remove the marker, unlike every sibling settle scan?

Every prior settle scan in this family removes its marker on escalation via the shared
`escalateSettle` helper, whose own doc comment justifies this with "dispatch/retry suppression is no
longer needed once `fabrik:paused` takes over." That holds for their direct call sites only because
each one unconditionally re-creates the marker on its very next relevant poll regardless of pause
state — e.g. `closeIssueIfNonDefaultBase` (ADR-1097) has no `fabrik:paused` guard at all, so if its
retry fails while the issue happens to be paused for an unrelated reason, it simply re-marks the issue
outstanding again next poll.

`advanceConvergedPRToDone` (`merge_gate.go`) does not have this property: removing
`fabrik:auto-merge-enabled` up front structurally prevents `checkAutoMergeConvergence` from ever
re-entering it, so it fires **at most once per episode** (this is also why it does not need
`advanceValidateTerminalItem`'s `awaitingAdvanceStuckOrReset` guard — there is no unconditional
re-entry to loop on in the first place). For an item whose *only* driver is
`settleAwaitingAdvanceScan` — i.e. every item that reached the stranded state through this call site
— removing the marker at escalation would permanently strand it: nothing would ever re-create it, even
after an operator fixes the board and removes `fabrik:paused` alone, which the escalation comment's
own recovery instruction promises is sufficient (found in PR #1469 review, second round; the first
round's fix — the `awaitingAdvanceStuckOrReset` guard on `advanceValidateTerminalItem` — only covers
that one call site's direct-re-entry loop, not this settle-scan-exclusive recoverability gap).

Keeping the marker in place through the pause is safe: `settleAwaitingAdvanceScan`'s own admission
guard already skips any item carrying `fabrik:paused`, so the coexistence of both labels is inert
until the pause is lifted — at which point the scan can see the item again with no further signal
needed, and the counter reset (above) ensures it gets a genuinely fresh retry budget rather than
re-escalating on the first subsequent failure.

## Consequences

**Positive:**
- A terminal advance stranded by a misconfigured board is visible on first failure — a durable label
  and a one-time comment naming the missing option and the options that exist — instead of a
  log-only warning no operator is watching for.
- Adding the missing board Status option is sufficient to unstick every stranded item on the very
  next poll — no engine restart, no manual re-dispatch — including one that was previously escalated
  and paused: removing `fabrik:paused` alone is enough, with no manual re-labeling required (the
  settle-scan-exclusive recoverability fix from PR #1469 review, second round).
- Downstream dependents unblock as a direct consequence, since the underlying issue actually reaches
  its terminal board state (and, independently, its GitHub close) once the advance succeeds — closing
  the specific harm #1422 reports, not merely making the advance itself succeed.
- Reuses the generic `recordSettleRetry`/`clearSettleMarker` helpers shared by six prior settle scans
  for retry counting and marker clearing, so most of this remains a structurally identical instance of
  the pattern — the one deliberate deviation (escalation not removing the marker) is isolated to
  `escalateAwaitingAdvanceFailure`, which does not call the shared `escalateSettle` helper.
- Does not touch `itemMayNeedWork`, `itemNeedsWork`, `transientLifecycleLabels`, or any
  dispatch-suppression code — cannot regress the terminal-skip optimization or interact with any
  other gate label, by construction.

**Negative / Trade-offs:**
- The marker is written only in the failure branch, not unconditionally first — an engine crash in
  the narrow window between the `UpdateProjectItemStatus` request being sent and its response being
  processed would leave neither a marker nor a completed advance for that one crash. Accepted,
  consistent with every other single-call escalation site in this engine.
- `settleAwaitingAdvanceScan` scans all of `board.Items` every poll (a cheap label-membership check),
  rather than a smaller subset — negligible cost, bounded by the small, transient number of issues
  mid-outstanding-advance at any time.

## Sibling Audit (R6)

Issue #1422 named four warn-and-return call sites to audit:

- **`engine/pr_terminal_advance.go:191`** and **`engine/merge_gate.go:627`** — the two genuine gaps,
  both closed by this ADR's mechanism.
- **`engine/no_work_needed_settle.go:178`** — **already fully covered, not a gap.** This line sits
  inside `settleNoWorkNeeded` itself (owned by `fabrik:awaiting-done`, ADR-060), which already calls
  `recordNoWorkNeededRetry` → bounded retry → `escalateNoWorkNeededFailure` (pause + comment) on this
  exact failure. The issue body's "warn, no escalation" characterization was stale by the time of
  this fix; no change made here.
- **`engine/closed_item_advance_settle.go:76`** — **already covered, deliberately unbounded by design
  (ADR-064).** `settleClosedItemsToDone` already retries every poll, sourced from `board.Items`,
  independent of dispatch admission — it already has an owner in the sense R2 cares about.
  `MaxRetries`-bounded escalation was deliberately **not** added there: ADR-064's own rationale ("no
  marker to lose or leak... a bare retry-forever loop is sufficient") reflects a structural
  difference from every settle scan in this family — that call site never writes a durable marker in
  the first place, so there is nothing for a restart to lose track of. Left unchanged; R3's bounded-retry
  requirement is not retroactively applied there.

Three other `advanceToNextStage` call sites exist in the codebase (`engine/poll.go`,
`stages/stages.go`, `engine/merge_train.go`) but are not named in #1422's Scope section and are left
untouched — a candidate follow-up, not fixed by this issue.

**References:** [ADR-1097: Non-Default-Base Explicit Close Retry](1097-non-default-base-close-retry.md)
(the closest structural precedent), [ADR-1387: Closed Items Are Never Dispatched](1387-closed-items-never-dispatched.md)
(defines the current closed-item admission rule this issue's Research relied on),
[ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md) (the "admission-gated retry is
fragile even when it mostly fires" rationale and the direct template for this pattern),
[ADR-064: Closed-Item-At-Any-Stage Advance To Done](064-closed-item-any-stage-advance-to-done.md)
(rationale for `closed_item_advance_settle.go`'s deliberately unbounded retry, audited above and left
unchanged), ADR-060/061/062/1208 (the other instances of the dedicated-settle-scan pattern this ADR
follows), issue #1422, issue #1082
