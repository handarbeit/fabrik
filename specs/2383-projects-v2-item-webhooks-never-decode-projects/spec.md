# Feature Specification: Decode real `projects_v2_item` webhook deliveries

**Feature Branch**: `fabrik/issue-2383`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description (verbatim):

```text
## Problem

Every real `projects_v2_item` webhook delivery fails to decode and is dropped. `projectsV2ItemPayload` (`boardcache/delta.go`) declares `projects_v2_item.id` as a `string`, but GitHub sends it as a number (`"id": 275291682`). The board item's node ID (`PVTI_…`) is in `projects_v2_item.node_id`. `json.Unmarshal` returns `cannot unmarshal number into Go struct field .projects_v2_item.id of type string`, and `applyProjectsV2ItemDelta` logs that and returns.

The field has been typed this way since May 2026, so webhook Status deltas have never been applied. The probe loop and reconcile have been covering Status changes. #2345's project and field filters are therefore unreachable in production: they run after the decode.

Found by a live probe on 2026-10-18. A temporary org webhook on handarbeit (deleted afterwards) recorded 10 real deliveries:
- 8 on the bed board #2, from a draft item: created, Status set twice, a non-Status single-select set, Status cleared, archived, restored, deleted.
- 2 from board #1: `Labels` edits by the dev daemon.

## Confirmed delivery shape

- `projects_v2_item.project_node_id` is present on every action, and there is **no** top-level `project_node_id`.
- `projects_v2_item.id` is a number, and `projects_v2_item.node_id` is the `PVTI_…` string. The engine's echo registry and `itemIDToKey` use the `PVTI_…` ID.
- `edited` carries `changes.field_value.{field_name, field_node_id, field_type, project_number}`. A single-select edit adds `from`/`to` (each `{id, name, color, description}`, or `null`). A Status **clear** has no `from`/`to` keys at all.
- The engine's own label writes produce `edited` with `field_type: "labels"` and a `field_node_id` for the board's Labels field, with no `to`.
- `archived`/`restored` carry `changes.archived_at.{from,to}`.

With only the struct tag changed to read `node_id`, the real payloads behave as #2345 intended:
- foreign-project events are ignored;
- non-Status single-select edits are ignored;
- Status edits apply;
- archived and deleted remove the item.

## Requirements

1. Decode the item's board ID from `projects_v2_item.node_id`. Read the numeric `id` as a number, or don't read it.
2. A payload that decodes with an unexpected type in a field the delta does not use must not drop the whole event.
3. Replace the hand-written `boardcache/testdata/webhooks/projects_v2_item_*.json` fixtures with the captured deliveries. Scrub `sender`, the `organization` block and `installation` (if present), keep the real field layout, and remove the "not a verbatim capture" notes.
4. Drop the top-level `project_node_id` fallback (`effectiveProjectNodeID`), or keep it with a comment saying no real delivery has carried it.
5. Decide what a Status **clear** does (an `edited` Status event with no `to`). Today it is a no-op that leaves the cached Status stale until reconcile.
6. Add a regression test that feeds a captured delivery through `applyProjectsV2ItemDelta` and asserts the Store changed. Prove it fails on today's `string` tag.

## Scope

- In scope: `boardcache/delta.go` (`projectsV2ItemPayload`, `applyProjectsV2ItemDelta`), the webhook fixtures and their tests.
- Out of scope: other webhook payload types. They were not probed. `singleSelectTo.id` is a string in real payloads, which is correct.

## Acceptance

- The captured deliveries apply through the real code path. Each Status edit on the own board changes the cached Status, and the foreign, non-Status and labels edits log `ignored` and change nothing.
- The fixtures are verbatim captures (scrubbed), with no placeholder shapes left.

The raw captures are in `~/dev/fabrik-e2e-logs/webhook-probe-2345/` on the dev machine. Mentioned for context: #2345.
```

## Background

The engine can receive GitHub webhook deliveries and apply them directly to its in-memory item cache, so a board change shows up without waiting for the next poll or reconcile. For project-board item events (`projects_v2_item`), that path has never worked against real GitHub traffic. The payload structure declares the item's `id` as text, but GitHub sends it as a number. Decoding fails, the event is logged as an unmarshal error and discarded. This has been the case since the field was introduced in May 2026.

The system still behaves correctly in practice, because the periodic probe and reconcile loops pick up Status changes. The cost is that the webhook fast path is dead, and the project and field filters added in #2345 can never run in production, since they sit after the failing decode.

A live probe on 2026-10-18 recorded 10 real deliveries: 8 on the test-bed board (created, Status set twice, a non-Status single-select set, Status cleared, archived, restored, deleted) and 2 `Labels` edits on the main board. They confirmed that:

- the item's board ID (`PVTI_…`) is delivered as `projects_v2_item.node_id`;
- the numeric `projects_v2_item.id` is a different value;
- the project identifier is only ever nested inside `projects_v2_item`, never at the top level;
- a Status clear carries no new-value information at all.

With only the identifier source corrected, the real deliveries already behave as #2345 intended: foreign-project events are ignored, non-Status single-select edits are ignored, Status edits apply, and archived and deleted events remove the item. The fix is therefore small. The larger part of the work is making the test fixtures honest, because today's hand-written fixtures encoded the wrong shape and let the bug through.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A Status change on the board reaches the cache via webhook (Priority: P1)

An operator moves an item to a different Status column on the engine's own project board. The webhook delivery for that edit is decoded, recognised as a Status edit on this board, and the cached Status of the item is updated without waiting for the next probe or reconcile.

**Why this priority**: This is the defect. Until real deliveries decode, the whole webhook path for board items is dead.

**Independent Test**: Feed each captured own-board Status edit delivery through the delta application path against a store holding the corresponding item, and assert the cached Status equals the delivery's new Status name.

**Acceptance Scenarios**:

1. **Given** a cached item whose board ID equals the delivery's `projects_v2_item.node_id` and whose Status differs from the delivery's new value, **When** the captured own-board Status `edited` delivery is applied, **Then** the cached Status becomes the delivery's new Status name.
2. **Given** the same item, **When** a second captured Status edit follows, **Then** the cached Status changes again to that delivery's value.
3. **Given** the pre-fix behaviour (numeric `id` read into a text field), **When** the same captured delivery is applied, **Then** the regression test fails, which proves the test detects the bug.

---

### User Story 2 - Irrelevant deliveries are recognised and ignored (Priority: P1)

The org-level webhook delivers events for every project in the organisation and for every field on this board. Events for another project, for a non-Status single-select field, and for the Labels field (which the engine's own label writes generate) must be recognised after a successful decode, logged as ignored, and must change nothing.

**Why this priority**: The #2345 filters exist for exactly this traffic. They must be reachable and correct on real payloads, or Status edits for other boards or fields could corrupt the cache.

**Independent Test**: Feed the captured foreign-project, non-Status single-select and Labels-field deliveries through the delta path and assert no store change plus an `ignored` log line for each.

**Acceptance Scenarios**:

1. **Given** a captured delivery from another project, **When** it is applied, **Then** the store is unchanged and an `ignored` line naming the foreign project is logged.
2. **Given** a captured non-Status single-select edit on the own board, **When** it is applied, **Then** the store is unchanged and an `ignored` line is logged.
3. **Given** a captured `field_type: "labels"` edit, **When** it is applied, **Then** the store is unchanged and an `ignored` line is logged.

---

### User Story 3 - Item lifecycle events keep the cache accurate (Priority: P2)

Archiving or deleting a board item removes it from the cache; adding an issue to the board is recognised; restoring an item is left to reconcile (existing behaviour).

**Why this priority**: These events also go through the same ID field and were equally dead. They are secondary to Status edits because reconcile covers them.

**Independent Test**: Apply the captured `archived` and `deleted` deliveries against a store holding the item and assert it is removed; apply `restored` and assert the cache is unchanged.

**Acceptance Scenarios**:

1. **Given** a cached item whose board ID matches the delivery's `node_id`, **When** the captured `archived` (or `deleted`) delivery is applied, **Then** the item is removed from the store.
2. **Given** a captured `restored` delivery, **When** it is applied, **Then** the store is unchanged and the next reconcile is what re-adds the item.
3. **Given** a captured `created` delivery for a draft item, **When** it is applied, **Then** it is ignored as a non-issue item, as today.

---

### User Story 4 - An unexpected shape in an unused field does not lose the event (Priority: P2)

If GitHub changes the type of a field the cache does not use (as happened with the numeric `id`), the event must still be processed using the fields it does need.

**Why this priority**: This is the class of failure behind the bug. One mistyped, unused field discarded every delivery for five months with only a log line.

**Independent Test**: Take a captured Status edit, change the type of several fields the delta never reads (for example the numeric `id`, timestamps in other blocks, `field_name`) and assert the Status still applies.

**Acceptance Scenarios**:

1. **Given** a Status edit delivery in which an unused field has an unexpected JSON type, **When** it is applied, **Then** the cached Status still updates.
2. **Given** a delivery in which a field the delta does use (for example `node_id` or `project_node_id`) has an unexpected type, **When** it is applied, **Then** it is logged and ignored, and the store is unchanged.

---

### User Story 5 - Clearing a Status is handled deliberately (Priority: P3)

An operator clears an item's Status on the board. The delivery is a Status-field edit with no `from` or `to` at all. The behaviour is an explicit, named rule with its own log line, not an accident of an empty-name check.

**Why this priority**: Low frequency, and reconcile corrects it. The requirement is that the behaviour is a decision, tested and documented, rather than silent.

**Independent Test**: Apply the captured Status-clear delivery and assert the documented outcome (see FR-007) and a log line that names the clear specifically.

**Acceptance Scenarios**:

1. **Given** a cached item with a Status, **When** the captured Status-clear delivery is applied, **Then** the store is unchanged and a log line states that a Status clear was received and is left to reconcile.

---

### Edge Cases

- A delivery with `projects_v2_item.node_id` missing or empty: logged and ignored; nothing is keyed on the numeric `id` as a fallback.
- A delivery with no project identifier under `projects_v2_item`: ignored, as today; a top-level identifier is not consulted (see FR-005).
- The numeric `id` is larger than a 32-bit integer or is delivered as a string in some future shape: neither case affects processing.
- An `edited` delivery for a field type other than single-select or labels (for example a text field written by the engine's display-only fields): remains a no-op.
- The engine's own Status write echoing back as a webhook: the echo registry is matched using the `PVTI_…` ID, so the echo is attributed to the engine and not to a human.
- An `edited` Status event for an item the cache does not hold: no change, no error.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The webhook payload decoder MUST take the board item identifier from `projects_v2_item.node_id` (the `PVTI_…` value). The numeric `projects_v2_item.id` MUST NOT be used as an identifier anywhere in the delta path.
- **FR-002**: Every place the delta path uses the item identifier (item creation bookkeeping, echo matching, Status application, removal on `archived` and `deleted`) MUST use the `node_id` value, consistently with the identifier the engine's echo registry and item lookup already use.
- **FR-003**: Decoding MUST tolerate an unexpected JSON type in any field the delta does not use. Only fields the delta actually relies on may cause an event to be rejected, and a rejection MUST be logged with the reason and leave the store unchanged.
- **FR-004**: Against the captured real deliveries, the delta path MUST behave as follows. An own-board Status edit changes the cached Status. An edit for a foreign project, for a non-Status single-select field, or for the Labels field is logged as `ignored` and changes nothing. `archived` and `deleted` remove the item. `restored` changes nothing. A `created` event for a non-issue (draft) item is ignored.
- **FR-005**: The top-level `project_node_id` fallback MUST be removed, because no real delivery carries it; the project identity MUST come only from `projects_v2_item.project_node_id`. An event without it is ignored as today.
- **FR-006**: The ignored-event log lines for foreign project, non-Status field and labels-field edits MUST each be distinguishable and contain the word `ignored`.
- **FR-007**: A Status edit that carries no new value (a Status clear) MUST be a named, logged rule that leaves the cached Status unchanged and defers to the next reconcile. The log line MUST say that a Status clear was received, and the behaviour MUST be covered by a test using the captured delivery.
- **FR-008**: The `projects_v2_item` webhook test fixtures MUST be the captured real deliveries, scrubbed of `sender`, the `organization` block and `installation` (where present), keeping GitHub's real field layout. All "not a verbatim capture" notes and placeholder shapes MUST be removed.
- **FR-009**: A regression test MUST feed captured deliveries through the delta application path and assert that the Store changed (for a Status edit, the cached Status). It MUST be demonstrated to fail when the item ID is read from the numeric `id` as text, and to pass with the fix.
- **FR-010**: Existing tests for the #2345 project and field filters MUST be re-expressed over the captured deliveries, so they exercise the filters on real payloads.

### Key Entities *(if applicable)*

- **Board item webhook delivery**: One `projects_v2_item` event with an action (`created`, `edited`, `archived`, `restored`, `deleted`), the item block (numeric `id`, `node_id`, `project_node_id`, content identity, timestamps), and for `edited` a `changes.field_value` block describing the field edited.
- **Board item ID**: The `PVTI_…` node identifier of the item on the board, used by the cache to find the item.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of the captured real deliveries (10 recordings) decode without error through the delta path.
- **SC-002**: Each captured own-board Status edit changes the cached Status of the matching item, with no probe or reconcile involved.
- **SC-003**: Every captured foreign-project, non-Status and labels-field delivery produces an `ignored` log line and zero store changes.
- **SC-004**: The regression test fails on the pre-fix code and passes after the fix.
- **SC-005**: No fixture in the webhook test data carries a "not a verbatim capture" note or a hand-invented payload shape.
- **SC-006**: A delivery whose unused fields have unexpected types is still applied, shown by a passing test.

## Assumptions

- The ten recorded deliveries in the dev machine's probe log directory are representative of real `projects_v2_item` traffic; a fixture needing a shape not captured (for example a payload with no project ID) is derived by removing fields from a capture and says so, instead of being invented.
- **Status clear (requirement 5):** leaving the cached Status unchanged and deferring to reconcile is the chosen behaviour. An empty Status means "unknown" to the cache, and several rules key on that, so writing an empty value from a webhook could make a cleared item look like a never-seen one and change how it is admitted. Reconcile already corrects the cache. This is low-impact and reversible; the change from today's silent no-op is that it becomes a named, logged, tested rule.
- Scrubbing replaces only personal or organisational identity in `sender`, `organization` and `installation`; node IDs, field IDs and numeric IDs are kept as captured since they are needed for the tests to be meaningful.
- Reconcile and the probe loop remain the safety net; this change does not alter them.

## Out of Scope *(optional)*

- Other webhook payload types (`issues`, `issue_comment`, `pull_request`, reviews, `check_run`, …). They were not probed and may have similar mismatches; a follow-up probe is the way to find out.
- Changing how `singleSelectTo.id` is read; it is a string in real payloads and correct.
- Applying `restored` events directly to the cache; reconcile continues to re-add restored items.
- Any change to the probe, reconcile or echo-registry design.

## Source References *(optional)*

- Live probe of 2026-10-18: raw captures in `~/dev/fabrik-e2e-logs/webhook-probe-2345/` on the dev machine (10 deliveries: created, edited ×5, archived, restored, deleted, plus board #1 Labels edits).
- #2345: the project and field filters this fix makes reachable.
- Affected code: the `projects_v2_item` payload type and delta application in the board cache package, and its webhook test fixtures.
