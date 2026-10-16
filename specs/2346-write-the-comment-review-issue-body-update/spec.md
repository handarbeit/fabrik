# Feature Specification: Write the comment-review issue body update through to the Store

**Feature Branch**: `fabrik/issue-2346`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "## Problem

A comment-review spec update is masked, so the next stage reads the old spec. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.7.

`processCommentsClassified` updates the issue body (engine/comments.go:1002) with no Store write-through, then applies `SelfWriteObserved`, which moves the freshness baseline past GitHub's edit time. The cached `Body` stays the pre-edit text; the next cache hit copies it, and `writeContextFiles` writes it to `.fabrik-context/issue.md` (engine/context.go:98). The code comment (\"issue body is not read from cache for dispatch decisions\") is wrong for this use.

With working webhooks the `issues.edited` echo applies the body and hides the bug. On a poll-only daemon (the dev daemon today), Specify/Research Q&A rounds feed the next invocation the old spec until something else moves the issue layer. The stage-output body edit (engine/item.go:1563) does not call `SelfWriteObserved` and self-heals with one refetch.

## Requirements

- **R1:** Every engine write of an issue body applies the new body to the Store (a write-through mutation) in the same step that advances the baseline.
- **R2:** The stage-output body edit and the comment-review body edit use the same helper.
- **R3:** The misleading comment is corrected.

## Acceptance

- A unit test: after a comment-review body update and with no webhook, the next `writeContextFiles` writes the new body.
- A sim twin on a poll-only engine.
- Non-vacuity: without the write-through the test fails."

## Background

The engine state audit of 2026-10-15 (§1.7) found that a spec update made during comment review is hidden from the next stage. When the engine updates the issue body in response to a comment, it does not update its own cached copy of the body. It does move the cache's freshness baseline past GitHub's edit time, so the cache now treats the stale body as current. The next cache hit then serves the pre-edit text, and the engine writes that text into the issue context file the next stage reads as its spec.

Whether the bug shows depends on how the daemon receives GitHub updates. With working webhooks, GitHub's `issues.edited` echo applies the new body and hides the problem. On a poll-only daemon (today's dev daemon), nothing corrects the cache until some other change touches the issue. Specify and Research Q&A rounds therefore feed the next invocation the old spec, so the user's answers appear to have been lost.

The other engine body write, the stage-output body edit, does not advance the baseline. It self-heals after one refetch, so it is not currently harmful. It is still a second, different way of writing the same thing. The code comment that justifies the omission ("issue body is not read from cache for dispatch decisions") is wrong, because the body is read from cache to build the stage context.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A comment-review spec update reaches the next invocation (Priority: P1)

An operator answers a Specify or Research question in a comment. The comment-review pass rewrites the issue body with the answer folded in. The next invocation of the stage, or of the following one, must read the rewritten spec, even when the daemon receives no webhooks.

**Why this priority**: This is the reported defect. Stale input silently discards the user's answers and makes the pipeline work from the wrong spec.

**Independent Test**: On an engine with no webhook delivery, run a comment-review pass that updates the issue body, then build the context files for the next invocation. The issue context file must contain the new body.

**Acceptance Scenarios**:

1. **Given** a poll-only daemon and an issue whose body is cached, **When** a comment-review pass updates the issue body and a later invocation's context files are written, **Then** the issue context file contains the updated body, not the pre-edit body.
2. **Given** the same setup, **When** the cache is then consulted for the issue, **Then** the cached body equals the body the engine wrote, and the freshness baseline reflects the edit.

---

### User Story 2 - Every engine issue-body write behaves the same way (Priority: P2)

Any place the engine writes an issue body, including the stage-output body edit and the comment-review body edit, goes through one shared step. That step applies the new body to the Store and advances the freshness baseline together. A future body-write site cannot repeat the bug by omission.

**Why this priority**: It removes the divergence that caused the bug, but the user-visible fix is Story 1.

**Independent Test**: Make a stage-output body edit and a comment-review body edit on separate runs. After each, the cached body equals the written body, with no refetch required.

**Acceptance Scenarios**:

1. **Given** a stage whose output updates the issue body, **When** the engine writes the body, **Then** the Store holds the new body immediately and the baseline advances in the same step.
2. **Given** a comment-review pass that updates the issue body, **When** the engine writes the body, **Then** it uses the same shared step as the stage-output edit.

---

### User Story 3 - The code comment is accurate (Priority: P3)

The comment that claims the issue body is not read from cache for dispatch decisions is corrected, so a maintainer is not misled into skipping the write-through again.

**Why this priority**: Documentation hygiene; it prevents regression of the same reasoning error.

**Independent Test**: Review the changed code: no comment claims the body is unused from cache, and any comment on the body write states that the body feeds the context files.

**Acceptance Scenarios**:

1. **Given** the corrected code, **When** a maintainer reads the comment at the body-write site, **Then** it does not state that the cached body is unused for dispatch.

---

### Edge Cases

- The webhook echo (`issues.edited`) arrives after the write-through: applying the same body again must be a harmless no-op, and no spurious change event should result.
- The body write to GitHub fails: the Store must not be updated with a body GitHub never accepted, and the baseline must not advance.
- The issue is not yet in the Store when the body is written: the write must not fail or create a partial record.
- A webhook echo or reconcile listing that predates the edit arrives after it: it must not revert the cached body to the old text.
- The new body is identical to the cached body: no change should be signalled.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every engine write of an issue body applies the new body to the Store in the same step that advances the freshness baseline, so the two cannot diverge.
- **FR-002**: The stage-output body edit and the comment-review body edit use the same shared step.
- **FR-003**: After a comment-review body update, with no webhook delivered, the next context-file write for that issue contains the new body.
- **FR-004**: The Store is updated only when the body write to GitHub succeeds; a failed write leaves both the cached body and the baseline unchanged.
- **FR-005**: The misleading code comment that says the issue body is not read from cache for dispatch decisions is corrected.
- **FR-006**: A unit test shows that after a comment-review body update on an engine with no webhook, the next context-file write holds the new body.
- **FR-007**: A sim scenario on a poll-only engine shows the same behaviour end to end across a comment-review round and the next stage invocation.
- **FR-008**: Both tests fail when the write-through is removed (non-vacuity), and this is demonstrated.

### Key Entities *(if applicable)*

- **Issue body**: The spec text of the issue. The engine writes it to GitHub, and a copy is cached in the Store and fed to stages as context.
- **Freshness baseline**: The point the cache uses to decide whether its copy is still current relative to GitHub's last edit time.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a poll-only daemon, 100% of comment-review body updates are visible in the next invocation's issue context file, with no intervening refetch or webhook.
- **SC-002**: Every engine site that writes an issue body is covered by the shared step, with zero write sites that advance the baseline without applying the body to the Store.
- **SC-003**: The new unit test and sim scenario pass with the fix and fail with it removed.
- **SC-004**: The full existing test suite, including the webhook-delivered paths, passes unchanged.

## Assumptions

- "Poll-only" means a daemon that receives no webhook deliveries and relies on polling and reconcile for GitHub updates.
- The stage-output body edit currently self-heals, but moving it onto the shared step is wanted for consistency (the issue's R2), not because of a user-visible defect.
- Applying the same body twice (write-through, then webhook echo) is idempotent and produces no extra user-visible effects.
- The audit document `docs/design/engine-state-audit-2026-10.md` is the authority for the finding; this issue does not re-audit other write paths beyond issue-body writes.

## Out of Scope *(optional)*

- Other findings from the engine state audit; these are handled in their own issues.
- Changing how webhooks, reconcile or the freshness rule work in general.
- Fixing staleness of anything other than the issue body, such as labels, comments or PR state.
- Redesigning how context files are built.

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md`, §1.7 (engine state audit, 2026-10-15)
- `engine/comments.go` (comment-review body update, `processCommentsClassified`)
- `engine/item.go` (stage-output body edit)
- `engine/context.go` (`writeContextFiles`, issue context file)
