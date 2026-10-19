# Webhook fixtures

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

## Repository and project events (captured 2026-10-19, for #2352 and #1995)

Verbatim deliveries from temporary webhooks pointing at an unroutable address:
a repository hook on `handarbeit/fabrik-test-alpha` and an org hook limited to
`projects_v2` against a throwaway project (#8, deleted afterwards). They are
scrubbed to the standard `TestWebhookFixturesAreScrubbedCaptures` enforces:
`sender`, `organization`, `projects_v2.creator` and `projects_v2.deleted_by` are
replaced as above, every string field holding an `api.github.com` or avatar URL
is removed, and the org login is rewritten to `example-org` (so repository
`full_name` reads `example-org/fabrik-test-alpha`). Numeric IDs, node IDs,
actions, `changes` blocks and every other field are as captured.

| Fixture | Event / action | Trigger |
|---|---|---|
| `issue_dependencies_blocked_by_added` / `_blocking_added` | `issue_dependencies` | `addBlockedBy` (#8766 blocked by #8767) |
| `issue_dependencies_blocked_by_removed` / `_blocking_removed` | `issue_dependencies` | `removeBlockedBy` |
| `sub_issues_sub_issue_added` / `_parent_issue_added` | `sub_issues` | `addSubIssue` (#8768 under #8766) |
| `sub_issues_sub_issue_removed` / `_parent_issue_removed` | `sub_issues` | `removeSubIssue` |
| `label_created` / `_edited` / `_deleted` | `label` | create, rename (`changes.name.from`), delete |
| `milestone_created` / `_edited` / `_deleted` | `milestone` | create, retitle (`changes.title.from`), delete |
| `issue_comment_edited` / `_deleted` | `issue_comment` | edit and delete of an issue comment |
| `pull_request_review_thread_resolved` / `_unresolved` | `pull_request_review_thread` | `resolveReviewThread` / `unresolveReviewThread` |
| `projects_v2_created` / `_edited` / `_closed` / `_reopened` / `_deleted` | `projects_v2` | project lifecycle; `edited` is a short-description change |

Observed facts the fixtures pin (do not assume otherwise):

- **One dependency change produces two deliveries**, one per side
  (`blocked_by_*` and `blocking_*`). Both carry both issues and their numeric
  IDs (`blocked_issue_id`, `blocking_issue_id`). The extra repository object
  differs by side: `blocked_by_*` carries `blocking_issue_repo`, `blocking_*`
  carries `blocked_issue_repo`. Read whichever is present; neither is
  guaranteed.
- **`sub_issues` likewise produces two deliveries per change**
  (`sub_issue_*` for the parent, `parent_issue_*` for the child).
- **Embedded objects are not a snapshot at the time of the action.** They
  reflect the object when GitHub rendered the delivery, which can be later than
  the action: `label_created` already carries the renamed name,
  `milestone_created` the edited title, and `parent_issue.sub_issues_summary.total`
  is 1 on both the `*_added` and `*_removed` deliveries.
  `issue_dependencies_summary` reads 0 on `blocked_by_added` and 1 on
  `blocked_by_removed`. Treat every embedded count, summary or name as
  unordered with respect to the action: act on `action` and the IDs, and read
  the current state live.
- **Closing a blocking issue produces no `issue_dependencies` delivery.** Only
  `issues` `closed` arrives, for the blocker.
- **Renaming a single-select field option produces no `projects_v2` delivery**
  (`updateProjectV2Field` echoing option IDs, with the org hook subscribed to
  `projects_v2`). `projects_v2` `edited` covers project fields such as the title
  and description only, so a Status option rename cannot be learned from a
  webhook.
- An `issue_comment` `created` delivery and the first `milestone` `edited`
  (title only) did not appear in the delivery log. The stored `milestone_edited`
  is from a second attempt that changed the title and description, and its
  `changes` names only `title`.
