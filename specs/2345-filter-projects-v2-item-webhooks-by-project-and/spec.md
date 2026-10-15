# Feature Specification: Filter projects_v2_item webhooks by project and Status field

**Feature Branch**: `fabrik/issue-2345`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `projects_v2_item` webhook events are not filtered by project or by field. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.6.
>
> The payload struct (boardcache/delta.go:219) never reads `project_node_id` or `changes.field_value.field_node_id`. Org-level delivery (gh webhook forward in org mode, Hookdeck App hooks) carries events for every project in the org.
>
> - A `created` on another board runs `FetchItemDetails` then `IssueOpened{ItemID: <foreign item>, Status: ""}` (delta.go:1135-1147), and `applyProjectItem` overwrites our ItemID and Status. This happens whenever an engine issue is also added to the public triage board (an established convention). Afterwards the triage board's column edits become our Status, a triage archive removes our item, our own board's edits stop resolving, `UpdateProjectItemStatus(ourProject, foreignItemID)` fails, and `liveLandingState` (merge_train.go:2253) reads the triage board's Status and can skip a landing as "already completed".
> - An `edited` on any single-select field of our own board (Priority, Size) sets `item.Status` to that option's name (delta.go:1163-1190, store.go:1238).
>
> Self-heals at the next drift reconcile (about 3 min).
>
> ## Requirements
>
> - **R1:** A `projects_v2_item` event whose `project_node_id` is not this instance's project ID is ignored (one debug log line).
> - **R2:** An `edited` event changes Status only when its field node ID is this board's Status field ID. Other single-select fields are ignored.
> - **R3:** An event without the IDs (older payload shapes, sim fixtures) is not applied as a Status change; it falls back to the existing reconcile.
> - **R4:** `applyProjectItem` never replaces a known ItemID with an empty or foreign one.
>
> ## Acceptance
>
> - Delta tests with captured payload shapes: a foreign-project `created`/`edited`/`archived` changes nothing; a non-Status single-select edit changes nothing; our Status edit still applies.
> - A wire-contract fixture for the `projects_v2_item` payload fields used.
> - This is a prerequisite for any direct org/App webhook source (#2143).

## Background

Fabrik keeps a cached view of its project board and updates it from `projects_v2_item` webhook events. Those events are currently applied without checking which project they belong to or which field changed. Org-level webhook delivery (org-mode `gh webhook forward`, Hookdeck App hooks) delivers events for every project in the organisation, not just the board this Fabrik instance serves.

This matters in practice because it is an established convention to add engine issues to a second, public triage board. Events from that board then leak into the engine's cache:

- A `created` event for the issue on the triage board replaces the engine's stored project-item identity and Status with the triage board's. After that, the triage board's column moves become the engine's Status, a triage-board archive removes the engine's item, edits on the engine's own board no longer resolve, status writes against the engine's board fail because they use the foreign item identity, and the merge-train's live "already landed?" check can read the triage board's Status and wrongly skip a landing as already completed.
- An `edited` event on any single-select field of the engine's own board (for example Priority or Size) is treated as a Status change, setting the item's Status to that option's name.

The damage self-heals at the next drift reconcile (about 3 minutes), but in that window the engine can make wrong decisions, including skipping a landing. The finding comes from the engine state audit of 2026-10-15 (§1.6). Filtering is also a prerequisite for any direct org/App webhook source (#2143), since such a source would deliver foreign-project events at much higher volume.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Foreign-project events never touch the engine's cache (Priority: P1)

An operator whose engine issues are also on a public triage board runs Fabrik with org-level webhook delivery. Whatever happens on the triage board (item added, column moved, item archived), the engine's cached item identity and Status are unchanged.

**Why this priority**: This is the harmful case — it corrupts item identity and can cause a landing to be skipped.

**Independent Test**: Feed the delta handler `created`, `edited` and `archived` payloads whose project ID differs from the instance's project; assert the cached item state is unchanged and no item-detail fetch is made for them.

**Acceptance Scenarios**:

1. **Given** an item cached with its own item identity and Status "Validate", **When** a `created` event arrives for the same issue on another project, **Then** the cached item identity and Status are unchanged.
2. **Given** the same cached item, **When** an `edited` Status change arrives from another project, **Then** the cached Status is unchanged.
3. **Given** the same cached item, **When** an `archived` event arrives from another project, **Then** the item is not removed or marked archived.
4. **Given** any ignored foreign event, **When** it is processed, **Then** exactly one debug log line records that it was ignored.

---

### User Story 2 - Only Status-field edits change Status (Priority: P1)

On the engine's own board, editing another single-select field (Priority, Size) must not alter an item's Status; editing the Status field must still do so.

**Why this priority**: Without it, routine board curation by humans silently changes engine state.

**Independent Test**: Feed `edited` payloads for the engine's own project with a non-Status single-select field ID and with the Status field ID; assert only the latter changes Status.

**Acceptance Scenarios**:

1. **Given** a cached item with Status "Plan", **When** an `edited` event for our project changes the Priority field, **Then** Status remains "Plan".
2. **Given** the same item, **When** an `edited` event for our project changes the Status field to "Implement", **Then** Status becomes "Implement" as it does today.

---

### User Story 3 - Events without identifying IDs fall back to reconcile (Priority: P2)

Older payload shapes and sim fixtures that lack project or field IDs are not trusted to change Status; the existing reconcile brings the cache to the right state.

**Why this priority**: Defensive, fail-safe behaviour; protects against ambiguity rather than a known frequent case.

**Independent Test**: Feed an `edited` payload with no project ID or no field ID; assert Status is unchanged and reconcile still corrects it.

**Acceptance Scenarios**:

1. **Given** an `edited` event lacking the field node ID, **When** it is processed, **Then** no Status change is applied.
2. **Given** a payload lacking the project node ID, **When** it is processed, **Then** it is not applied as a Status change and the next reconcile remains the source of correction.

---

### User Story 4 - A known item identity is never overwritten by an empty or foreign one (Priority: P2)

As a defence in depth independent of webhook filtering, applying a project item to the cache never replaces a known item identity with an empty or foreign one.

**Why this priority**: Protects against any other path (not only webhooks) that could deliver a bad item identity.

**Independent Test**: Apply a project item carrying an empty item identity, and one carrying a different one, to a cached item with a known identity; assert the identity is retained.

**Acceptance Scenarios**:

1. **Given** a cached item with a known item identity, **When** an update arrives with an empty identity, **Then** the known identity is kept.
2. **Given** a cached item with a known item identity, **When** an update arrives with a foreign identity, **Then** the known identity is kept.
3. **Given** a cached item with no known identity, **When** an update arrives with an identity, **Then** it is adopted.

---

### Edge Cases

- A foreign-project `archived` or `deleted` event must not remove or mark the engine's item.
- An event for our project whose item the cache does not yet know is handled as today (not affected by this change), provided project ID matches.
- A legitimate re-identification (the engine's item genuinely re-added to its own board with a new item ID) must still be recoverable via the existing reconcile, since the live board listing is authoritative.
- The instance's project ID or Status field ID is not yet resolved when an event arrives: the event is not applied as a Status change (treated as missing IDs) and reconcile covers it.
- Payloads with an unexpected shape never cause a panic and never apply a Status change.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A `projects_v2_item` event whose project node ID is not this instance's project ID is ignored — no cache change, no item-detail fetch — and one debug log line records that it was ignored.
- **FR-002**: An `edited` event changes an item's Status only when its changed-field node ID equals this board's Status field ID; edits to any other field (including other single-select fields) are ignored.
- **FR-003**: An event lacking the project node ID, or an `edited` event lacking the changed-field node ID, is not applied as a Status change; the cache is left for the existing reconcile to correct.
- **FR-004**: Applying a project item to the cache never replaces a known item identity with an empty or different one; an identity is adopted only when none is known.
- **FR-005**: Foreign-project `created`, `edited` and `archived` events each leave the cached item (identity, Status, archived/removed state) unchanged.
- **FR-006**: An `edited` event for this board's Status field continues to update Status exactly as before.
- **FR-007**: Delta tests cover, using captured real payload shapes: foreign-project `created`/`edited`/`archived` (no change), a non-Status single-select edit (no change), a Status edit on our board (applies), and events missing the IDs (no Status change).
- **FR-008**: A wire-contract fixture records the `projects_v2_item` payload fields the engine relies on (project node ID, changed-field node ID, item identity, action), validated alongside the existing wire-contract tests.

### Key Entities *(if applicable)*

- **Project node ID**: Identifier of the board an event belongs to; compared with this instance's project.
- **Field node ID**: Identifier of the field an `edited` event changed; compared with this board's Status field.
- **Project item identity**: The engine's identifier for an issue's card on its own board, used for status writes and live Status reads.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With an issue present on both the engine board and a second board, no sequence of events from the second board changes the engine's cached item identity, Status or archived state.
- **SC-002**: Editing any non-Status field on the engine's own board produces zero Status changes in the cache.
- **SC-003**: Every Status edit on the engine's board delivered with correct IDs is still applied (no regression in webhook-driven Status updates).
- **SC-004**: The merge-train's live "already landed?" check can no longer be misled by a foreign board's Status delivered via webhook.
- **SC-005**: Delta and wire-contract tests for all the above pass in the standard test run.

## Assumptions

- The instance's project ID and Status field ID are known to the engine (resolved at startup) and available to the webhook delta handling.
- Real `projects_v2_item` payloads carry the project node ID and, for `edited`, the changed-field node ID; payloads missing them are rare legacy/sim shapes.
- Ignoring (rather than attempting to reconcile) events without IDs is acceptable: the drift reconcile (about 3 min) is the existing safety net.
- Fixtures used by the sim bed that omit these IDs may need updating so that sim-driven Status webhooks still apply; the sim must stay faithful to real payloads.
- One debug log line per ignored event is sufficient observability; no warnings-panel entry or metric is needed.

## Out of Scope *(optional)*

- Building a direct org/App webhook source (#2143) — this change only makes it safe.
- Changes to the drift reconcile cadence or reconcile logic.
- Filtering other webhook event types (issues, comments, pull requests) by project.
- Per-project routing for instances that serve multiple boards.

## Source References *(optional)*

- Engine state audit, `docs/design/engine-state-audit-2026-10.md` (2026-10-15), §1.6
- Related: #2143 (direct org/App webhook source)
