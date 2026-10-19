# projects_v2_item webhook fixtures

`projects_v2_item_*.json` are verbatim `projects_v2_item` deliveries captured
from a live org webhook on 2026-10-18 (#2383). Only personal and organisational
identity is scrubbed: `sender`, `projects_v2_item.creator` (same data as
`sender`) and `organization` are replaced by minimal real-shaped blocks. Node
IDs, field IDs and numeric IDs are as captured. No delivery carried an
`installation` block or a top-level `project_node_id`.

| Fixture | Delivery | Board |
|---|---|---|
| `created_draft` | `created`, a DraftIssue | #2 `PVT_kwDOENB0Ac4BYraz` |
| `edited_status_set` | Status null -> Backlog | #2 |
| `edited_status_change` | Status Backlog -> Specify | #2 |
| `edited_other_single_select` | non-Status single-select `Probe2345` | #2 |
| `edited_status_clear` | Status cleared: no `from`/`to` keys | #2 |
| `archived` / `restored` / `deleted` | item lifecycle | #2 |
| `edited_labels_board1_a` / `_b` | `field_type: "labels"` edit | #1 `PVT_kwDOENB0Ac4BW0n_` |

Shapes with no capture (a missing `node_id`, a missing `project_node_id`, a
mistyped field, a top-level `project_node_id`) are derived in test code by
mutating a capture (`deriveFixture` in `projects_v2_item_capture_test.go`);
none is stored here.

## `issue_dependencies` captures (captured 2026-10-19, for #1995)

Taken verbatim from the capture branch of #2352 (scrubbed to `example-org/fabrik-test-alpha`).

| Fixture | Action | Trigger |
|---|---|---|
| `issue_dependencies_blocked_by_added` / `_blocking_added` | `blocked_by_added` / `blocking_added` | `addBlockedBy` (#8766 blocked by #8767) |
| `issue_dependencies_blocked_by_removed` / `_blocking_removed` | `blocked_by_removed` / `blocking_removed` | `removeBlockedBy` |

One dependency change produces two deliveries, one per side. `blocked_by_*`
carries `blocking_issue_repo`; `blocking_*` carries `blocked_issue_repo`. The
issue objects carry no `repository_url`, only `html_url`. Embedded
`issue_dependencies_summary` counts are unordered with respect to the action and
are never read. Closing a blocking issue sends no `issue_dependencies` delivery.
