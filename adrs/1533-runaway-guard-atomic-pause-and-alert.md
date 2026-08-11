# ADR 1533: Runaway Guard Pause+Alert Made Atomic and Idempotent

**Date**: 2026-08-11
**Status**: Accepted
**Issue**: #1533 — merge-train: runaway guard pauses a member without posting its alert
comment — paused with no stated reason

## Context

The runaway guard (ADR-059 D8) pauses every affected Queued member and posts an
explanatory alert comment on each when a repo's trial counter trips. `fireRunawayGuard` is
called from three independent sites — twice inside `runMergeTrainWorker`'s re-form loop
(Hook 1, running on the worker goroutine) and once from `routeQueuedGroup` (Hook 2, running
on the poll goroutine) — each constructing its own, possibly-overlapping `items` slice from
whatever local state it holds. Nothing prevented Hook 1 and Hook 2 from running concurrently
for the same `repoKey` once the shared trial counter tripped: the poll loop never checked
whether a worker was mid-firing.

Observed on the e2e bed (`TestMergeTrainRunawayGuardPausesBatch`, 4 all-poison members):

```
12:55:13 [merge-train] runaway guard fired for .../fabrik-test-beta: 6 trial(s) with zero successful lands within 1h0m0s — pausing 1 Queued member(s)
12:55:45 [merge-train] runaway guard already tripped for .../fabrik-test-beta (6 trial(s)) — pausing 2 Queued member(s) before dispatch
12:55:45 [merge-train] runaway guard fired for .../fabrik-test-beta: 6 trial(s) with zero successful lands within 1h0m0s — pausing 2 Queued member(s)
```

Three members were paused across these two firings (1 + 2), but the batch had four members.
The fourth (`#255`) ended up `fabrik:paused` with **zero alert comments** — it was processed
by neither firing's `items` slice, so no `fireRunawayGuard` call ever attempted a comment for
it. `fireRunawayGuard` itself compounds this: it posts the comment, logs-and-continues on
failure, and applies the pause labels regardless — so a transient `AddComment` failure
independently produces the identical stranded shape even when a member *is* included in a
firing's `items`. Once `fabrik:paused` lands, `groupQueuedByRepo` (the source of every future
Queued snapshot, including Hook 2's) permanently excludes that member — there is no path back
to a retry. This is the same "stranded with no signal" defect class this project has
repeatedly closed: ADR-060 (`fabrik:awaiting-done`), ADR-1422 (`fabrik:awaiting-advance`),
ADR-1097 (`fabrik:awaiting-close`).

A pass/fail split across three runs of the same pre-fix binary (FAIL, PASS, FAIL) confirmed
this is a genuine race, not a deterministic logic bug — a single green e2e run does not prove
the defect is fixed.

## Decision

Two complementary mechanisms, each closing a distinct failure mode:

### 1. Atomicity + per-episode idempotency (R2, R3)

`fireRunawayGuard`'s entire pause+alert loop for one call becomes a single critical section,
serialized by a new `mergeTrainRunawayMu sync.Mutex`. Within that section, a new in-memory set
`mergeTrainRunawayAlerted map[string]bool` (keyed `"owner/repo#N"`, mirroring
`mergeTrainEjectionCounts`'s existing flat-key convention rather than a nested map) records
which members have already been alerted **this episode**. A call re-encountering an
already-alerted member skips it entirely — no duplicate comment, no redundant (though
individually idempotent) label calls.

`resetTrialCounter` — the guard's only existing "this trip is over" signal, called on a
successful land — also clears every `mergeTrainRunawayAlerted` entry for the repo (matched by
key prefix, since entries are per-member). The next trip starts a fresh episode where every
member is eligible for a fresh alert.

This closes the case where the *same* member appears in two racing calls' `items` slices
(Hook 1's two call sites, or a Hook 1/Hook 2 race) — the shape the observed e2e log's
"already tripped ... pausing N before dispatch" line demonstrates is reachable. It does
**not** by itself close the case where a member is omitted from *every* call's `items`
slice, or where a single call's own `AddComment` fails outright — mechanism 2 covers those.

### 2. Durable retry for the "comment failed" residual (R1)

`fireRunawayGuard` still applies `fabrik:paused` + `fabrik:awaiting-input` unconditionally,
regardless of whether the comment succeeded — the pause is correct and must not be
gated on alert delivery. On a comment failure, instead of just logging and continuing, the
member is marked with a new durable label, `fabrik:awaiting-runaway-alert`
(`markRunawayAlertOutstanding`). A new per-poll settle scan, `settleRunawayGuardAlertScan`
(`engine/runaway_alert_settle.go`), sourced directly from raw `board.Items` — independent of
`groupQueuedByRepo`'s `fabrik:paused` exclusion, which would otherwise hide exactly the items
this scan exists to find — retries the alert every poll for any item carrying the marker,
regardless of whether any `fireRunawayGuard` call will ever reach that member again. On
success, the member is recorded in `mergeTrainRunawayAlerted` (so a still-in-flight or later
`fireRunawayGuard` call doesn't double-post) and the marker is cleared. After `MaxRetries`
failed retries, `escalateRunawayAlertFailure` posts a fallback comment carrying the same
explanation and removes the marker — the member is never left paused with zero delivered
explanation, even when the underlying failure is persistent rather than transient.

This reuses the shared `recordSettleRetry`/`clearSettleMarker`/`escalateSettle` helpers
(`engine/settle.go`) already backing four other settle scans in this family
(`fabrik:awaiting-done`, `fabrik:awaiting-member-close`, `fabrik:awaiting-close`,
`fabrik:awaiting-advance`) — this is the eighth instance of the ADR-1270 dedicated-settle-scan
pattern, not a new shape.

### Comment-then-label ordering unchanged

The pre-existing code already posts the comment before applying the pause labels; that
ordering is preserved. The defect was the lack of atomicity/idempotency *across* calls and
the lack of a retry path on failure, not the order of operations within one call — reordering
was considered and rejected as solving the wrong problem.

### One structural deviation from this family: no `fabrik:paused`-absence guard in the settle scan

Every sibling settle scan in this family (`settleMergeTrainMemberCloses`,
`settleNonDefaultBaseCloses`, `settleAwaitingAdvanceScan`) skips any item already carrying
`fabrik:paused`, because for those, the marker is written *before* the item reaches its
terminal paused state — a paused item there means "already escalated by this same
mechanism, stop retrying." Here, `fabrik:paused` is applied by `fireRunawayGuard`
**unconditionally**, in the same loop iteration as the comment attempt — a member carrying
`fabrik:awaiting-runaway-alert` always also carries `fabrik:paused`, from the very first poll
the marker exists. Gating `settleRunawayGuardAlertScan` on `fabrik:paused`'s absence would
make it a permanent no-op. The marker's own presence/absence is the sole retry-eligibility
signal; `escalateRunawayAlertFailure` removing the marker (rather than leaving it, as
`fabrik:awaiting-advance`/ADR-1422 deliberately does) is what stops the retries once the
fallback comment has been posted.

## Rejected alternatives

- **Reordering the comment before the pause labels, or gating the pause on comment
  success.** Considered in Research. Rejected: reordering doesn't fix a transient network
  failure, and gating the pause on the comment would leave a member neither paused nor
  alerted on a comment failure — a worse outcome than today's "paused but unexplained,"
  since the runaway guard's entire purpose is to stop the train regardless of whether the
  explanation lands.
- **Unifying the three call sites' `items` construction into one canonical live-computed
  set.** Considered in Research and Plan. Rejected: `current` (Hook 1a, the worker's
  in-progress members), `survivors` (Hook 1b, post-bisection-ejection), and the uncapped
  Queued snapshot (Hook 2) mean genuinely different things at their respective call sites,
  and collapsing them into a single live re-derivation risks losing that intent (e.g. Hook
  1b's `survivors` may have already had a poisoner ejected — "all Queued members" recomputed
  fresh could differ). The atomicity/idempotency mechanism achieves the same correctness
  guarantee (no member missed, no member double-alerted) without needing the three call
  sites to agree on what "the current member set" means.
- **Reusing `fabrik:bot-reprompted` as the idempotency-tracking label.** Considered in
  Research. Rejected: `fabrik:bot-reprompted` is scoped to the review-wait gate's re-prompt
  ladder — a structurally different "already alerted" concept with its own cycle boundary
  (bot-review wait, not a runaway-guard episode). A new label, `fabrik:awaiting-runaway-alert`,
  keeps the two mechanisms independently reasoned about.
- **Per-repo mutexes instead of one global `mergeTrainRunawayMu`.** Considered in Plan.
  Rejected: `fireRunawayGuard` fires only during genuine runaway incidents — a rare,
  exceptional path. Cross-repo serialization costs at most a few seconds of contention in
  the worst case, and is far simpler than a keyed-mutex-with-cleanup scheme a `sync.Map`-of-
  mutexes would require.

## Correction to ADR-059 §D8

§D8's "Two-hook dispatch" design states the one-poll-cycle gap between Hook 1 and Hook 2 is
acceptable because "beyond-cap members cannot form their own batch while the worker goroutine
is still active." That argument bounds only whether a *second train* can form — it says
nothing about whether Hook 2's own guard check can race Hook 1's `fireRunawayGuard` call for
the same repo, which the observed e2e log demonstrates it can. This ADR does not edit
ADR-059 in place (ADRs are historical decision records, not as-built docs, per this repo's
convention, following the identical precedent set by ADR-1528's own correction note);
`docs/state-machine.md`'s "Runaway guard (ADR-059 D8)" section is the as-built doc updated in
the same PR to describe the corrected contract: Hook 1 and Hook 2 *can* run concurrently, and
correctness on the alerting side now comes from `mergeTrainRunawayMu`/`mergeTrainRunawayAlerted`,
not from an assumption that the two hooks never overlap.

## Verification

`TestMergeTrainRunawayGuardPausesBatch` and its A2 unit-test analog are `//go:build e2e` and
require a live GitHub bed not available in this environment. Verified instead by:

- Four new unit tests in `engine/merge_train_test.go`:
  `TestFireRunawayGuard_IdempotentAcrossTwoFirings` (a member in two overlapping firings gets
  exactly one alert), `TestFireRunawayGuard_CommentFailureLeavesMarkerAndRetriable` (a comment
  failure applies the marker, does not mark the member alerted, and a later firing retries
  the comment), `TestResetTrialCounter_ClearsRunawayAlertedIdempotency` (a new episode after
  `resetTrialCounter` re-alerts), and `TestRouteQueuedGroup_RunawayGuardHook2AlertsEveryMember`
  (A2 — Hook 2 coverage, calling `routeQueuedGroup` directly with the counter pre-tripped).
- Five new unit tests in `engine/runaway_alert_settle_test.go` covering the settle scan's
  retry-succeeds, retry-fails-marker-stays, skip-without-marker, deliberate
  does-not-skip-paused-items (the structural deviation above), and
  escalate-posts-fallback-at-MaxRetries paths.
- The existing runaway-guard unit suite (`TestRunawayGuard_Fires`,
  `TestRunawayGuard_NormalBisectionNotTripped`,
  `TestRunawayGuard_BisectionExceedsThresholdWithoutTripping`,
  `TestFireRunawayGuard_PauseVisibleToCacheAndEcho`) passes unmodified — the rewrite reuses
  the shared `postComment`/`addLabel` helpers (identical cache/echo write-through behavior to
  the manual `AddComment`/`AddLabelToIssue` + cache/echo blocks they replace) and preserves
  every externally-observable behavior for a single, non-racing `items` call.
- Non-vacuity (A4): the idempotency skip (`mergeTrainRunawayAlerted` check) was temporarily
  neutralized and `TestFireRunawayGuard_IdempotentAcrossTwoFirings` re-run against the
  unfixed code, confirming it fails — member #1 (present in both firings) receives 2 alert
  comments instead of 1. Full red-run output recorded in the PR body.
- `go build ./... && go vet ./... && go test -race ./...` — full engine suite green,
  including the new `TestAddCommentCompliance` allow-list update for the extracted
  `runawayGuardAlertMessage` formatter helper.

A1's "3 consecutive e2e passes" and A2's live-bed portion require a real GitHub bed and are
flagged for Validate to run against the e2e infrastructure.

## Consequences

- Every member the runaway guard pauses — through any of its three call sites — now either
  receives the alert comment directly, or is left with a durable marker guaranteeing a
  settle-scan retry (and, eventually, a fallback comment) until it does. No member can end up
  `fabrik:paused` with zero delivered explanation and no retry path.
- No member can accumulate more than one alert comment per guard episode, even under the
  Hook 1/Hook 2 concurrent-firing race the e2e bed demonstrated.
- `fireRunawayGuard`'s pause+alert loop now holds a process-wide mutex for its duration — a
  deliberate, documented trade-off given how rare firing is (an exceptional infra-failure
  path), not a hot-path concern.
- Out of scope, unchanged: trial-counting, `isRunawayTripped`, the trip threshold/window, and
  #1528's green-trial exclusion (confirmed pre-existing and untouched by this fix, matching
  the issue's own "not caused by #1528" framing).
