# Feature Specification: Optional reserved worker slot for review, CI-fix and rebase rounds

**Feature Branch**: `fabrik/issue-2490`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

Review, CI-fix and rebase reinvokes take a slot from the same `max_concurrent` semaphore as stage runs (`dispatchReinvoke`, `engine/reinvoke.go`). With every slot held by long Implement runs (median 67 min in the report), a short fix round on a PR that is one push from landing waits behind them. The report logs slots fully busy 13.8 h in 4.5 days, ~27 h of logged slot waits on 14 issues, and a fix round that waited 12 min for a slot (on one PR the review-wait clock then expired during the wait).

## Requirements

1. An optional reserved slot for fix rounds, on top of `max_concurrent`, used only by `review-reinvoke`, `ci-fix-reinvoke` and `rebase-reinvoke` — the same shape as the merge train's reserved slot (#2115, ADR 2115): try the reserved slot, then a free ordinary slot, else wait on either.
2. A setting (flag + env + YAML) for the reserved count: `0` disables (today's behaviour). Research recommends the default from our own fleet logs and the report's data; record the decision in the ADR.
3. Stage runs and comment-review passes never use it.
4. `fabrik_health.workers` and the TUI report reserved use separately (as `train_in_use`/`train_max` do).

## Acceptance

- Unit test: all ordinary slots busy → a fix round starts at once on the reserved slot; a stage run does not.
- Sim twin; neutralising the reservation makes it wait.
- `CLAUDE.md`/`docs/state-machine.md` worker-slot text updated; `docs/llms-full.txt` regenerated.

## Notes

- Source: community report #2453 (totalslacker's flow analysis of `totalslacker/chronicle`, Oct 1–26: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874; data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec).
- Mention #2453 in the PR body by bare number only. Do **not** use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it — the report is closed by hand when a release ships. The PR closes this work issue only."

## Background

Review, CI-fix and rebase rounds (the "fix rounds") share one pool of worker slots, sized by `max_concurrent`, with long-running stage runs. When Implement runs hold every slot, a fix round, which is short and often the last step before a PR can land, waits behind them. The community flow analysis (#2453, `totalslacker/chronicle`, Oct 1–26) logged slots fully busy for 13.8 h in 4.5 days, about 27 h of logged slot waits across 14 issues, and one fix round that waited 12 minutes, during which the PR's review-wait clock expired.

The merge train already solved the same shape of problem with its own reserved slot (#2115, ADR 2115): a slot on top of `max_concurrent` that only train work may use, tried first, then a free ordinary slot, otherwise wait on either. This issue gives fix rounds an equivalent, optional reservation.

Research input already on the issue (the maintainers' analysis of the #2453 data and of our own fleet's engine logs):

- Fix-round waits are small in absolute terms: about 3.6 h over 24 occurrences, split evenly between Review and Validate (1.8 h each). Stage runs waiting for a slot account for 26.9 h in the same data (Specify 15.3 h), and a further 33.2 h of mostly-slot waits before Oct 22 is estimated from unlogged idle time (Specify 21.9 h). Most measured slot starvation is therefore new stage work waiting to start, which a fix-round reservation does not touch.
- Fix-round waits are logged only from Oct 22 (about 4.5 of 25 days). Scaled up generously, the total is under about 20 h across the month, spread over about 200 issues.
- The costly case, the review-wait timeout firing during a slot wait (chronicle 12-minute wait), is a separate clock problem that #2467 addresses. Once that ships, the reservation buys minutes, not avoided pauses.
- In our own fleet only one of five instances (`max_concurrent` 5) ever filled its slots, with all busy 8.9 h of 329 h (3%). Of its 62 fix rounds, 18 started during or just after a full-slot window, and their queueing is at most 2.9 h in total (12 under 4 min, 6 at 21–27 min). This is an upper bound: the engine logs nothing when a fix round takes its slot, so the real wait cannot be measured today.
- Headroom is large (406 worker-hours over 25 days is about 16 per day, against 48–72 slot-hours per day at `max_concurrent` 2–3, so 23–34% used), so more slots is the other remedy.
- The recommendation recorded there: ship the setting with default `0` (today's behaviour), and change the default only if fleet logs show fix rounds queueing behind full slots after #2467 has shipped. The reporting in FR-006, FR-007 and FR-008 is what makes that observable.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A fix round starts immediately when stage runs hold every slot (Priority: P1)

An operator has configured one reserved fix-round slot. Every ordinary worker slot is occupied by Implement runs. A reviewer submits feedback on a PR that is one push from landing, and the engine dispatches a review round for it.

**Why this priority**: This is the whole point of the feature: the short, near-landing round no longer queues behind long stage runs.

**Independent Test**: With ordinary slots all busy and one reserved slot configured, trigger a review, CI-fix or rebase round and verify it starts at once. Trigger a stage run in the same state and verify it still waits.

**Acceptance Scenarios**:

1. **Given** a reserved count of 1 and all ordinary slots busy, **When** a review, CI-fix or rebase round is dispatched, **Then** it starts immediately on the reserved slot.
2. **Given** the same state, **When** a stage run or a comment-review pass is dispatched, **Then** it does not use the reserved slot and waits for an ordinary slot as today.
3. **Given** the reserved slot is already in use by a fix round and all ordinary slots are busy, **When** a second fix round is dispatched, **Then** it waits until either the reserved slot or an ordinary slot frees up.

---

### User Story 2 - The reservation is off by default and leaves today's behaviour unchanged (Priority: P1)

An operator who does not set the new option sees no change. Fix rounds and stage runs compete for the `max_concurrent` slots exactly as before.

**Why this priority**: The maintainers' recommendation is to ship the setting off; existing deployments must not change behaviour or capacity.

**Independent Test**: Run with the option unset (and with `0`) and confirm that total concurrency never exceeds `max_concurrent` and that fix rounds wait behind full ordinary slots.

**Acceptance Scenarios**:

1. **Given** the option is unset, **When** all ordinary slots are busy and a fix round is dispatched, **Then** it waits for an ordinary slot, as today.
2. **Given** the option is `0`, **When** the engine starts, **Then** no reserved capacity exists and health and the TUI report none.

---

### User Story 3 - The operator can configure the reserved count through the usual settings (Priority: P2)

An operator sets the number of reserved fix-round slots through a command-line flag, an environment variable or the YAML config, with the usual precedence.

**Why this priority**: Without a setting the feature cannot be used; it ranks below the behaviour itself.

**Independent Test**: Set the value through each of the three routes and confirm the engine reports that many reserved slots; set an invalid value and confirm startup refuses it.

**Acceptance Scenarios**:

1. **Given** the value is set through the flag, the environment variable or YAML, **When** the engine starts, **Then** the configured number of reserved slots exists, with flag taking precedence over environment and environment over YAML.
2. **Given** a negative or non-integer value, **When** the engine starts, **Then** startup is refused with a message naming the setting.

---

### User Story 4 - Reserved use is visible and the queue time is measurable (Priority: P2)

An operator or maintainer deciding whether to turn the reservation on, or to change its default, can see how much reserved capacity exists and is in use, and how long fix rounds queued for a slot.

**Why this priority**: The decision on the default depends on this data, which today cannot be measured.

**Independent Test**: With a reserved slot in use, read the health data and the TUI and confirm reserved use is reported apart from ordinary use; dispatch a fix round that has to wait and confirm one log line states how long it waited.

**Acceptance Scenarios**:

1. **Given** a reserved fix-round slot is in use, **When** the operator reads `fabrik_health.workers` or the TUI worker-slot display, **Then** reserved in-use and reserved max are shown separately from ordinary use and from the merge train's slot.
2. **Given** a fix round waited for a slot, **When** it takes its slot (ordinary or reserved), **Then** the engine logs one line giving the item, the round kind, which slot it took and the time between dispatch and taking the slot.

---

### Edge Cases

- A stage run or a comment-review pass must never take the reserved slot, even when it is the only free capacity and the item has waited a long time.
- Merge-train conflict resolution keeps using its own reserved slot and is unaffected; the two reservations do not share capacity.
- A fix round that is cancelled (item stopped, shutdown, SIGHUP drain) while waiting for a slot releases nothing it does not hold and refunds its cycle exactly as it does today.
- Fix rounds refused under the Claude usage-limit hold or the forge-unreachable hold are refused before any slot is taken, as today.
- With a reserved count above 1, up to that many fix rounds may run beyond `max_concurrent` at once.
- When both a free ordinary slot and a free reserved slot exist, the round uses the reserved slot first (the train's order), keeping ordinary slots available for stage work.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The engine supports an optional reserved capacity for fix rounds, in addition to and separate from `max_concurrent`, used only by review-reinvoke, ci-fix-reinvoke and rebase-reinvoke rounds.
- **FR-002**: A fix round acquires a worker slot by trying a free reserved slot first, then a free ordinary slot, and otherwise waits until either becomes free.
- **FR-003**: Stage runs and comment-review passes never use the reserved capacity.
- **FR-004**: The reserved count is configurable by command-line flag, environment variable and YAML key, with flag over environment over YAML precedence; the value `0` disables the feature, and `0` is the default.
- **FR-005**: A negative or non-integer reserved count is refused at startup with an error naming the setting.
- **FR-006**: `fabrik_health.workers` reports reserved fix-round slot use and the configured maximum as separate fields, distinct from ordinary `in_use`/`max_concurrent` and from the merge train's `train_in_use`/`train_max`.
- **FR-007**: The TUI worker-slot display reports reserved fix-round use separately, in the same way it reports the merge train's slot.
- **FR-008**: When a fix round takes its slot, the engine logs one line stating the item, the round kind, which kind of slot was taken (reserved or ordinary) and the elapsed time from the round's dispatch to the moment it holds the slot.
- **FR-009**: With the reserved count at `0`, the engine's behaviour is identical to the behaviour before this change, including total concurrency never exceeding `max_concurrent` for item work.
- **FR-010**: The merge train's reserved slot and its acquisition behaviour are unchanged.
- **FR-011**: The decision to ship the default at `0`, with the supporting fleet-log and report data, is recorded in the ADR for this issue.
- **FR-012**: The documentation that describes worker slots (`CLAUDE.md`, `docs/state-machine.md`, and the user guide wherever the new setting belongs) is updated, and the documentation bundle `docs/llms-full.txt` is regenerated in the same change.

### Key Entities

- **Fix round**: A review-reinvoke, ci-fix-reinvoke or rebase-reinvoke worker run, started by the engine to address review feedback, a CI failure or a needed rebase on an item's PR.
- **Ordinary slot**: One of the `max_concurrent` worker slots shared by stage runs, comment-review passes and fix rounds.
- **Reserved fix-round slot**: Extra capacity, on top of `max_concurrent`, usable only by fix rounds.
- **Merge-train reserved slot**: The existing one-slot reservation for merge-train work (#2115); a separate pool.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a reserved count of at least 1 and every ordinary slot busy, a fix round starts within one poll cycle without waiting for any ordinary slot to free, and a stage run dispatched in the same state does not start.
- **SC-002**: With the reserved count at `0` or unset, a recorded run is indistinguishable from one before the change: same slot capacity, same waiting behaviour for fix rounds.
- **SC-003**: An operator can read, from health data or the TUI alone, how many reserved fix-round slots exist and how many are in use, separately from ordinary and merge-train slots.
- **SC-004**: For any fix round that took a slot, the engine log states how long it queued, so that the share of fix rounds that waited behind full slots can be counted from logs.
- **SC-005**: Neutralising the reservation in the simulated test environment makes the same scenario show the fix round waiting, proving the test depends on the reservation.

## Assumptions

- The default is `0` (disabled), following the maintainers' recommendation on this issue. It may be revisited later, as a separate change, using the data FR-006 to FR-008 make available and only once #2467 has shipped.
- The setting is a plain non-negative count; no upper bound is enforced beyond what the operator chooses.
- "Used only by" means the three fix-round kinds named in the issue; comment-review passes, including those on review feedback, are not fix rounds for this purpose.
- The reservation is a separate pool from the merge train's reserved slot; neither borrows from the other.
- The queue-time log line (FR-008) comes from the maintainers' own request in the issue discussion and is in scope because it is what lets the default be judged later.
- Naming of the flag, environment variable, YAML key and health fields follows the conventions of the existing train-slot setting and fields; exact names are a planning decision.

## Out of Scope *(optional)*

- Changing the review-wait timeout clock so it does not run during a slot wait (#2467).
- Raising the default for `max_concurrent`, or adding slots for stage runs.
- Reserving or prioritising slots for new stage work such as Specify, which the data shows is the main source of waiting.
- Reordering or prioritising fix rounds among themselves beyond the existing dispatch order.
- Changing the default from `0` to a non-zero value.

## Source References *(optional)*

- Community report #2453 and its flow analysis: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874
- Report data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec
- Merge train reserved slot: #2115, ADR 2115
