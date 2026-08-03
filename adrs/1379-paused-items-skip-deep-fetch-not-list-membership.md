# ADR 1379: Paused items skip the deep-fetch call, not list membership

**Status:** Accepted
**Date:** 2026-08-03
**Issue:** [#1379](https://github.com/handarbeit/fabrik/issues/1379)

## Context

`fabrik:paused` means "a human must act before anything else happens." The engine
already honours that for dispatch (`poll.go`'s catch-up loop skips paused items) and
for the catch-up gates, but not for deep-fetch admission: `selectDeepFetchCandidates`
never tested `fabrik:paused` at all, so a paused item carrying any of the existing
admission signals — an expired periodic cooldown, or a bypass label such as
`fabrik:awaiting-review`/`fabrik:rebase-needed` — still paid a full per-item
`FetchItemDetails` GraphQL read on every poll that admitted it. On the dev instance, a
handful of items parked awaiting a human merge decision drove ~418 `FetchLinkedPR`
refetches in a ~10h window (see Consequences for why that number is not, in the end,
attributable to this fix). Pausing is the natural operator gesture for "set this aside"
— it should also stop the engine spending on it, not just stop it from acting.

## Decision

**Skip the `FetchItemDetails` call only; keep the item in `deepFetchCandidates`.** The
new gate sits in `selectDeepFetchCandidates` (`engine/poll.go`), immediately after the
existing cleanup-stage skip and before the `FetchItemDetails` call:

```go
if hasLabel(board.Items[i].Labels, "fabrik:paused") && !cycleSet[iKey] {
    if _, snapErr := e.store.Get(repo, board.Items[i].Number); snapErr == nil {
        deepFetchCandidates = append(deepFetchCandidates, board.Items[i])
        continue // skip FetchItemDetails; keep list membership
    }
    // not yet in the store — fall through and fetch once for a baseline
}
```

**Rejected alternative: exclude paused items from `deepFetchCandidates` entirely.**
That would also stop `runValidatePRTerminalAdvance`'s per-poll `FetchLinkedPR` REST
call (a separate, unscoped cost source — see Consequences), giving a larger real-world
saving. It was rejected because two existing, ADR-protected, test-covered behaviours
depend on **list membership**, not on the item having been through `FetchItemDetails`:

- `runValidatePRTerminalAdvance` (ADR-056 D2) advances a paused Validate-stage item to
  Done when its linked PR merged externally — the merged-while-paused self-heal.
  Covered by `TestValidatePRTerminalAdvance_TableDriven`'s three `+paused/merged`
  cases and by `TestConvergencePausedRecovery_PRMerged_AdvancesToDone`, which drives
  the full `poll()` pipeline (not just the handler directly).
- `settleRevalidateScan`'s own doc comment states it "runs on ALL candidates
  unconditionally (paused items included — FR-5)." `fabrik:revalidate`'s entire
  purpose is to force Validate re-entry despite pause. Covered end-to-end by
  `TestRevalidate_ClearsLabelsOnValidateStage`.

Excluding paused items from the list would have silently regressed both at the
production call site while their handler-level unit tests kept passing — the exact
false-green trap the issue's own Verification Note warns about for AC1/AC2, just for a
different pair of functions. Neither consumer reads `item.Comments` or
`item.LinkedPR*` (the fields `FetchItemDetails` populates); both only need the item to
be *present* in the slice they iterate. Skipping only the fetch, not the append,
satisfies R1 exactly as specified with a minimal, localized diff, and requires no
re-homing of either consumer onto a `board.Items`-sourced settle scan.

**Gate on `!cycleSet[iKey]`, not on pause alone.** `itemNeedsWork`'s "a human comment
resumes a paused item" check (`engine/item.go`) needs fresh `item.Comments`, which only
`FetchItemDetails` populates. A bare "paused → never fetch, full stop" rule would
permanently blind that check in any deployment without healthy webhooks. Gating on
cycleSet non-membership lets real activity — a new comment, a label mutation — still
fall through to a normal fetch (cycleSet is populated by the `mayNeedWork` Store
observer whenever `LabelsChanged`/`CommentsChanged`/etc. fires, whether from a webhook
delta applied directly or from the pause-unaware `runProbeAndDeepFetch` picking up an
`updatedAt` drift earlier in the same poll and firing `ItemDeepFetched`) — while a
genuinely idle paused item (no cycleSet entry) is skipped.

**Unpause re-admission needs no new plumbing (R2).** `board.Items[i].Labels` is always
the current poll's live snapshot: `FetchProjectBoard`, the shallow board-wide read that
runs at the top of every poll independent of this gate, refreshes it every cycle. The
moment `fabrik:paused` is removed on GitHub, `hasLabel(...)` reads `false` on the very
next poll with no cycleSet dependency at all — confirmed directly (not assumed) by
`TestSelectDeepFetchCandidates_UnpauseReadmitsWithinOnePoll`, which reproduces the
"paused, skipped" → "unpaused, fetched" transition across two calls to
`selectDeepFetchCandidates` with only the label and (per R2's own hint about the
observer path) a `cycleSet` entry changed between them — no restart, no store reset.

**`notInStore` fallback retained as a narrow escape hatch.** A never-before-seen
paused item (no store entry yet) falls through and gets a one-time baseline fetch,
mirroring the existing `notInStore` bypass in the pre-filter above it. This costs
nothing in production, where `CacheImpl`-backed deployments always seed new items via
`runProbeAndDeepFetch` before this code runs; it exists to avoid stranding a paused
item without any store baseline in a non-cache (`GitHubAdapter`-only) configuration.

**`fabrik:auto-merge-enabled` and `fabrik:revalidate` are not special-cased.** Both are
part of the existing `hasAwaitingLabel` bundle (four labels) that the pre-filter
already treats identically for admission. Because the new gate never removes an item
from `deepFetchCandidates`, `fabrik:revalidate`'s FR-5 list-membership guarantee holds
regardless of which bypass label accompanies it — no differential treatment is needed
for either label.

## R4 audit result

What was checked, and what — if anything — depends on a paused item having been
through `FetchItemDetails` (as opposed to merely appearing in `deepFetchCandidates`):

- **The settle-scan family** (`engine/ci_settle.go`, `engine/poll_settle.go`,
  `engine/merge_train_member_close_settle.go`, `engine/close_nondefault_base_settle.go`).
  All are sourced directly from `board.Items`, independent of `selectDeepFetchCandidates`
  by construction (ADR-1270's pattern) — unaffected regardless of what this change does.
  Four of the five explicitly *exclude* paused items by design
  (`settleAwaitingCIScan`, `settleMergeTrainMemberCloses`, `settleNonDefaultBaseCloses`,
  `settleAwaitingPlacementScan`/`settleChildPlacements`), each with a near-identical
  doc comment: "an operator is investigating for an unrelated reason and this scan
  must not fight them." Only `settleClaudeLimitLabelSweep` (ADR-1183) genuinely
  processes paused items — by design, since the account-wide usage-limit suspension it
  clears is not per-issue. Confirmed still passing:
  `TestSettleClaudeLimitLabelSweep_RemovesWhenNotSuspended` (AC5, R6).
- **`runValidatePRTerminalAdvance`** (`engine/pr_terminal_advance.go`, ADR-056 D2) —
  depends on list membership only (see Decision above). Makes its own `FetchLinkedPR`
  REST call regardless of GraphQL cache state, so it is untouched by this change either
  way. Confirmed via `TestValidatePRTerminalAdvance_TableDriven` and
  `TestConvergencePausedRecovery_PRMerged_AdvancesToDone`.
- **`settleRevalidateScan`** — list membership only (see Decision above). Confirmed via
  `TestRevalidate_ClearsLabelsOnValidateStage`.
- **`settleSHAInvalidationScan`** (`engine/poll_settle.go`) — reads
  `snap.LinkedPR()` from the store, not `item.LinkedPR`, so needs list presence, not a
  fresh fetch. Unaffected.
- **`engine/janitor.go`** — audited; no reference to `fabrik:paused`,
  `FetchItemDetails`, `.Comments`, or `LinkedPR` anywhere. Operates on closed/off-board
  status and worktree disk state via the store, independent of pause. No dependency
  found; no code change made (in scope per the issue as "audit only, unless the audit
  finds a dependency").
- **`engine/startup.go`** (`checkStageColumnAlignment`, `runStartupTransientLabelScan`)
  — audited; both are shallow board-column-name checks or closed-items-only scans, no
  paused dependency. No dependency found.
- **`runProbeAndDeepFetch`** (`engine/terminal.go`) — runs *earlier* in the same poll
  than `selectDeepFetchCandidates`, gated purely on cache staleness, with **no**
  `fabrik:paused` awareness. It deep-fetches any item whose `updatedAt` has drifted,
  paused or not. This is deliberately left unscoped (see Consequences) — it is also
  precisely the mechanism that makes unpause re-admission via cycleSet work without a
  webhook (see Decision, R2).

No genuine dependency on paused-item deep-fetch was found in-scope; the two real
list-membership dependencies (ADR-056 D2, FR-5) were preserved by design rather than
special-cased as an exception.

## Consequences

- A paused item admitted only via an expired cooldown or a bypass label
  (`fabrik:awaiting-review`, `fabrik:rebase-needed`, `fabrik:auto-merge-enabled`,
  `fabrik:revalidate`) no longer costs a `FetchItemDetails` GraphQL call each poll it
  remains parked and quiet. It still appears in `deepFetchCandidates` (unfetched), so
  `runValidatePRTerminalAdvance`'s merged-while-paused self-heal and
  `settleRevalidateScan`'s FR-5 guarantee are unaffected.
- Removing `fabrik:paused` re-admits the item within one poll cycle: no restart, no
  store reset, no operator action beyond the label removal itself.
- **This fix's real-world GraphQL/REST savings are narrower than the issue's own
  motivating cost evidence.** The Research stage identified two cost sources that are
  both outside this issue's stated scope (`engine/poll.go`, `engine/janitor.go`,
  `docs/state-machine.md`) and are unaffected by this change:
  - `runProbeAndDeepFetch` (`engine/terminal.go`) runs earlier in the same poll,
    pause-unaware, and deep-fetches any item whose `updatedAt` drifted — in a
    `CacheImpl`-backed production deployment (always the case per `engine.go`'s `New`
    wiring), this can make a subsequent `selectDeepFetchCandidates` call for the same
    item a free cache hit or a no-op regardless of this fix, but does not itself
    respect pause.
  - `runValidatePRTerminalAdvance`'s per-poll `FetchLinkedPR` REST call, made for
    every item in `deepFetchCandidates` (paused or not, by design), is very likely the
    actual source of the issue's own headline evidence ("~418 `FetchLinkedPR`
    refetches" — a REST call this ADR's fix does not touch, since paused items remain
    in the list by design).

  Both are legitimate follow-up targets, deliberately left out of this change because
  addressing them changes what's admitted into `deepFetchCandidates` at all — a much
  larger, riskier change than R1 as scoped, and one that would require re-homing the
  two list-membership dependencies documented above.
- Non-paused admission is unaffected: the new gate is a strict subset of the paused
  case, and the existing five admission criteria ((a) cycleSet, (b) cleanup stage, (c)
  bypass label, (d) expired cooldown, (e) not-in-store) are entirely untouched for any
  item without `fabrik:paused`.
