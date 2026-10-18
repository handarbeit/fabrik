# ADR 2345: Filter projects_v2_item webhooks by project and Status field

## Status

Accepted. Narrows the `projects_v2_item` handling of ADR 034/036 and generalises the text-field guard of ADR 2048. Prerequisite for any direct org/App webhook source (#2143).

## Context

`applyProjectsV2ItemDelta` applied every `projects_v2_item` event without asking which project or field it concerned. Org-level delivery carries events for every project in the org. Engine issues are also added to a public triage board by convention, so a `created` event from that board replaced our `ItemID` and blanked our Status, its later column edits became our Status, and `liveLandingState` could read the triage board's Status and skip a landing. An `edited` on any single-select field of our own board (Priority, Size) was read as a Status change. The damage self-healed at the next drift reconcile (about 3 minutes).

> **Partially superseded by [ADR 2383](2383-projects-v2-item-decode-node-id.md):** only `projects_v2_item.project_node_id` is read (no real delivery carries a top-level one), the item ID is `projects_v2_item.node_id`, and the fixtures are now verbatim (scrubbed) captures.

## Decision

- **Project filter first, every action.** The event's project node ID must equal the cache's project ID. A foreign, missing or not-yet-known ID ignores the event with one log line, before `FetchItemDetails` (which also spends no GraphQL budget on foreign traffic), the echo match and the removal.
- **Field filter for `edited`.** `changes.field_value.field_node_id` must equal the board's Status field ID. The ID lives in the cache (`SetStatusFieldID`), set by the engine where it resolves `e.statusField`, and resolved once lazily from the fallback client when a Status edit finds it unknown, so webhook-driven Status works before the first full-board poll. A failed lookup drops the event.
- **Fail closed (R3).** An event without the IDs is never applied as a Status change. The reconcile is the safety net (ADR 034/035).
- **Filter before echo.** The echo registry key has no project or field discriminator; a foreign or non-Status event carrying our item ID must not consume an entry.
- **Both nestings of `project_node_id` are decoded.** GitHub's documentation does not confirm whether it sits under `projects_v2_item` or at the top level, and a wrong choice would silently ignore every Status webhook.
- **R4 scope.** `applyProjectItem` never replaces a known `ItemID` with an empty or different one. The live board-listing applies (`BoardReconciled`, shallow, probe) are authoritative and may re-identify, so a genuine re-add of an issue to its board is still recovered by the reconcile. A webhook `created` for our own project carrying a different item ID does not replace a known one; the reconcile corrects it.
- **`applyLayer1StatusRefresh` stays unfiltered:** clearing the off-board negative cache on a foreign `created` is the safe direction.
- **Fixtures** live in `boardcache/testdata/webhooks/` (consumed by `boardcache`, outside the GraphQL/REST `github/testdata` wire-contract layer and its scrub scan). They follow the documented shape with placeholder IDs and are not verbatim captures; `TestProjectsV2ItemPayloadContract` fails if a relied-on field is missing from a fixture or the decoder does not read it.

## Consequences

- Foreign-project and non-Status events never touch the cache; Status edits with correct IDs apply as before.
- If the cache's project ID or Status field ID is wrong or unset, real Status webhooks degrade to reconcile latency (about 3 minutes), with only the debug log line as a signal.
- Test seam `SetProjectFieldFilterDisabledForTest` lets non-vacuity tests show the foreign cases corrupt state without the filters.
- No sim twin: the sim has no `projects_v2_item` webhook injection path. Adding one (audit §1.6) is a follow-up.
