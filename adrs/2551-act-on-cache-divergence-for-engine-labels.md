# ADR 2551: Act on Cache Divergence for Engine Lock and State Labels

**Date**: 2026-11-04
**Status**: Accepted
**Issue**: #2551 — confirm a divergence live, re-apply once, and never advance on a diverged label (source: a follow-up on community report #2533)
**Builds on**: ADR 2170 (the `cache-divergence` anomaly), ADR 2348 (observed-at ordering), ADR 2159 (engine-write protection), ADR 2350 (live reads are applied to the Store), ADR 2341 (the fail-closed pause guard)

## Context

The channel's `cache-divergence` anomaly (ADR 2170) reports when a later listing, probe or deep fetch contradicts a recent engine label write. It was observation only. The observed-at rule (ADR 2348) keeps the engine's value for a 30 s grace window and then accepts the listing, so a write GitHub reported as successful but did not keep was silently lost.

On 0.0.90 (2026-10-31) four such divergences occurred on one repo. Two have consequences:

- the engine added `fabrik:locked:<id>` and a later read showed it absent while the worker ran;
- the engine removed `stage:Review:complete` when a Review rework started, and a read 233 s later still showed it present. The non-Validate catch-up advance (`runCatchUpPhase2`) has no worker or rework guard and reads `item.Labels`, so it would advance an item whose rework was never checked.

## Decision

### 1. The existing detector is the only trigger

No second detector. `noteChangeForDivergence` already knows the item, label and written direction when it raises the event; it now also appends a structured `labelDivergence` record, which `onChange` drains and passes to `Engine.handleLabelDivergence`. The hook is void, non-blocking and panic-recovered. Consequence: the repair exists exactly when the channel hub does (production `Run()` always starts one; a hub-less engine, such as `NewWithDeps`, has no detector and so no repair).

### 2. Confirm with one live read; re-apply at most once

The observer goroutine runs on the writer's goroutine, so it makes no GitHub call. It marks the label as diverged (memory) and queues the record (capacity 64, drop and log on overflow) for one tracked worker goroutine, started and stopped with the channel consumer. Per record, in order:

1. **Recurrence.** The same `(item, label)` acted on within `--cache-divergence-window` is reported and not touched again. This is also the once-per-episode bound.
2. **Intent.** The engine's last write for that label (recorded at the one funnel every engine label write goes through, `syncLabelAddBy` / `syncLabelRemovalBy`) must still be the direction that diverged, and must still be current (below).
3. **One live read** through `liveLabels`, which also applies the read to the Store (ADR 2350). A failed read writes nothing and is reported; the label stays marked.
4. **GitHub already matches the intent:** the listing lagged. Log `stale-read`, clear the mark, write nothing.
5. **Otherwise it is a lost write.** The intent is re-checked immediately before the write, because a release can race the read and a lock must not be put back after its worker exited. Then one re-apply through `addLabelChecked` / `removeLabelChecked` (echo reserved, cancelled on failure), logged as `lost-write`. The write stamps `EngineWriteAt`, so a lagging listing is dropped for the next 30 s and cannot cause ping-pong.

"Intent still current" uses in-memory state only, because the Store's `fabrik:editing` and `fabrik:reworking:*` labels are subject to the same loss and inferring from them would be circular:

| Engine write | Current while |
|---|---|
| add of the engine's own lock, `fabrik:editing`, `stage:<S>:in_progress` | the Store holds a worker for the item, or a rework is open |
| removal of `stage:<S>:complete` | a rework of `<S>` is open (`Engine.labels.reworks`, set in `beginStageRework`, cleared in `endStageRework`) |
| add of `fabrik:paused` / `fabrik:awaiting-input` | the event log shows no removal after the write (`FetchLabelRemovedAt`) |

Every other direction (a lock re-appearing after release, a `complete` add) is confirmed and logged but never re-applied. Another instance's lock label is never in scope. `fabrik:reworking:*`, `fabrik:awaiting-ci` and `fabrik:pause:<cause>` are out of scope: the cause label rides the pause pair.

The pause pair is the one case that needs a second read. A person who lifts a pause (ADR 1813) is indistinguishable from a lost write by the live label set alone; a removal event after the engine's write means a person acted, and nothing is re-applied. An unreadable event log is doubt, and doubt re-applies nothing.

### 3. The trust gate (R2)

`refuseIfLabelsUntrusted` runs in `runCatchUpPhase2` right after the autonomy gate, ahead of the Validate / non-Validate split. It is a no-op unless the stage's `stage:<S>:complete` label is marked diverged or a rework of `<S>` is open, so every other item behaves as before. Where it applies it fails closed: a rework of the stage is open, the label set cannot be re-read, GitHub disagrees with the engine's last write, or GitHub lacks a completion the cached item claims. A refusal logs one `[#N label-trust]` line, writes nothing, pauses nothing and records the short `merge-unsettled` admission cooldown; the decision is taken again when the two agree and the mark clears.

The other in-scope labels need no gate: the pause pair is already read live by the pause guard at each decision (ADR 2341), and no advance or landing decision reads the lock, `fabrik:editing` or `in_progress` labels from the cache.

### 4. Reporting (R3)

A failed read, a failed re-apply and a recurrence go to the warnings panel as type `label_divergence`, reconciled over the set of unresolved `(item, label)` subjects and resolved when a later repair succeeds or the live read agrees. Every report also writes a log line. Nothing is paused.

The panel was chosen over an `item-wedged` event because the wedge thresholds and lapse logic are keyed by gate name and would delay the signal, and because the panel also works for a daemon whose channel has no subscriber. The panel's "never a per-item state" rule (ADR 2135) is respected by modelling the report as a reconciled dynamic subject set, as `display_field:*` does, not as a state of the item.

### 5. The R4 finding: lost write or stale read?

**Not established. The code argues against a stale read for the slower events, but there is no captured evidence, and this ADR does not claim one.**

What was searched: `adrs/`, `specs/` and `docs/` in the repository, the issue and report threads as reproduced in the issue, and the worktree's own logs. None contains the 0.0.90 event lines. The daemon's log of the original run is outside the worktree boundary this work was done in, and was not read. A reproduction against GitHub was not attempted.

What the code says:

- The 233 s case is far past the 30 s grace. A board listing is one GraphQL query that reflects committed state, so a listing fetched after write + 30 s should not lag by minutes. This favours a write that never held, or one overwritten by something else.
- The shallow board query selects `labels(first: 15)`. An item with more than 15 labels can show a *present* label as absent, which explains "lock add not held" but cannot explain "removal not held".
- Competing explanations that the sim cannot rule out: a concurrent label replace by a person or another instance sharing the App (the lock label carries a host hash, so two instances do not share it, but they do write the same state labels), and a late webhook re-adding a removed label.

How the implementation is safe either way: step 4 never writes when the live read agrees with the intent, so a lagging listing is never fought (FR-009). Step 5 writes only on a positive live disagreement, once. The two outcomes are logged under distinct names, `stale-read` and `lost-write`, with the live result, the intent age and the recurrence state, so the first occurrences in production settle the question; a follow-up can read that log rather than speculate again.

Confidence in the finding "these were lost writes": low to moderate, from the code reasoning above only.

## Consequences

- A lock or state label the engine wrote and GitHub did not keep is restored while its intent is current, with one live read and one write, and a lagging listing is never fought.
- No advance trusts a completion label the engine withdrew for a rework. The gate is deliberately narrow and changes nothing for items with no mark and no open rework.
- A human removal of the pause pair, or a human edit of an in-scope label while the engine's intent was current, can still look like a lost write for the labels without an event-log check (lock, `fabrik:editing`, `in_progress`, `complete`). The bound is one re-apply per episode and a reported recurrence. This is the accepted residual risk.
- The repair is memory-only and hub-dependent. A restart starts with no intent and re-applies nothing, which is the safe direction.
- Out of scope, unchanged: the observed-at rule, the grace window, the detector and its window, board Status moves, human-applied labels, and pausing on divergence.

## Test seams

`SetLabelRepairDisabledForTest` (R1) and `SetLabelTrustGateDisabledForTest` (R2), per Engine, so each sim twin can fail independently. `DrainLabelRepairForTest` waits for the worker. `simgh.Sim.DropLabelWrite` is the "write succeeded but did not stick" fault (`tests/sim/simgh/FIDELITY.md`). Sim twins: `tests/sim/label_divergence_test.go`; unit tests: `engine/label_repair_test.go`.
