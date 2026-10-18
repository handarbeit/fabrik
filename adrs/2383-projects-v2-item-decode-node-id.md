# ADR 2383: Decode `projects_v2_item` deliveries from `node_id`

## Status

Accepted. Partially supersedes ADR 2345 (the "both nestings of `project_node_id`" and "not verbatim captures" decisions).

## Context

`projectsV2ItemPayload` declared `projects_v2_item.id` as a string. Real deliveries send it as a number (`"id": 275291682`); the board item's `PVTI_…` node ID is `projects_v2_item.node_id`. `json.Unmarshal` failed on every real delivery, so webhook Status deltas never applied and the #2345 project and field filters (which run after the decode) were unreachable. The probe loop and reconcile hid it. A live probe on 2026-10-18 recorded ten real deliveries (kept as the fixtures); the hand-written fixtures had set `id` and `node_id` to the same string, which is how the bug passed review.

## Decision

- **The item ID is `projects_v2_item.node_id`.** It is the value the Store, `itemIDToKey` and the echo registry already key on. The numeric `id` is not declared and never used, not even as a fallback; a delivery without `node_id` is ignored.
- **Lenient decode.** Every declared scalar is a `lenientString` (a non-string yields `""`, never an error). A mistyped field the delta does not use cannot drop an event; a mistyped relied-on identifier becomes missing and is ignored and logged.
- **Only `projects_v2_item.project_node_id` identifies the project.** No real delivery carries a top-level `project_node_id`; the fallback is removed.
- **A Status clear is a named no-op.** An `edited` Status event has no `to` when the Status is cleared. An empty Status means "unknown" to the Store and several admission rules key on that, so a webhook never writes one: the cached Status is kept, `Status clear received for <id>; left to reconcile` is logged (without the word `ignored`; it is not a filter drop), and reconcile corrects it. The echo is matched first, so an engine-caused clear still consumes its echo entry.
- **Fixtures are verbatim captures.** `boardcache/testdata/webhooks/` holds the ten deliveries with `sender`, `projects_v2_item.creator` and `organization` replaced by minimal blocks (no `installation` block was present). Shapes with no capture are derived in test code by mutating a capture, never stored. `TestCapturedDeliveryFailsToDecodeWithStringID` keeps proving the captures carry a numeric `id`, and the captured-delivery tests fail under the old declaration.

## Consequences

- Webhook Status, archive and delete deltas now apply for real; the echo registry now sees `projects_v2_item` echoes of the engine's own Status moves. After release, watch `[cache] projects_v2_item` log lines.
- The engine's own Labels edits (`field_type: "labels"`) are filtered before the echo match and never consume an entry.
- Other webhook payload types were not probed and may have similar mismatches (out of scope).
