# Feature Specification: Subscribe to the webhook events for facts the engine uses

**Feature Branch**: `fabrik/issue-2352`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "## Problem

The engine uses facts that no webhook it subscribes to carries. Part of the state foundation from the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15). It must land before a second webhook source (#2143). See §5.

`defaultWebhookEvents` (engine/webhook.go:65) omits `sub_issues`, `pull_request_review_thread` (resolve/unresolve), `label` (rename/delete), `milestone` (edit) and `projects_v2` (Status option renames). #1995 covers `issue_dependencies`. These facts come only from deep fetches or live reads, and the freshness rule can mask them (#1945). `issue_comment` and `pull_request_review_comment` `edited`/`deleted` are ignored too.

## Requirements

- **R1:** Subscribe to and apply `sub_issues`, `pull_request_review_thread`, `label`, `milestone` and `projects_v2`, each through the ordered Store path.
- **R2:** Apply comment and review-comment `edited`/`deleted`.
- **R3:** Each new event that changes a dispatch fact sets the right wake flag.

## Acceptance

- Delta tests with captured payloads per event; wire-contract fixtures for the fields read.
- A sim twin: resolving a review thread is seen without a refetch."

## Background

The engine audit (2026-10-15, §5) found that the engine relies on facts no subscribed webhook carries. Today those facts reach the item Store only through a deep fetch or a live read, and the cache freshness rule (#1945, ADR 2180) can leave a stale copy in place until the next probe notices a difference. The result is lag, or a missed change, for facts such as "this review thread is now resolved" (which the feedback gate reads), "this label was renamed", or "this milestone's title changed". Comment and review-comment `edited`/`deleted` deliveries are subscribed but ignored by the delta handlers, so a deleted or edited comment is not seen until a refetch.

This is part of the `statefix` state foundation. It must land before a second, direct webhook source (#2143) is added, because that source would otherwise inherit the same blind spots. It is blocked by #2351 and sits beside #1995 (`issue_dependencies`), which covers dependency edges separately.

Captured real deliveries (PR #2401, `boardcache/testdata/webhooks/`; branch `fixtures/webhook-captures-2352` if that PR has not merged) changed the scope as originally written:

- **`projects_v2` does not cover Status option renames.** Renaming a single-select option produced no `projects_v2` delivery; `projects_v2` `edited` covers only the project title and description. Option renames are therefore dropped from this issue, and the existing reconcile and status-field lookup stay the way the engine learns them. `projects_v2` is subscribed only if a project-level fact the engine uses (for example the project being closed or deleted) justifies it.
- **`sub_issues` sends two deliveries per change**: `sub_issue_added`/`sub_issue_removed` on the parent and `parent_issue_added`/`parent_issue_removed` on the child. Both must be handled and each change applied once.
- `label` `edited` carries `changes.name.from`; `milestone` `edited` carries `changes.title.from`.
- `pull_request_review_thread` `resolved`/`unresolved` carry `thread.node_id` and the thread's comments.
- `issue_comment` `edited`/`deleted` were captured; no `created` was (existing fixtures cover it).
- **Embedded objects in a delivery reflect render time, not the action.** `label_created` already carries the renamed name, `milestone_created` the edited title, and `sub_issues_summary` reads the same on add and remove. Handlers key on `action` and IDs, plus `changes.*.from` where present, and treat embedded names, counts and summaries as possibly later than the event.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Review thread resolution is seen without a refetch (Priority: P1)

A reviewer resolves (or unresolves) a review thread on a PR the engine is tracking. The engine's view of unresolved feedback changes from the webhook alone, so the feedback gate stops holding (or starts holding) the item without waiting for a deep fetch.

**Why this priority**: The feedback gate (#1953) holds every advance and landing on unresolved threads; a stale resolved/unresolved state directly delays or wrongly permits advancing. It is the named sim-twin acceptance case.

**Independent Test**: Deliver a captured `pull_request_review_thread` `resolved` payload to a cache-backed engine and observe the thread's resolved state and the item's feedback verdict change with no GitHub read; deliver `unresolved` and observe it reverse.

**Acceptance Scenarios**:

1. **Given** a tracked PR with an unresolved thread holding the feedback gate, **When** a `resolved` delivery for that `thread.node_id` arrives, **Then** the Store marks that thread resolved, the gate no longer counts it, and the item is woken for re-evaluation, with no refetch.
2. **Given** a resolved thread, **When** an `unresolved` delivery arrives, **Then** the Store marks it unresolved and the gate holds again.
3. **Given** a delivery for a thread the Store does not know, **When** it arrives, **Then** it is ignored without error and the next deep fetch or reconcile remains the source.

---

### User Story 2 - Edited and deleted comments are reflected (Priority: P1)

A human edits or deletes an issue comment or a PR review comment. The Store's copy of the comment follows.

**Why this priority**: Comments drive pausing, resume, review feedback and the pending-comment predicate; a deleted comment that stays cached can keep a gate holding, and an edited one feeds stale text to a worker.

**Independent Test**: Deliver captured `issue_comment` `edited` and `deleted` payloads (and the `pull_request_review_comment` equivalents) and compare the Store's comments for the item before and after.

**Acceptance Scenarios**:

1. **Given** a cached comment, **When** an `edited` delivery for it arrives, **Then** the cached body is replaced and nothing else about the comment (creation time, author, processed/reaction state) changes.
2. **Given** a cached comment, **When** a `deleted` delivery arrives, **Then** it is removed from the item's comments and no longer counts as pending feedback.
3. **Given** a comment the Store does not hold, **When** an `edited` or `deleted` delivery arrives, **Then** it is ignored without error.
4. **Given** an already-processed (🚀) comment, **When** it is edited, **Then** it does not become unprocessed.

---

### User Story 3 - Label and milestone renames and deletions are reflected (Priority: P2)

A repository label is renamed or deleted, or a milestone is edited or deleted. Cached items carrying that label or milestone reflect the change without a deep fetch.

**Why this priority**: State is carried in labels (`fabrik:*`, `stage:*`, `base:*`) and the cached milestone feeds the local API and channel scopes; a rename leaves cached items under the old name until a refetch.

**Independent Test**: Deliver captured `label` `edited` (with `changes.name.from`), `label` `deleted`, `milestone` `edited` (with `changes.title.from`) and `milestone` `deleted` payloads against a Store holding items that carry the old name, and observe those items update.

**Acceptance Scenarios**:

1. **Given** cached items carrying label `old`, **When** `label` `edited` with `changes.name.from = old` arrives, **Then** every cached item of that repository carrying `old` now carries the new name (taken from the label in the delivery).
2. **Given** cached items carrying a label, **When** `label` `deleted` arrives, **Then** the label is removed from those items.
3. **Given** cached items in a milestone, **When** `milestone` `edited` arrives, **Then** their cached milestone title follows; **When** `milestone` `deleted` arrives, **Then** their milestone becomes "none".
4. **Given** a `label` or `milestone` `created` delivery, **Then** no item changes.

---

### User Story 4 - Sub-issue links are tracked from both deliveries (Priority: P3)

A sub-issue relationship is added or removed. GitHub sends one delivery for the parent and one for the child; the Store records the relationship once.

**Why this priority**: The relationship is not a dispatch fact today, so nothing blocks on it, but it is one of the facts the audit lists as unheard and it must be heard before a second source is added.

**Independent Test**: Deliver the captured `sub_issue_added` and `parent_issue_added` pair and confirm one relationship is recorded; deliver the `_removed` pair and confirm it is removed.

**Acceptance Scenarios**:

1. **Given** neither item records the link, **When** both the parent-side and child-side `added` deliveries arrive in either order, **Then** the link exists exactly once.
2. **Given** a recorded link, **When** both `removed` deliveries arrive, **Then** the link is gone and no error results from the second one.
3. **Given** only one of the two deliveries arrives, **Then** the link is still applied correctly from that one.

---

### Edge Cases

- A delivery for an item, thread, comment or label the Store has never seen is ignored, never an error; reconcile and deep fetch remain the fallback.
- Out-of-order or replayed deliveries (including the duplicate sub-issue pair) obey the existing observed-at ordering rule (ADR 2348): a stale delivery cannot overwrite a newer fact.
- A `label` `edited` delivery whose `changes.name.from` is absent (for example a color or description edit) renames nothing.
- A label rename to a name an item already carries must not duplicate the label on that item.
- A delivery for a repository the engine does not manage is ignored.
- Embedded names, counts and summaries in a payload may be later than the event; handlers must not infer the action from them.
- A rename of a `fabrik:*`/`stage:*` label is applied as data; the Store does not interpret it beyond the label name, and the next fetch remains authoritative.
- The listener must still ack inside the existing time bounds when the new handlers are added (no network call is added to the apply path).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The default webhook event list MUST include `sub_issues`, `pull_request_review_thread`, `label` and `milestone`, and every transport that takes the list (gh-forward and Hookdeck) MUST request them.
- **FR-002**: `pull_request_review_thread` `resolved` and `unresolved` MUST update the resolved state of the thread identified by `thread.node_id` in the Store, through the ordered Store path, without a GitHub read.
- **FR-003**: `sub_issues` deliveries for `sub_issue_added`/`sub_issue_removed` (parent side) and `parent_issue_added`/`parent_issue_removed` (child side) MUST all be handled, keyed on `action` and issue IDs only, and a given add or remove MUST take effect once regardless of which delivery arrives first or whether both arrive.
- **FR-004**: `label` `edited` MUST rename the label on every cached item of that repository carrying `changes.name.from`, and `label` `deleted` MUST remove it; `label` `created` MUST change nothing.
- **FR-005**: `milestone` `edited` MUST update the cached milestone title (using `changes.title.from` to find affected items) and `milestone` `deleted` MUST set the cached milestone to "none" on affected items.
- **FR-006**: `issue_comment` `edited` and `deleted` MUST update or remove the cached comment; `pull_request_review_comment` `edited` and `deleted` MUST do the same for PR review-thread comments.
- **FR-007**: An `edited` comment MUST NOT change a comment's processed state; a `deleted` comment MUST stop counting as pending feedback.
- **FR-008**: Every new application MUST go through the item Store with an observed-at time per ADR 2348 (the payload's GitHub time where it carries one, else receipt time), so that a stale or replayed delivery cannot overwrite a newer fact.
- **FR-009**: Each new application that changes a fact the engine dispatches on (resolved threads and comment add/remove/edit as feedback; label changes as engine state) MUST raise the matching change flag so the poll wakes, and applications that change only informational facts (sub-issue links, milestone title) MUST NOT wake the poll.
- **FR-010**: Unknown subjects (item, thread, comment, label, milestone not in the Store) and unrecognised actions MUST be ignored with at most one log line and no error.
- **FR-011**: Handlers MUST NOT make a GitHub API call on the apply path.
- **FR-012**: Each new event type MUST have delta tests using the captured payloads as fixtures (not hand-written shapes), and the fields read MUST be pinned by wire-contract fixtures.
- **FR-013**: A sim twin MUST show that resolving a review thread is seen by the engine, and acted on by the feedback gate, with no refetch.
- **FR-014**: The webhook documentation (supported events, and what each event feeds) MUST be updated, including a note that an existing org/repo webhook or Hookdeck source must be extended with the new events to benefit.

### Key Entities *(if applicable)*

- **Review thread**: A PR review thread identified by `thread.node_id`, with a resolved flag and its comments; read by the feedback gate.
- **Sub-issue link**: A parent/child relationship between two issues, delivered as two mirrored events per change.
- **Label / milestone**: Repository-level objects whose rename or edit must fan out to the cached items that carry them.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a thread is resolved or unresolved on GitHub, the engine's feedback verdict for the item reflects it on the next poll with zero additional GitHub reads, shown by the sim twin.
- **SC-002**: A deleted comment no longer counts as pending feedback, and an edited comment's cached body equals the new text, with no refetch, for both issue comments and review comments.
- **SC-003**: A label rename or deletion and a milestone edit or deletion update all affected cached items in one application, with no per-item GitHub read.
- **SC-004**: Delivering the parent-side and child-side sub-issue events in either order, or twice, leaves exactly one link.
- **SC-005**: Every new event type has at least one delta test fed by a captured payload, and the full existing suite (including sim and wire-contract tests) passes unchanged.

## Assumptions

- Sub-issue links are not a dispatch fact today (no engine code reads them), so they are recorded as an informational fact on the Store and do not wake the poll; if a consumer later needs them, wake semantics are decided then.
- `projects_v2` is not subscribed. The capture shows it carries no Status option renames, and no project-level fact currently used by the engine justifies it. The existing reconcile and Status-field lookup keep handling renamed options. This overrides the original R1 wording for `projects_v2`.
- A comment edit refreshes the cached body but does not reset the 🚀 processed state; re-processing an edited comment is a separate behavior change and not part of this issue.
- Label and milestone fan-out is limited to items already in the Store, matched by repository and the old name (`changes.*.from`) or by ID/number; items not cached are corrected by their next fetch.
- Operators extend existing webhook configurations themselves; the engine does not edit an already-created org/repo hook beyond what it does today for event lists.
- The captured fixtures from PR #2401 are the payload shapes of record; if that PR has not merged when implementation starts, they are taken from its branch.

## Out of Scope *(optional)*

- `issue_dependencies` (covered by #1995).
- `projects_v2` Status option renames (not delivered by GitHub, per the capture).
- Making a sub-issue link a dispatch input or adding new behavior driven by it.
- Re-processing edited comments, or changing how processed state is tracked.
- The second direct webhook source and its delivery dedupe, serial apply queue and signature handling (#2143).
- Changes to Hookdeck reconnect/catch-up behavior.

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md` §5 (what a second webhook source needs).
- PR #2401 captured deliveries (`boardcache/testdata/webhooks/`), branch `fixtures/webhook-captures-2352`.
- Related: #1995, #2143, #2351, #1945, ADR 2348 (observed-at ordering), ADR 2180 (cache freshness).
