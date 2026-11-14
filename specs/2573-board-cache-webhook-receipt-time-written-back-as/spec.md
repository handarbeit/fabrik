# Feature Specification: Stop webhook receipt times from causing self-inflicted cache drift

**Feature Branch**: `fabrik/issue-2573`
**Created**: 2026-11-14
**Status**: Specified
**Input**: User description: "## Problem

An item waiting on CI with a busy linked PR causes a `cache-divergence` (light-reconcile drift) report on almost every reconcile tick. The engine causes the drift itself. On one production daemon, 713 of 714 drifts over about 39 hours came from a single item that was waiting on CI (`fabrik:awaiting-ci`) while its PR's Actions suite stayed queued.

The loop:

1. `CacheImpl.FetchProjectBoard` (`boardcache/boardcache.go`, the `localDeltaAtCopy` override) raises each served item's `UpdatedAt` to its latest webhook receipt time (`localDeltaAt`). The receipt time is a little later than GitHub's own `updatedAt`. Every check event on the linked PR refreshes it.
2. The awaiting-CI settle scan (`settleAwaitingCIScan`) applies that served board copy back into the Store through the deep-fetch apply, which moves `UpdatedAt` forward only (`internal/itemstate/project_apply.go`, \"UpdatedAt is a listing fact\"). The Store now holds the receipt time.
3. `LightReconcile` compares the Store with GitHub's listing, which still holds GitHub's older value, and reports drift. It writes the value back. The next poll pushes it forward again.

Steps 1 and 3 are confirmed in the code and in daemon logs, where the board `updatedAt` matches a webhook receipt time and no real change on the issue. Step 2 is the scan's documented behaviour but has not been traced line by line.

## Impact

- The transport reports itself unhealthy on each drift, and each reconcile does a needless cache rewrite.
- The real cost: a genuine missed webhook produces exactly this signal, so the noise hides real drift.
- No lost events or wrong decisions have been seen.

## Requirements

1. A value the cache derived from a webhook receipt time is never written back into the Store as if GitHub had reported it. Either the board served to engine code carries GitHub's `UpdatedAt` (with the receipt time used only where staleness decisions need it), or the deep-fetch apply ignores an `UpdatedAt` that came from the override.
2. Light-reconcile drift for an item with no real GitHub change and continuous check events on its PR does not recur.
3. Real drift (a field GitHub changed that the cache missed) is still reported.

## Acceptance

- A sim twin: an item at `fabrik:awaiting-ci` whose PR receives a check event before every poll shows no drift across N reconcile ticks. With the fix neutralised by a seam, it shows drift on each tick (non-vacuity).
- The existing #2180 freshness tests still pass (a webhook stamp still counts as an issue-layer change for cache freshness).
- `docs/state-machine.md` D.4 updated if the served `UpdatedAt` changes meaning."

## Background

An item waiting on CI whose linked PR keeps receiving check events produces a `cache-divergence` (light-reconcile drift) report on almost every reconcile tick, although nothing on GitHub changed. The engine causes this drift itself. On one production daemon, 713 of 714 drifts over about 39 hours came from a single item at `fabrik:awaiting-ci` whose PR's Actions suite stayed queued.

The loop, as understood today:

1. When the cache serves the board, it raises each item's "last updated" time to the latest time a webhook for that item was *received*. That receipt time is slightly later than GitHub's own value, and every check event on the linked PR refreshes it.
2. The awaiting-CI settle scan applies that served copy back into the engine's item store, which only moves the "last updated" time forward. The store now holds the receipt time as if GitHub had reported it.
3. The next light reconcile compares the store with GitHub's listing, which still holds GitHub's older value, reports drift and writes the value back. The next poll pushes it forward again.

The first and last steps are confirmed in code and daemon logs (board "last updated" equals a webhook receipt time with no real change on the issue). The middle step is the scan's documented behaviour but has not been traced line by line; the work should confirm it first.

**Impact.** Each drift marks the webhook transport unhealthy and triggers a needless cache rewrite. The real cost is signal loss: a genuine missed webhook produces exactly the same report, so this noise hides real drift. No lost events or wrong decisions have been observed.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - No self-inflicted drift for an item waiting on CI (Priority: P1)

An operator runs a daemon with webhooks enabled. An item sits at `fabrik:awaiting-ci` while its PR receives a steady stream of check events. Nothing on GitHub changes for the issue itself.

**Why this priority**: This is the reported defect and the source of nearly all observed false drift.

**Independent Test**: In the deterministic sim bed, seed an item at `fabrik:awaiting-ci` and deliver a check event on its PR before every poll across many reconcile ticks; assert that no cache-divergence is reported.

**Acceptance Scenarios**:

1. **Given** an item at `fabrik:awaiting-ci` with no real change on GitHub, **When** a check event for its PR arrives before each poll and reconcile ticks run repeatedly, **Then** no light-reconcile drift is reported for the item on any tick.
2. **Given** the same setup with the fix neutralised by a test seam, **When** the same ticks run, **Then** drift is reported on each tick (the test is not vacuous).

---

### User Story 2 - Real drift is still detected (Priority: P1)

An operator relies on drift reports to notice a genuinely missed webhook.

**Why this priority**: Removing the noise must not remove the signal; otherwise the fix defeats its own purpose.

**Independent Test**: In the sim bed, change a field on GitHub (label, status, or similar) without delivering the corresponding webhook, run a reconcile tick, and assert drift is reported.

**Acceptance Scenarios**:

1. **Given** a field GitHub changed that the cache missed, **When** a light reconcile runs, **Then** the drift is reported as before.
2. **Given** an item that also has continuous check events on its PR, **When** a real missed change occurs, **Then** it is still reported (the check-event stream does not mask it).

---

### User Story 3 - Cache freshness decisions are unchanged (Priority: P2)

A webhook event for an item still counts as a change to that item when the cache decides whether its cached copy is fresh.

**Why this priority**: The receipt time exists for a reason (staleness decisions, ADR 2180); the fix must narrow where it is used, not remove it.

**Independent Test**: The existing cache-freshness tests continue to pass unmodified in meaning.

**Acceptance Scenarios**:

1. **Given** an item that received a webhook after its cached copy was taken, **When** the cache decides freshness, **Then** it still treats the cached copy as stale as before.

---

### Edge Cases

- An item with no webhook ever received: behaviour unchanged.
- Poll-only daemons (no webhooks, no receipt times): behaviour unchanged.
- A genuine GitHub-reported "last updated" later than any receipt time: the GitHub value is still honoured.
- Items other than `fabrik:awaiting-ci` that are applied through the same deep-fetch path (any apply site that receives a served board copy) must not reintroduce the loop.
- Repeated reconcile ticks must not oscillate between two values for the same item.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A value the cache derived from a webhook receipt time is never written into the engine's item store as if GitHub had reported it.
- **FR-002**: For an item with no real GitHub change and continuous check events on its linked PR, light-reconcile drift does not recur on successive reconcile ticks.
- **FR-003**: Real drift (a field GitHub changed that the cache missed) is still reported by light reconcile, including for an item that also receives continuous check events.
- **FR-004**: A webhook event for an item still counts as an issue-layer change when the cache decides whether a cached copy is fresh (the ADR 2180 freshness behaviour is unchanged).
- **FR-005**: A test seam can neutralise the fix so a sim scenario can show the drift returns without it (non-vacuity).
- **FR-006**: A sim twin covers the scenario in User Story 1 (an item at `fabrik:awaiting-ci`, a check event on its PR before every poll, N reconcile ticks, no drift) and its neutralised counterpart (drift on each tick).
- **FR-007**: The as-built documentation of the cache freshness rule (`docs/state-machine.md` D.4) is updated if the meaning of the "last updated" value served to engine code changes, and the generated documentation bundle is regenerated in the same change.

### Key Entities *(if applicable)*

- **Receipt time**: the moment the daemon received the latest webhook for an item; a local fact, always slightly later than GitHub's own time.
- **GitHub "last updated"**: GitHub's own timestamp for the item; the only value that light reconcile can compare against GitHub's listing.
- **Drift report (`cache-divergence`)**: the signal that the cache and GitHub's listing disagree, used to surface possibly missed webhooks.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim scenario (item at `fabrik:awaiting-ci`, a check event before every poll, N reconcile ticks), zero drift reports occur across all N ticks.
- **SC-002**: With the fix neutralised, the same scenario reports drift on every tick.
- **SC-003**: A sim scenario with a real missed GitHub change reports drift, with and without a continuous check-event stream.
- **SC-004**: All existing cache-freshness (#2180) tests pass.
- **SC-005**: On a production-like daemon, an item waiting on CI with a busy PR no longer accounts for a steady stream of drift reports; reported drift reflects only real changes.

## Assumptions

- Either of the two approaches in the original issue satisfies the requirements (serving engine code GitHub's own value and using the receipt time only for staleness decisions, or ignoring the receipt-derived value when applying a served copy); choosing between them is a Plan-stage decision.
- The middle step of the loop (the settle scan writing the receipt time back) will be traced and confirmed during Research; if it turns out to be incorrect, the requirement still stands but the mechanism to fix changes.
- Other apply sites that consume the served board copy are in scope only to the extent they could reintroduce the same loop.
- No lost events or wrong decisions result from the current behaviour, so no data repair or migration is needed.

## Out of Scope *(optional)*

- Changing how real drift is detected or how often reconciles run.
- Changing the webhook transport health model beyond removing this false trigger.
- Redesigning the receipt-time mechanism for staleness decisions.

## Source References *(optional)*

- `boardcache/boardcache.go`: the served-board override that raises "last updated" to the receipt time (`localDeltaAtCopy`), and `LightReconcile`.
- `engine/ci_settle.go`: the awaiting-CI settle scan.
- `internal/itemstate/project_apply.go`: the deep-fetch apply, which treats "last updated" as a listing fact and moves it forward only.
- ADR 2180 and `docs/state-machine.md` D.4: the cache freshness rule.
