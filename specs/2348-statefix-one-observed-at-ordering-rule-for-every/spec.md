# Feature Specification: One observed-at ordering rule for every write into the item Store

**Feature Branch**: `fabrik/issue-2348`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description (verbatim):

```text
## Problem

The itemstate Store has no general ordering rule. Part of the state foundation from the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15). It must land before a second webhook source (#2143). See §2.

The only rule is the engine-write protection of #2159/#2178: `EngineWriteAt` plus a listing's `FetchedAt`, applied on the listing paths (`applyShallowItem`, `applyProbeItem`), for Status, labels and closed, and only for engine writes. Every other producer overwrites without comparing when its value was observed:

- **Webhook state vs older listings (§2.1):** a human move or label change applied by webhook is reverted by the next probe or reconcile listing that lags GitHub. `LightReconcile` reads the newer webhook value as drift and runs `Reconcile`, which applies the older listing (boardcache/boardcache.go:466, 1474).
- **Unordered webhooks (§2.2):** `IssueLabeled`/`IssueUnlabeled` and `ProjectV2ItemEdited` apply unconditionally (internal/itemstate/store.go:447-455, 1238); handlers run concurrently (engine/webhook.go:621). A late echo of an earlier engine write re-adds a removed label, which `protectLabels` then shields for 30 s.
- **Head SHA regression (§2.3):** eight writers set `HeadSHA` with no ordering; a regression wipes check runs and the CI verdict (store.go:1043-1070).
- **Partial payloads (§2.4):** `issues.opened` / `projects_v2_item.created` apply empty Status, ItemID, comments and `UpdatedAt` as if complete; `IssueOpened` unions labels.
- **Reconcile drops items (§2.5):** `Reconcile` removes cached items missing from the listing (boardcache.go:490-497) without `LightReconcile`'s `preFetchKeys` guard.
- **Deep fetch (§2.6):** `ItemDeepFetched` carries no fetch time.
- **Review lists (§2.8):** webhooks keep every review, deep fetch the latest per author.

## Requirements

- **R1:** Every mutation that writes a GitHub fact carries an observation time: a listing its `FetchedAt`, a deep fetch or live read the time its request was issued, a webhook the payload's GitHub time where one exists (`updated_at`, review `submitted_at`) and otherwise its receipt order, an engine write its write time.
- **R2:** The Store keeps, per item and per fact (Status, each label, closed, head SHA, linked-PR state, reviews, milestone, blockedBy), the time of its last applied observation, and drops a write whose observation is older, whoever made either one. This replaces the engine-write-only protection; the 30 s grace becomes the special case it is today.
- **R3:** A partial payload writes only the fields it carries.
- **R4:** `Reconcile` never removes an item added after its fetch started.
- **R5:** Review lists have one shape whichever producer filled them.
- **R6:** `docs/state-machine.md` D.4 states the rule once; ADR records the decision and supersedes the engine-write-only part of ADR 2159.

## Acceptance

- Store tests per fact: an older observation never overwrites a newer one, across every producer pair (listing, probe, deep fetch, webhook, engine write).
- Sim twins: a webhook move survives a lagging listing; a late label echo does not resurrect a removed label; a replayed `synchronize` does not regress the head.
- The #2159/#2178/#2180 regression tests pass unchanged.
- A live e2e case for a webhook-applied change surviving the next reconcile, since the sim cannot model listing lag.
```

## Background

The in-memory item Store is fed by several producers: board listings (the per-poll probe, shallow listings, `LightReconcile` and `Reconcile`), deep fetches and live reads, webhook deliveries, and the engine's own write-throughs. Only one of them has an ordering rule. Since #2159/#2178, an engine write is remembered with its time, and a listing fetched before that write (plus a 30 s grace) cannot overwrite it. This covers Status, labels and closed state, and only on the listing paths.

Everything else overwrites on arrival without asking when the value was observed. The engine state audit (`docs/design/engine-state-audit-2026-10.md` §2) traces these symptoms to that one cause:

- A human's move or label change that arrives by webhook is reverted by the next probe or reconcile listing, which lags GitHub. `LightReconcile` reads the newer webhook value as drift and triggers a full `Reconcile` that applies the older listing, so Status and labels flap and a dispatch can happen in the stale column.
- Webhook deliveries are handled concurrently and out of order. A late echo of an earlier engine write can re-add a label the engine removed. The existing protection then shields that wrong value for 30 s as if the engine had written it. `fabrik:editing` gates dispatch, so this matters.
- A lagging listing or a replayed `pull_request` event after a `synchronize` can set an old head SHA back. That wipes check runs and the CI verdict and mis-keys later `check_run` events.
- `issues.opened` and `projects_v2_item.created` payloads are applied as complete items, although they carry no Status, item ID, comments or update time. A late or duplicate delivery can wipe an existing item, and `issues.opened` can only add labels, never remove one.
- `Reconcile` removes cached items the listing has not caught up with, including items a webhook or a spawn added during the fetch, together with their counters, worker handle and engine-write records. `LightReconcile` already guards against this.
- A deep fetch or live read issued before a concurrent worker write or spawn can revert that write when it is applied.
- Review lists differ by producer. Webhooks keep every review, and a deep fetch keeps only the latest per author. A superseded `CHANGES_REQUESTED` may therefore be visible or invisible depending on which producer filled the list last.

This work must land before a second webhook source (#2143) is added, because every additional unordered producer multiplies these races. #2383 (webhook payloads that never decoded) has shipped. Real `projects_v2_item` deliveries now reach the Store, so the webhook ordering gap is live, not latent.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A webhook-applied change survives lagging listings (Priority: P1)

An operator moves an item to another column or adds or removes a label in the GitHub UI. The webhook applies the change within about a second. The next probe or reconcile listing was fetched from GitHub before the change became visible there and still shows the old value. The Store keeps the newer webhook value, no drift is declared, and nothing is dispatched on the old value.

**Why this priority**: This is the most visible failure. The Store reverts a human decision, and the engine may act on the reverted value.

**Independent Test**: Apply a webhook Status move and a webhook label change, then apply a listing and a probe whose fetch started before the webhook's GitHub time. Both facts keep the webhook values. A listing fetched after the webhook's time does apply.

**Acceptance Scenarios**:

1. **Given** an item whose Status was just changed by a webhook, **When** a listing fetched before that change is applied, **Then** the item keeps the webhook Status and no `StatusChanged` is emitted.
2. **Given** a webhook-applied label change, **When** `LightReconcile` compares the Store with an older listing, **Then** it does not count the difference as drift and does not start a `Reconcile`.
3. **Given** a newer listing than the webhook, **When** it is applied, **Then** the listing's value wins.

---

### User Story 2 - Out-of-order and replayed deliveries cannot resurrect old state (Priority: P1)

Webhook deliveries can arrive late, duplicated or concurrently. A late echo of an earlier engine write must not re-add a label the engine has since removed. A replayed `pull_request` event must not move the head SHA back. In both cases the older observation is dropped, whoever recorded the newer one.

**Why this priority**: These regressions are destructive. A wiped CI verdict or a wrongly shielded label gates or mis-dispatches work.

**Independent Test**: Engine-remove `fabrik:editing`, then deliver the `labeled` echo of the earlier add: the label stays absent. Apply `synchronize` to head B, then a replayed event for head A: the head stays B and its check runs and CI verdict are intact.

**Acceptance Scenarios**:

1. **Given** the engine removed a label at time T2, **When** a webhook `labeled` event whose GitHub time is T1 < T2 arrives, **Then** the label stays removed and the event is not treated as an engine write.
2. **Given** the head SHA was set from an observation at T2, **When** any producer applies a different head observed at T1 < T2, **Then** the head, check runs and CI verdict are unchanged.
3. **Given** two webhook deliveries for the same item handled concurrently, **When** they complete in either order, **Then** the resulting state equals the state from the newer observation.

---

### User Story 3 - Partial payloads, reconciles and fetches only write what they know (Priority: P2)

Payloads and fetches that carry only part of an item must not overwrite what they do not carry. A reconcile must not delete an item that appeared after its listing was fetched. A deep fetch or live read must not revert a write made while it was in flight. Review lists mean the same thing whichever producer filled them.

**Why this priority**: These are real but rarer than the P1 races, and each can be fixed and verified independently once the observed-at rule exists.

**Independent Test**: Apply `issues.opened`, `projects_v2_item.created` and a duplicate of each to an existing populated item: its Status, item ID, comments and update time are unchanged. Run `Reconcile` with a listing fetched before a webhook-added item: the item survives with its worker handle and counters. Apply a deep fetch issued before a worker write: the write survives. Fill a review list by webhook and by deep fetch with the same reviews: the lists are equal.

**Acceptance Scenarios**:

1. **Given** a populated item, **When** a late `issues.opened` arrives, **Then** only the fields the payload carries are written, and labels are replaced by the payload's set only if the payload is newer than the recorded label observations.
2. **Given** an item added after a `Reconcile` fetch started, **When** the `Reconcile` applies its listing, **Then** the item is not removed.
3. **Given** a deep fetch issued before a concurrent spawn's `blockedBy` edge write, **When** the fetch is applied, **Then** the edge is kept.
4. **Given** a superseded `CHANGES_REQUESTED` review, **When** reviews arrive by webhook or by deep fetch, **Then** both produce the same list shape and the same per-author outcome.

---

### Edge Cases

- **Equal observation times.** GitHub timestamps have one-second resolution, so a webhook and a listing can tie. An observation equal to the recorded one is not "older". It applies, and since values usually agree this is a no-op. The behaviour for a tie with differing values is stated once in the rule and pinned by a test.
- **Observations with no GitHub time.** A webhook without a usable payload time is ordered by receipt order. It never overrides an observation that carries a later GitHub time.
- **Clock sources differ.** Listings and engine writes use the daemon's clock, and webhooks use GitHub's. The rule must tolerate skew between them. The existing 30 s grace on engine-write versus listing comparisons is kept as the special case it is today, so the #2159/#2178/#2180 behaviour is unchanged.
- **A fact never observed.** The first observation of any fact always applies. Absence of a record is never read as "newer than everything".
- **Deletions and clears.** A label removal or Status clear is an observation with a time like any other. An older "present" cannot undo a newer "removed".
- **Items removed in GitHub.** A `Reconcile` still removes an item that was present before its fetch started and is genuinely missing from the listing.
- **Per-label granularity.** A label observation affects only that label. Ordering a change to `fabrik:paused` does not block an unrelated label change.
- **Restart.** Observation times are memory-only like the rest of the Store. After a restart the first listing re-baselines every fact.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every Store mutation that writes a GitHub-derived fact carries an observation time, chosen by producer: a listing or probe uses the time its fetch started, a deep fetch or live read uses the time its request was issued, a webhook uses the payload's GitHub time where one exists (for example `updated_at`, review `submitted_at`) and otherwise its receipt order, and an engine write uses its write time.
- **FR-002**: The Store records, per item and per fact, the observation time of the last applied write. The facts are Status, each individual label, closed state, head SHA, linked-PR state, reviews, milestone and blockedBy.
- **FR-003**: The Store drops any write to a fact whose observation time is older than the recorded one, regardless of which producer made the recorded observation and which made the new one. Every producer pair (listing, probe, deep fetch, webhook, engine write) is covered.
- **FR-004**: This single rule replaces the engine-write-only protection (`EngineWriteAt` with a listing's `FetchedAt`). The existing 30 s engine-write grace remains as the special case of the rule for engine writes versus listings. The #2159, #2178 and #2180 regression tests pass unchanged.
- **FR-005**: A dropped write is silent in effect: it emits no change event, does not wake the poll, does not alter any recorded observation time, and is not attributed to any actor.
- **FR-006**: `LightReconcile` does not count as drift a difference that is explained by a newer recorded observation than its listing's fetch time. A webhook-applied change newer than the listing therefore does not mark the stream unhealthy or trigger `Reconcile`.
- **FR-007**: Webhook label events, `projects_v2_item` edits, head-SHA updates and review events are ordered by FR-003. A late echo of an earlier engine write cannot re-add a removed label, and the label protection no longer shields a value the engine did not write.
- **FR-008**: A replayed or lagging head-SHA observation older than the recorded one cannot change the head SHA, and so cannot wipe check runs or the CI verdict or re-key the SHA lookup.
- **FR-009**: A partial payload (`issues.opened`, `projects_v2_item.created`, and any other payload that does not carry every field) writes only the fields it carries. It never writes empty Status, item ID, comments or update time over known values, and an `opened` payload's label set is ordered against recorded label observations rather than unioned.
- **FR-010**: `Reconcile` never removes an item that was added after its fetch started, with the same protection `LightReconcile` has. Items genuinely absent from a listing fetched after they were added are still removed.
- **FR-011**: A deep fetch or live read carries the time its request was issued, and applying it cannot revert a fact written or observed more recently.
- **FR-012**: Review lists have one shape whichever producer filled them. Webhook and deep-fetch producers yield the same per-author result for the same underlying reviews, so a superseded review is either consistently visible or consistently absent.
- **FR-013**: `docs/state-machine.md` D.4 states the ordering rule once, covering the producers, the observation-time rules of FR-001, the tie behaviour and the retained engine-write grace, and `docs/llms-full.txt` is regenerated if a canonical doc changes.
- **FR-014**: An ADR numbered `adrs/2348-…` records the decision and states that it supersedes the engine-write-only part of ADR 2159. The Architecture notes in `CLAUDE.md` that describe the engine-write protection are updated to match.
- **FR-015**: Tests cover the rule per fact: for each fact, an older observation never overwrites a newer one across every producer pair, including the equal-time case.
- **FR-016**: Sim-bed twin scenarios cover: a webhook move surviving a lagging listing; a late label echo not resurrecting a removed label; a replayed `synchronize` not regressing the head. Each has a neutralisation seam so a non-vacuity check can show the scenario fails without the rule.
- **FR-017**: A live end-to-end case shows a webhook-applied change surviving the next reconcile, and the live suite's registry and sim-twin mapping are updated for it.

### Key Entities

- **Observation**: A fact value together with the time it was observed and the producer that observed it.
- **Fact**: The unit the ordering rule applies to: Status, a single label, closed state, head SHA, linked-PR state, reviews, milestone or blockedBy edges.
- **Producer**: A source of writes into the Store: listing, probe, deep fetch or live read, webhook, or engine write.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every fact and every ordered pair of producers, a test shows that applying the older observation after the newer one leaves the Store unchanged, and the reverse order applies both.
- **SC-002**: A human's column move or label change delivered by webhook is never reverted by a listing or probe fetched before it, in the sim scenario and in the live end-to-end case.
- **SC-003**: Replaying an earlier head SHA after a newer one leaves the head, check runs and CI verdict unchanged in 100% of tested sequences.
- **SC-004**: Delivering a late echo of an engine-written label add after the engine removed it never leaves the label present in tested sequences.
- **SC-005**: A `Reconcile` with a listing older than an item's creation never removes that item, with its worker handle and counters intact.
- **SC-006**: The existing #2159, #2178 and #2180 regression tests pass without modification.
- **SC-007**: Each of the three sim twin scenarios fails when the ordering rule is neutralised and passes with it.

## Assumptions

- The rule is a per-fact "newest observation wins" comparison. Where an observation's time equals the recorded one, the write is applied, since values normally agree and the outcome is then a no-op.
- Producers on different clocks (daemon versus GitHub) are compared as they are today for engine writes versus listings. The 30 s grace absorbs skew, and this issue does not introduce a clock-synchronisation mechanism.
- A webhook with no GitHub time in its payload uses its receipt order and is never allowed to override an observation with a later GitHub time.
- The observation records are memory-only like the rest of the Store, and no restart persistence is added.
- The freshness baseline of the cache (`SelfWriteObserved`, audit §2.7) is a separate concern with its own issue. This work does not change what the baseline means.
- Real `projects_v2_item` webhook deliveries now decode, following #2383, so the live end-to-end case can rely on them.
- The "second webhook source" (#2143) is not part of this issue. The rule is written so such a source needs only to supply observation times.

## Out of Scope *(optional)*

- Adding a second webhook source (#2143).
- The freshness-baseline clock problem of audit §2.7, and any other audit item that is not in §2.1–§2.6 or §2.8.
- Persisting observation times across restarts.
- Changing what the board query, probe or webhook handlers fetch, beyond carrying observation times.
- Changing dispatch, gate or landing decisions. Only the values the Store holds change.

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md` §2 (items 2.1–2.6, 2.8)
- ADR 2159 (engine-write protection against stale listings), ADR 2178 (probe apply), ADR 2180 (cache freshness)
- `docs/state-machine.md` D.4 (Reconciliation)
- #2143 (second webhook source), #2383 (`projects_v2_item` webhook decoding)
