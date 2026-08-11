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

(A later human review round found this episode boundary incomplete for the operator-resume
recovery path the alert text itself instructs — see "Corrections from human review" below;
`mergeTrainRunawayAlerted` is now `map[string]int`, not `map[string]bool`.)

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
explanation — the member is never left paused with zero delivered explanation, even when the
underlying failure is persistent rather than transient. (A later human review round found the
original version of this escalation removed the marker regardless of whether the fallback
comment itself succeeded, which could reproduce exactly this failure mode under a persistent
outage — see "Corrections from human review" below.)

This reuses the shared `recordSettleRetry`/`clearSettleMarker`/`escalateSettle` helpers
(`engine/settle.go`) already backing four other settle scans in this family
(`fabrik:awaiting-done`, `fabrik:awaiting-member-close`, `fabrik:awaiting-close`,
`fabrik:awaiting-advance`) — this is the eighth instance of the ADR-1270 dedicated-settle-scan
pattern, not a new shape.

### Comment-then-label ordering — revised during Validate

The pre-existing code posted the comment before applying the pause labels, and this ADR
originally preserved that ordering as out of scope (the defect being atomicity/idempotency
*across* calls, not the order of operations within one call). A Validate-stage bot review
identified a residual crash window in that ordering: if the process died between
`markRunawayAlertOutstanding` and the two `addLabel` calls, a member could end up carrying
`fabrik:awaiting-runaway-alert` without ever actually being paused — violating
`settleRunawayGuardAlertScan`'s own invariant that a member carrying the marker also carries
`fabrik:paused`. `fireRunawayGuard` now applies `fabrik:paused`/`fabrik:awaiting-input`
**before** attempting the comment, closing that window; the settle scan's invariant now holds
unconditionally rather than "except mid-crash."

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
signal.

`escalateRunawayAlertFailure` stopping the retries by removing the marker (rather than
leaving it, as `fabrik:awaiting-advance`/ADR-1422 deliberately does) was this ADR's original
design — but a later human review round (`@verveguy`, changes-requested) found it did so
*unconditionally*, discarding its fallback comment's own error via the shared `escalateSettle`
helper. Under a persistent `AddComment` outage (not just a transient one), that meant: the
original alert fails, `MaxRetries` settle-scan retries fail, the fallback comment fails
silently, and the marker is removed anyway — reproducing #1533 itself (a member paused with
no delivered explanation) through the very machinery meant to fix it, and erasing the one
diagnostic signal (the marker) that would have let a human notice. `escalateRunawayAlertFailure`
no longer delegates to `escalateSettle`; it checks the fallback comment's own result directly
and only clears the marker (and records the member alerted) on confirmed success. On failure
the marker stays, and `settleRunawayGuardAlertScan` keeps retrying — primary alert, then
fallback — every poll, indefinitely, until a comment actually lands. See "Corrections from
human review" below.

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

## Corrections from human review

Two rounds of human PR review (`@verveguy`, changes-requested) found gaps in the design as
first implemented — both fixed in the same PR, both non-vacuously verified (see Verification
below). Recorded here rather than silently folded into the sections above, per this repo's
usual practice for a design that changes shape mid-PR.

**1. Escalation could silently reproduce #1533 itself under a persistent failure.**
`escalateRunawayAlertFailure` originally delegated to the shared `escalateSettle` helper,
which unconditionally removes the durable marker and discards its comment closure's error.
Under a persistent `AddComment` outage (lost token permission, a secondary rate limit
outlasting `MaxRetries` polls — not just a transient blip): the original alert fails, every
settle-scan retry fails, the fallback comment fails silently, and the marker is removed
anyway — leaving the member paused with zero delivered explanation and no diagnostic trace,
which is bug #1533 verbatim, reproduced through the very machinery meant to fix it, and
*harder* to detect than the original bug (the marker a human or settle scan would have
noticed is gone). Fixed: `escalateRunawayAlertFailure` no longer uses `escalateSettle`; it
checks the fallback comment's own result and only clears the marker (and records the member
alerted) on confirmed success. On failure the marker stays, so the settle scan keeps
retrying — primary alert, then fallback — every poll, indefinitely.

**2. Cross-episode idempotency ignored the operator-resume recovery path.**
`mergeTrainRunawayAlerted` was cleared only by `resetTrialCounter`, which fires solely on a
successful land. But the alert text itself instructs operators to manually remove
`fabrik:paused`/`fabrik:awaiting-input` to re-enable the train — a path that never calls
`resetTrialCounter`. With old trial timestamps still inside the rolling window, a second
genuine trip after a resume retriggered `fireRunawayGuard` for the same members while they
were still marked alerted: pause labels reapplied, no alert comment — reproduced against the
original branch by firing twice with no intervening reset (6 trials → 1 comment, 8 trials →
still 1 comment total). Fixed: `mergeTrainRunawayAlerted` is now `map[string]int`, recording
the trial count in effect when a member was last alerted. A later call is treated as
already-alerted only while its own count is `<=` the recorded value — trials cannot
accumulate while the guard keeps the queue paused, so an increase can only mean an operator
resumed the train and it genuinely tripped again, which now produces a fresh alert.

**3. Label-ordering crash window** (addressed above, under "Comment-then-label ordering").

Two further findings from the same review rounds were explicitly left as non-blocking,
follow-up-if-desired: `applyLabelAdd`'s `fabrik:paused` add can itself fail silently (an
existing, codebase-wide swallowed-error pattern, not unique to this function); and
`mergeTrainRunawayMu`'s cross-repo blocking scope was under-documented rather than
incorrect (now called out explicitly in `fireRunawayGuard`'s own doc comment).

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
  Hook 1/Hook 2 concurrent-firing race the e2e bed demonstrated, or under the narrower
  `fireRunawayGuard`-vs-`settleRunawayGuardAlertScan` race a Review pass on this issue found:
  `settleRunawayGuardAlert` now holds `mergeTrainRunawayMu` across its own `postComment` call
  (not just the post-success map update), so a stale Hook 1 call reprocessing a member that
  already carries the marker can never post concurrently with the settle scan's retry for
  that same member. Covered by
  `TestFireRunawayGuard_RacesSettleRunawayGuardAlert_NoDuplicateAlert`.
- `fireRunawayGuard`'s pause+alert loop (and `settleRunawayGuardAlert`'s retry) now hold a
  process-wide mutex for their duration — a deliberate, documented trade-off given how rare
  firing is (an exceptional infra-failure path), not a hot-path concern.
- Out of scope, unchanged: trial-counting, `isRunawayTripped`, the trip threshold/window, and
  #1528's green-trial exclusion (confirmed pre-existing and untouched by this fix, matching
  the issue's own "not caused by #1528" framing).
