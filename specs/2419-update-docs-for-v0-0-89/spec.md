# Feature Specification: Update docs for v0.0.89

**Feature Branch**: `fabrik/issue-2419`
**Created**: 2026-10-21
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.89.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.89

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Every release is followed by a documentation pass so that operators reading the user guide, the README and the marketing page see the behavior of the version they are running. v0.0.89 ("The engine stops looping on things it cannot finish") has three themes:

1. **No looping on unfinishable work** (#2388 family): no worker is started when the `fabrik:editing` lock label cannot be written (#2393); all new model work is held while GitHub is unreachable (#2393); a finished stage is recorded and its output published by retry instead of re-running the stage (#2394); and one stage of one item is dispatched at most `max_stage_dispatches` times (default 25, `0` disables) before the item is paused (#2395).
2. **State that holds up under webhooks, restarts and concurrent writes**: one observed-at ordering rule means an older read never overwrites a newer fact (#2348, #2350); merge-train caps and the usage-limit hold survive a restart (#2349); all webhook transports share one intake with persisted delivery dedupe (#2351); more webhook events are applied directly — `issue_dependencies`, `sub_issues`, `pull_request_review_thread`, `label`, `milestone`, comment edits and deletes (#1995, #2352); `projects_v2_item` is decoded from `node_id` (#2383).
3. **New overseer action**: `fabrik_set_blocked_by` adds or removes blocked-by edges from an MCP session, guarded against self-blocks, PR blockers and cycles (#2385).

Smaller fixes: the board cache never serves an empty project ID and a cache started on an empty board bootstraps on a later poll (#2396, #2411); a forced Validate re-run is completed only by a Validate stage run, via the `fabrik:validate-rerun` marker (#2378); a usage-limit refusal of a reinvoke or comment review waits until the hold lifts (#2407); Hookdeck reconnects log their cause (#2384); the self-write freshness baseline uses GitHub's timestamps (#1945); Pruefer removes the empty `memory/` directory the Claude CLI leaves in a review's profile folder (#2406).

Pull-request branches for the v0.0.89 work already updated parts of `docs/USER_GUIDE.md` and `docs/state-machine.md`. The user guide already documents `max_stage_dispatches`/`--max-stage-dispatches`, `fabrik_set_blocked_by`, the forge-unreachable hold and its `forge_unreachable` warning, the persisted `engine-decisions` state, the new webhook events and `issue_dependencies`. A search of the guide found no mention of the recorded stage outcome and its publication retry, the `fabrik:validate-rerun` marker, or the ordering/live-read rule. The README ends its "highlights" series at v0.0.88 and `docs/index.md` carries a "New in v0.0.88" note. So this issue is mostly a **gap-fill and release-surfacing** pass, not a rewrite. The README and marketing page are the main surfaces that still describe v0.0.88 as current.

Precedent: the previous docs pass (#2381, "Documentation updated for v0.0.88") added a README "vX highlights" paragraph, updated the `docs/index.md` "New in" note, extended the user guide, and regenerated the llms bundle.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator upgrading to v0.0.89 learns what changed and what to do (Priority: P1)

An operator running v0.0.88 reads the README and the user guide before upgrading. They need to know the one new default that can change behavior (an item is paused after 25 dispatches of one stage) and the one manual step that can leave them missing events (webhooks they created themselves keep their own event list).

**Why this priority**: These are the only two behavior changes an operator can act on. Getting them wrong causes surprise pauses or silently missing webhook events.

**Independent Test**: Read only the README v0.0.89 highlights paragraph and the linked user-guide sections. Confirm that the dispatch bound (default, how to raise or disable it, refusal of invalid values) and the hand-created-webhook event list (`sub_issues`, `pull_request_review_thread`, `label`, `milestone`) are both stated.

**Acceptance Scenarios**:

1. **Given** the README, **When** an operator looks for the latest release, **Then** a v0.0.89 highlights entry follows the v0.0.88 entry in the same style and links to the user-guide sections for the new behavior.
2. **Given** the user guide, **When** an operator searches for how to avoid the new pause, **Then** the `max_stage_dispatches` flag/env/YAML forms, default `25`, `0` to disable, and startup refusal of negative or non-integer values are documented.
3. **Given** an operator with a self-managed organisation/repository hook, GitHub App or Hookdeck source, **When** they read the webhook section, **Then** they are told which four events to add to receive the new facts, and that without them Fabrik learns those facts from normal fetches.

---

### User Story 2 - An operator understands why a finished stage is no longer re-run (Priority: P1)

An operator sees GitHub writes failing and wants to know what Fabrik does with a stage that finished but whose output could not be posted. They need to know that the result is recorded, published later by retry, that it counts toward `max_retries` unless the failure is a transport failure, and what happens at the bound.

**Why this priority**: This is the headline fix of the release (#2388) and is not in the user guide at all.

**Independent Test**: Search the user guide for the recorded-outcome behavior and confirm that it describes record, retry, the bound, the restart behavior, and the cases in which a record is discarded.

**Acceptance Scenarios**:

1. **Given** a stage that completed but whose comment or push failed, **When** the operator reads the guide, **Then** it explains that the stage is not re-run and the output is published by a later retry.
2. **Given** a forced Validate re-run (`fabrik:revalidate` or a SHA invalidation), **When** the operator reads the guide, **Then** it explains that such a record is never used to complete the re-run and that `fabrik:validate-rerun` is an engine marker not to be removed by hand.
3. **Given** the Fabrik-Managed Labels table, **When** the operator looks up `fabrik:validate-rerun`, **Then** it has a row.

---

### User Story 3 - A reader of the marketing page sees the current release (Priority: P2)

A prospective user opens the project site. The "New in" note should describe v0.0.89, not v0.0.88, in the same tone and length as before.

**Why this priority**: Visible but low-risk; the content already exists in the README once written.

**Independent Test**: Open `docs/index.md`; the "New in" note names v0.0.89 and links to a user-guide anchor that exists.

**Acceptance Scenarios**:

1. **Given** `docs/index.md`, **When** it is read, **Then** the note says "New in v0.0.89", summarizes the release in one or two sentences, and its link target resolves to an existing heading.

---

### User Story 4 - A reader trusts the documented behavior of restarts, ordering and bootstrap (Priority: P3)

A reader wants to know the observable consequences of the state work: restarts keep merge-train caps and the usage-limit hold; an older read never overwrites a newer fact; gate decisions use live reads; a cache started on an empty board recovers on a later poll; a usage-limit refusal of a reinvoke waits for the hold to lift.

**Why this priority**: Mostly internal; the operator-visible consequences deserve a sentence each, but the engine-level specification lives in `docs/state-machine.md`.

**Independent Test**: For each consequence, confirm that the user guide states it in operator terms (no internal function names) or that the audit recorded it as already documented.

**Acceptance Scenarios**:

1. **Given** the user guide, **When** the reader looks up usage-limit behavior, **Then** it states both that the hold survives a restart and that refused reinvokes/comment reviews wait for the hold to lift.
2. **Given** the user guide, **When** the reader looks up webhook handling, **Then** it states that deliveries are de-duplicated across transports and that stale information never overwrites newer information.

---

### Edge Cases

- A feature is already documented (most of the webhook, dispatch-bound, forge-hold and overseer items): the pass must not duplicate it; it only verifies accuracy against the release notes and fixes drift.
- Internal-only items (sim twins, e2e gate coverage, plugin pinning) are not user-facing and get no mention.
- Anchors: any link added from the README or `docs/index.md` must target a heading that exists in `docs/USER_GUIDE.md`; renamed or added headings must not break existing links.
- Documentation of v0.0.89 behavior must describe only behavior released in v0.0.89; wording must not claim behavior that landed after the tag.
- The canonical documents (`USER_GUIDE.md`, `state-machine.md`, `stage-lifecycle.md`, `positioning.md`) and the generated `docs/llms-full.txt` must agree at commit time.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The documentation set MUST be audited against every user-facing item in the v0.0.89 release notes (Features, Fixes, Improvements, Upgrading), recording for each whether it is already documented, needs a new passage, or needs a correction.
- **FR-002**: `docs/USER_GUIDE.md` MUST describe the recorded stage outcome and its publication retry in operator terms: a finished stage is recorded before its first GitHub write, its output is published by retry rather than by re-running the stage, failures count toward `max_retries` unless they are transport failures, the item is paused at the bound, and an unusable or stale record is discarded rather than published.
- **FR-003**: `docs/USER_GUIDE.md` MUST document the `fabrik:validate-rerun` label in the Fabrik-Managed Labels table and explain that a forced Validate re-run is completed only by a Validate stage run, not by a comment-review pass or a catch-up landing.
- **FR-004**: `docs/USER_GUIDE.md` MUST state the operator-visible effects of the ordering and live-read changes (an older read never overwrites a newer fact; gates decide from fresh live reads; dispatch re-reads a candidate before starting work) without exposing internal identifiers.
- **FR-005**: `docs/USER_GUIDE.md` MUST state that a usage-limit refusal of a reinvoke or comment review waits until the hold lifts, alongside the existing statement that the hold survives a restart.
- **FR-006**: `docs/USER_GUIDE.md` MUST state that a board cache started on an empty board bootstraps on a later poll and that board writes defer while the project ID is unknown, in the existing board-fetch-resilience passage or an equivalent place.
- **FR-007**: The documentation MUST carry the two upgrade notes from the release: the `max_stage_dispatches` bound (flag, env var, YAML key, default, disable value, startup refusal) and the need to add `sub_issues`, `pull_request_review_thread`, `label` and `milestone` to self-managed webhook sources.
- **FR-008**: Existing v0.0.89 documentation (`max_stage_dispatches`, `fabrik_set_blocked_by`, forge-unreachable hold, persisted engine decisions, new webhook events, `issue_dependencies`, the shared intake and its delivery dedupe, Hookdeck reconnect-cause logging, `projects_v2_item` decoding, Pruefer's `memory/` cleanup) MUST be verified against the release notes and corrected if inaccurate; the Pruefer and Hookdeck items MUST be added if absent.
- **FR-009**: `README.md` MUST gain a "v0.0.89 highlights" entry after the v0.0.88 entry, in the same style, summarizing the three themes, stating the two upgrade actions, and linking to existing user-guide anchors.
- **FR-010**: `docs/index.md` MUST replace its "New in v0.0.88" note with a v0.0.89 note of similar length whose link resolves.
- **FR-011**: Configuration references and code examples touched by the pass MUST match the flags, environment variables and YAML keys shipped in v0.0.89.
- **FR-012**: If `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md` need correction found during the audit, they MUST be updated in the same change.
- **FR-013**: `docs/llms-full.txt` MUST be regenerated (`bash scripts/generate-llms-full.sh`) and committed in the same commit as any canonical-doc edit, so the `docs-drift` check passes.
- **FR-014**: The change MUST be documentation-only: no Go source, stage YAML, plugin or test changes.

### Key Entities *(if applicable)*

- **Documentation surfaces**: `docs/USER_GUIDE.md` (canonical operator reference), `README.md` (release highlights), `docs/index.md` (marketing page), `docs/llms-full.txt` (generated bundle).
- **Release notes**: the v0.0.89 GitHub release, the source of truth for what shipped.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of user-facing items in the v0.0.89 release notes are accounted for in the audit as documented, added, or intentionally omitted (internal-only).
- **SC-002**: A search of `docs/USER_GUIDE.md` finds a passage for each of: recorded stage outcome/publication retry, `fabrik:validate-rerun`, usage-limit refusal waiting, and empty-board bootstrap; none of these returned a hit before the change.
- **SC-003**: The README and `docs/index.md` both name v0.0.89 as the latest release and every link they add resolves to an existing heading.
- **SC-004**: The `docs-drift` workflow passes on the PR (the committed `docs/llms-full.txt` equals the regenerated output) and the full test suite still passes with no Go changes.
- **SC-005**: Both upgrade actions (dispatch bound, hand-created webhook event lists) are findable from the README entry in one click.

## Assumptions

- The v0.0.89 release notes are accurate and complete; where they conflict with the code, the shipped behavior described in `docs/state-machine.md` is used to resolve the wording.
- Existing user-guide coverage added by the v0.0.89 feature PRs is accurate unless the audit shows otherwise; the pass does not re-document it.
- The README highlights entry stays a single paragraph in the style of the v0.0.84–v0.0.88 entries rather than a new section.
- Internal-only changes (sim twins, e2e gate coverage, plugin pinning, test fixtures) are intentionally not documented.
- Operator-facing wording avoids internal function and file names; detailed mechanics belong in `docs/state-machine.md`, which already carries the as-built specification.

## Out of Scope

- Any change to engine, Pruefer, plugin, stage YAML or test code.
- Rewriting or restructuring documentation that is already accurate.
- New blog posts, competitive or design documents, and changes to `docs/specs/`.
- Documenting behavior from commits after the v0.0.89 tag.
- Creating ADRs.

## Source References

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.89
- Precedent docs pass: #2381 (v0.0.88)
- Related issues named in the notes: #1945, #1995, #2348–#2352, #2378, #2383–#2385, #2393–#2396, #2406, #2407, #2411
