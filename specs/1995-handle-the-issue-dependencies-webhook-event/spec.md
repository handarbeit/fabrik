# Feature Specification: Handle the `issue_dependencies` webhook event

**Feature Branch**: `fabrik/issue-1995`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "Handle the issue_dependencies webhook event — dependency changes currently wait for the next poll"

<details>
<summary>Original issue body (verbatim)</summary>

## Problem

GitHub delivers an `issue_dependencies` webhook event when an issue's "blocked by" / "blocking" relationship changes. Fabrik ignores it. `boardcache/delta.go`'s event dispatcher has cases for `issue_comment`, `issues`, `pull_request`, `pull_request_review`, `pull_request_review_comment`, `check_run`, `check_suite` and `projects_v2_item`, and **no `default` branch**, so an `issue_dependencies` delivery is dropped silently.

As a result, a dependency change reaches the engine only on a later poll's deep fetch:

- **Unblocking.** When the blocking issue is closed, `issues.closed` arrives for *that* issue, but nothing updates the **blocked** item's cached `BlockedBy`. An item labelled `fabrik:blocked` resumes only once a poll re-reads it. `checkDependencies` re-reads `BlockedBy` live on resume (ADR-1419), but only once the item is evaluated at all.
- **Edge added or removed by a human.** Adding or removing a "blocked by" link in GitHub's UI changes nothing in Fabrik until a poll. That means up to a full `poll` interval: 180s on this repo's dev instance, or longer on a quiet board where idle backoff applies.

This matters more under App auth with `event_source: hookdeck`. The App can subscribe to `issue_dependencies` (the dev App `fabrik-dev-daemon` now does), so the event arrives but goes unused.

## Requirements

- **R1 — Handle `issue_dependencies` deliveries.** Add a delta case in `boardcache/delta.go`. For each issue named by the payload (the blocked issue, and the blocking issue where present), either apply the change to the cached `BlockedBy` or invalidate that item's deep cache so the next evaluation re-reads it. Invalidation is acceptable and simpler, provided it is precise to the affected items. **Verify the payload shape and action names against GitHub's webhook documentation and a captured real delivery.** Do not assume them.
- **R2 — Wake the engine.** A delivery that changes dependency state for a board item triggers the same wake path other event-driven deltas use. A `fabrik:blocked` item whose last blocker was removed is then re-evaluated promptly instead of waiting for the poll.
- **R3 — Never unblock on the event alone.** The event is a trigger. The decision to remove `fabrik:blocked` still goes through `checkDependencies`'s live `BlockedBy` read (ADR-1419). A stale or partial payload must never unblock an item.
- **R4 — PAT-mode parity, where available.** If `--webhooks` (gh forwarding) can subscribe to `issue_dependencies` for a repo, include it in the forwarded event list. If it cannot, document that dependency changes stay poll-driven in PAT mode.
- **R5 — Docs.** Update the event lists in `docs/USER_GUIDE.md` (the Hookdeck section, plus the App "Subscribe to events" guidance in that section and in `cmd/pruefer/README.md`'s equivalent if it applies) and `docs/state-machine.md`. Regenerate `docs/llms-full.txt`.

## Acceptance

- A unit test applies a recorded `issue_dependencies` payload to `boardcache` and shows the affected item's dependency data refreshed or invalidated, with a wake signalled. An unrelated item is untouched.
- A sim scenario covers it: an item is `fabrik:blocked` on one blocker, the blocker edge is removed, and the item is unblocked on the next wake rather than the next poll. A second case delivers a stale event while the live read still shows a blocker; the item stays blocked.
- The test fixture is a captured real delivery, recorded under `github/testdata/` alongside the existing wire-contract fixtures.

</details>

## Background

GitHub sends an `issue_dependencies` webhook when an issue's "blocked by" / "blocking" relationship changes. Fabrik's webhook intake does not recognise it and drops it silently, so a dependency change reaches the engine only when a later poll deep-fetches the affected item: up to a full poll interval (180 s on the dev instance, longer when idle backoff applies). This was observed live on 2026-10-15: "blocked by" edges were added to two in-flight issues, no webhook was acted on, the cached dependency list stayed empty, and one issue dispatched Plan and Implement while it should have been blocked. The workaround was to label it `fabrik:blocked` by hand, which forces the live re-read described in ADR-1419.

The event matters most under App auth with `event_source: hookdeck`, where the App can already subscribe to it and the delivery arrives but goes unused.

Facts established from real deliveries (captured 2026-10-19 on PR #2401, `boardcache/testdata/webhooks/issue_dependencies_*.json`; if that PR has not merged, the files are on branch `fixtures/webhook-captures-2352`):

- **One dependency change produces two deliveries**: `blocked_by_added` / `blocked_by_removed` and `blocking_added` / `blocking_removed`. Each carries both issue objects plus `blocked_issue_id` and `blocking_issue_id`.
- **The repository object differs by side.** `blocked_by_*` deliveries carry `blocking_issue_repo`; `blocking_*` deliveries carry `blocked_issue_repo`. The issue objects' own `repository_url` / `html_url` identify each issue's repository on both sides.
- **Embedded counts are unreliable.** `issue_dependencies_summary` (and other embedded objects) reflect the issue when GitHub rendered the delivery, which may be before or after the action. They must not be used to derive the new blocker count.
- **Closing a blocking issue sends no `issue_dependencies` delivery.** Only `issues` `closed` arrives, for the blocker. The "unblocking" case in the original problem statement is therefore not covered by this event. Note that a push-based unblock on blocker close already exists for items whose dependency list is cached (`PushUnblockObserver`, `docs/state-machine.md` §2.5); the remaining gap is items whose cached dependency data is absent or stale at that moment.

Related: #2351 (transport-neutral webhook intake, so the event applies through the ordered path) is a prerequisite; #2352 covers other missing subscriptions; ADR-1419 defines the live re-read that remains the sole authority for unblocking.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A dependency edge added or removed by a human takes effect promptly (Priority: P1)

An operator adds or removes a "blocked by" link in GitHub's UI. The daemon notices from the webhook, refreshes what it knows about the affected items, and re-evaluates them on its next wake rather than waiting for the next poll.

**Why this priority**: This is the failure that was seen live (an item dispatched work while a blocker edge existed) and the core purpose of the issue.

**Independent Test**: Deliver a captured `blocked_by_added` payload for an in-flight item whose cached dependency data is empty and confirm the item's dependency data is marked for re-read and the engine is woken; the next evaluation sees the new blocker.

**Acceptance Scenarios**:

1. **Given** an item with no cached blockers, **When** a `blocked_by_added` delivery names it as the blocked issue, **Then** its cached dependency data is invalidated and a wake is signalled, so the next evaluation re-reads the dependencies from GitHub.
2. **Given** an item labelled `fabrik:blocked` on a single blocker, **When** a `blocked_by_removed` delivery arrives for that edge and the live read shows no remaining open blocker, **Then** the item is unblocked on the next wake, not the next poll.
3. **Given** an unrelated board item, **When** the same delivery is applied, **Then** that item's cached state is unchanged and it is not woken.

---

### User Story 2 - A stale or partial event never unblocks an item (Priority: P1)

The event is only a trigger. The decision to remove `fabrik:blocked` is always made from a live read of the item's blockers.

**Why this priority**: Wrongly unblocking lets work start on an item whose prerequisites are not done; the payload's embedded data is known to be unreliable.

**Independent Test**: Deliver a `blocked_by_removed` event while the live read still shows an open blocker and confirm the item stays blocked.

**Acceptance Scenarios**:

1. **Given** a `fabrik:blocked` item, **When** a `blocked_by_removed` delivery arrives but the live blocker read still shows an open blocker, **Then** the item stays blocked and `fabrik:blocked` is not removed.
2. **Given** a delivery whose embedded counts disagree with the live state, **When** it is processed, **Then** the embedded counts have no effect on any decision.

---

### User Story 3 - Each dependency change is handled once per affected item (Priority: P2)

GitHub sends two deliveries per change (one from each side). The daemon does the work for each affected item once, not once per delivery, and identifies each side's repository correctly.

**Why this priority**: Prevents redundant invalidation and re-reads, and avoids misreading the repository from a field that is absent on one side.

**Independent Test**: Apply the `blocked_by_*` and `blocking_*` deliveries for the same change and confirm each of the two issues is invalidated and woken once in effect, and neither delivery fails for lack of a repository field.

**Acceptance Scenarios**:

1. **Given** the `blocked_by_added` and `blocking_added` deliveries for one edge, **When** both are applied, **Then** both issues' dependency data is refreshed on the next evaluation, with no repeated re-read caused by the second delivery.
2. **Given** a `blocking_*` delivery (which carries `blocked_issue_repo` rather than `blocking_issue_repo`), **When** it is applied, **Then** both issues are resolved to the correct repositories.

---

### User Story 4 - Closing a blocker promptly prompts re-evaluation of the items it blocks (Priority: P2)

Because closing a blocker sends no `issue_dependencies` delivery, the daemon uses the `issues` `closed` delivery for the blocker to prompt prompt re-evaluation of items whose cached dependencies name it.

**Why this priority**: Closes the "unblocking" gap the original problem statement described, which the new event alone cannot.

**Independent Test**: Close a blocker that a cached `fabrik:blocked` item lists and confirm the item is re-evaluated on the next wake, and that unrelated items are not.

**Acceptance Scenarios**:

1. **Given** items whose cached blockers include issue X, **When** `issues` `closed` arrives for X, **Then** those items (and only those) are woken or have their dependency data invalidated, using cache lookups only (no GitHub call).
2. **Given** that wake, **When** the item is evaluated, **Then** the decision to remove `fabrik:blocked` still comes from the live blocker read.

---

### User Story 5 - Operators know which setups get the event (Priority: P3)

Operators can tell from the documentation which event list to subscribe to and whether PAT-mode `--webhooks` forwarding delivers dependency changes.

**Why this priority**: Without correct documentation operators will not subscribe the App to the event or will expect PAT-mode behaviour that does not exist.

**Independent Test**: Read the webhook sections of the user guide, state-machine doc and Pruefer README and confirm each lists `issue_dependencies` where applicable, and the PAT-mode behaviour is stated.

**Acceptance Scenarios**:

1. **Given** the Hookdeck and App "Subscribe to events" guidance, **When** an operator follows it, **Then** `issue_dependencies` is among the events to subscribe to.
2. **Given** PAT-mode `--webhooks` forwarding, **When** the event can be subscribed to, **Then** it is in the forwarded list; **otherwise** the docs state that dependency changes stay poll-driven in PAT mode.

---

### Edge Cases

- A delivery names an issue that is not on the board or not in the cache: no effect, no error, no fetch.
- A delivery whose action is not one of the four known actions: ignored with a log line, not an error.
- A delivery with a missing or unreadable issue id, number or repository for one side: the readable side is still handled; nothing is guessed.
- The webhook cache is paused (stream unhealthy): no change from other event types; the poll remains the fallback.
- Cross-repository edges (blocker in a different repository from the blocked issue): both sides resolve to their own repositories.
- A delivery arrives before the dependent item has ever been deep-fetched: invalidation is harmless; the first evaluation reads live.
- Duplicate or reordered deliveries: re-invalidating is harmless and never changes any unblock decision, since the live read decides.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The board cache MUST recognise `issue_dependencies` deliveries and handle the four actions `blocked_by_added`, `blocked_by_removed`, `blocking_added` and `blocking_removed`; any other action is ignored and logged.
- **FR-002**: For each of the two issues a delivery names, the cache MUST invalidate that item's cached dependency data so its next evaluation re-reads it from GitHub; invalidation MUST be limited to those items and MUST NOT touch any other item.
- **FR-003**: Payload parsing MUST follow the real captured deliveries: it MUST identify each issue's repository from that issue's own object (not unconditionally from `blocking_issue_repo`), MUST use only the action and the two issue identifiers, and MUST NOT read the embedded dependency or sub-issue summaries.
- **FR-004**: An applied delivery that affects at least one cached board item MUST signal the engine wake path used by other event-driven deltas, so a `fabrik:blocked` item whose last blocker was removed is re-evaluated before the next poll.
- **FR-005**: The event MUST NOT, by itself, cause `fabrik:blocked` to be removed or applied. The decision MUST continue to go through `checkDependencies`'s live `BlockedBy` read (ADR-1419); a stale, partial or reordered payload MUST NOT unblock an item.
- **FR-006**: The two deliveries GitHub sends for one change MUST NOT cause repeated re-reads of the same item beyond what a single invalidation per affected item would cause.
- **FR-007**: On `issues` `closed` for an issue, the engine MUST wake or invalidate the cached items whose cached blocker list names that issue, using cache data only (no GitHub request), and MUST still decide through the live read (FR-005). It MUST leave items that do not list the closed issue untouched.
- **FR-008**: Where `gh webhook forward` can subscribe to `issue_dependencies` for a repository, the event MUST be added to the default forwarded event list; where it cannot, the documentation MUST say dependency changes stay poll-driven in PAT mode, and startup behaviour MUST NOT regress (an unsupported event is handled as other unsupported events are today).
- **FR-009**: Documentation MUST list `issue_dependencies` in the webhook event lists and App/Hookdeck "Subscribe to events" guidance in `docs/USER_GUIDE.md`, in `cmd/pruefer/README.md` where it has an equivalent list, and describe the behaviour (including that closing a blocker is signalled via `issues`, not this event) in `docs/state-machine.md`; `docs/llms-full.txt` MUST be regenerated.
- **FR-010**: A unit test MUST apply the recorded real deliveries (all four actions) to the board cache and show the affected items invalidated, a wake signalled, and an unrelated item untouched. The fixtures MUST be the captured deliveries under `boardcache/testdata/webhooks/` (from PR #2401 / branch `fixtures/webhook-captures-2352`), used verbatim.
- **FR-011**: A sim scenario MUST cover both (a) a `fabrik:blocked` item whose only blocker edge is removed, unblocked on the next wake rather than the next poll, and (b) a stale `blocked_by_removed` delivery while the live read still shows a blocker, where the item stays blocked. A further sim case SHOULD cover FR-007.

### Key Entities *(if applicable)*

- **Dependency-change delivery**: a webhook with an action, the blocked issue, the blocking issue and each issue's repository; one real change yields two of them.
- **Cached dependency data**: the item's cached list of blockers, which is only a hint; the live read is authoritative.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a human adds or removes a "blocked by" link on a board item while webhooks are delivered, the affected item is re-evaluated on the next wake (seconds), not after a full poll interval.
- **SC-002**: In every tested case where the live read shows an open blocker, the item stays blocked regardless of what the delivery says.
- **SC-003**: Applying a delivery changes no state for any board item other than the two issues it names.
- **SC-004**: Every one of the four captured real deliveries is accepted without error by the cache, with each issue resolved to its correct repository.
- **SC-005**: The user guide, state-machine doc and (where applicable) Pruefer README all list the event, and `docs/llms-full.txt` matches the regenerated bundle.

## Assumptions

- Invalidating the deep cache (rather than patching cached `BlockedBy` from the payload) is sufficient and preferred: it is simple, precise to the affected items, and keeps the live read as the single source of truth. A wake is required in addition, because invalidation alone does not wake the engine today.
- The transport-neutral webhook intake (#2351) is in place, so the event reaches the cache through the same ordered path as other events.
- Whether `gh webhook forward` supports `issue_dependencies` is unknown until checked against a real forward session; FR-008 covers both outcomes, and the finding is recorded in the docs.
- The existing `PushUnblockObserver` behaviour on blocker close is unchanged; FR-007 only closes the gap where a dependent's cached dependency data is absent or stale.
- The fixture location is `boardcache/testdata/webhooks/` (per the 2026-10-19 maintainer comment), superseding the `github/testdata/` location named in the original acceptance criteria.

## Out of Scope *(optional)*

- Applying the payload's embedded counts or issue objects to the cache, or deriving blocker counts from them.
- Other missing webhook subscriptions (tracked in #2352) and the transport-neutral intake itself (#2351).
- Changes to `checkDependencies`, `PushUnblockObserver`, or the ADR-1419 live re-read semantics.
- Handling of sub-issue relationship events.
- Any change to polling intervals or idle backoff.

## Source References *(optional)*

- Captured deliveries and their README: PR #2401, `boardcache/testdata/webhooks/issue_dependencies_{blocked_by,blocking}_{added,removed}.json`
- ADR-1419 (live re-read on resume), `docs/state-machine.md` §2.5 (Blocking Issue Closed)
- Maintainer comments of 2026-10-15 (live occurrence) and 2026-10-19 (capture findings and corrections)
