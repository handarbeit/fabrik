# Feature Specification: Update docs for v0.0.88

**Feature Branch**: `fabrik/issue-2381`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.88.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.88

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Each release is followed by a documentation pass so the User Guide, README and marketing page describe what the shipped binary does. v0.0.88 is a large release with two themes. The v0.0.88 release notes are the source of truth for this pass.

**Pruefer reviews through plugins and reads the whole PR.**
- The default review is an embedded plugin skill, `pruefer-review:review` (#2306).
- Operators can load their own review plugins and select one by `plugin:skill`. When the selected skill fails to load or never runs, Pruefer falls back to the default, and the posted review shows a `Review skill fallback` notice (#2307).
- A repo can supply plugins through its standard Claude Code plugin declarations, but only behind three opt-ins that default off (#2366): the operator's `repo_plugins`, the repo naming the plugin in its base-ref `.pruefer/config.yaml` `plugins:` key, and a PR targeting the default branch. Plugins from another repository also need a `repo_plugin_marketplaces` allowlist entry and a pinned commit. Plugin hooks and MCP servers run as the daemon's OS user.
- Review tools are coded within an operator ceiling (`review_tools`), validated at startup, and outside authors get a read-only review (#2308, hardened by #2370).
- A **coverage resume loop** (#2375) resumes the same review session until every staged diff entry has been read, bounded by `max_coverage_passes` (default 2, `0` disables). Each extra pass is another resumed invocation against the operator's Claude quota. The posted coverage block is the union of every accepted pass.
- The reviewer reads staged entries one at a time, or in batches of at most 20 KiB, so the Claude CLI does not cut a result to a 2 KB preview. Entries seen only as a preview count as unread (#2362).
- Review sessions no longer live in the Claude profile. Transcripts are written to `.pruefer/transcripts/` and kept 7 days (#2363).
- When `refs/pull/<N>/head` is missing, Pruefer fetches the head commit by its SHA (#2331).

**Engine state fixes from the 2026-10-15 state audit (#2341–#2347).**
- `fabrik:paused` wins at every automatic advance and landing site, on the stage path and in the merge train. An item whose labels cannot be read is refused like a paused one (#2341).
- Every engine merge is pinned to the head the gates judged. A head that moved defers the landing silently, and only the head Validate verified can land (#2342).
- The Validate completion SHA is rebuilt after a restart, so a push after a restart still invalidates Validate. If it cannot be recovered, Validate re-runs and the item is never landed unverified (#2343).
- Comment-review issue-body writes reach the cache, so a poll-only daemon no longer feeds the next stage the pre-edit spec (#2346).
- Three cache-write paths no longer overwrite newer state with older data (#2344, #2347).
- `projects_v2_item` webhooks are filtered by project and Status field (#2345).

A first scan shows part of this is already documented, because several feature PRs updated the docs as they landed: `repo_plugins`, `repo_plugin_marketplaces`, `review_tools` and its hardening, `review_skill`/`repo_review_skill`, the transcript location and retention settings, the "Fabrik only merges the commit it judged" paragraph, and `projects_v2_item` filtering. This pass is therefore an audit-and-fill, not a rewrite. Items the scan found missing from the User Guide include the following; the Research stage confirms the exact list:
- `max_coverage_passes` and the coverage resume loop.
- The pause guard as a user-visible guarantee.
- The Validate-completion-SHA rebuild after a restart (the guide currently mentions only "a narrow restart window").
- The `Review skill fallback` and read-only-review notices a reader will see in a posted review.
- The staged-entry batch limit.
- The head-SHA fallback.

The README's version highlights and the marketing page's "New in" line stop at v0.0.87.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Pruefer operator upgrades to v0.0.88 (Priority: P1)

An operator running Pruefer upgrades and needs to know what changed, what could stop Pruefer from starting, and what the new loop costs.

**Why this priority**: The release makes breaking and cost-bearing changes for Pruefer operators: `review_tools` rules are validated at startup and a refused rule stops Pruefer, repo plugins are off by default, and every coverage pass spends Claude quota. Missing docs here cause a failed start or a surprise bill.

**Independent Test**: Read only the Pruefer sections of the User Guide and the README highlight. A reader can state:
- what to do if `review_tools` contains a refused rule;
- how to enable repo plugins and what that trusts;
- what `max_coverage_passes` does, its default, and how to turn it off;
- where transcripts live and how long they are kept.

**Acceptance Scenarios**:

1. **Given** the Pruefer section of the User Guide, **When** a reader looks for the coverage loop, **Then** it explains that Pruefer resumes the review session while staged entries are unread, names `max_coverage_passes` (flag, environment variable and YAML key as the other Pruefer settings are given), its default of 2 and that `0` disables it, and states that each extra pass costs quota.
2. **Given** the same section, **When** a reader looks for what a posted review can contain, **Then** it describes the `Review skill fallback` notice and the read-only-review notice for outside authors, and says what triggers each.
3. **Given** the guide's description of the reviewer's reading rules, **When** a reader wonders why a review reports unread entries, **Then** it explains the batch limit and that an entry seen only as a preview counts as unread.
4. **Given** the `review_tools` documentation, **When** an operator's config holds a refused rule, **Then** the guide tells them Pruefer will not start and to remove or narrow the rule.

---

### User Story 2 - Fabrik operator relies on the engine's new safety guarantees (Priority: P1)

An operator wants to know what Fabrik now guarantees about paused items, merged heads and restarts, so they can trust or adjust their automation.

**Why this priority**: These are behavior guarantees that change what an operator should expect: pausing now stops advancement and landing everywhere, and a restart no longer weakens Validate verification.

**Independent Test**: Read the pause, landing and Validate sections. A reader can state:
- that a paused item is never advanced or landed automatically, including in the merge train;
- that a landing waits quietly if the PR head moved;
- that a restart does not let an unverified head land.

**Acceptance Scenarios**:

1. **Given** the documentation of `fabrik:paused`, **When** a reader asks whether a paused item can still advance or land, **Then** it says no, on the stage path and in the merge train, and that an item whose labels cannot be read is refused as if paused.
2. **Given** the Validate documentation, **When** a reader asks what a restart does to a completed Validate, **Then** it says the completion SHA is recovered from Fabrik's own Validate comment, a later push still re-runs Validate, and an unrecoverable SHA re-runs Validate instead of landing.
3. **Given** the existing "only merges the commit it judged" paragraph, **When** compared with the release notes, **Then** it covers every pinned path (merge-queue enqueue, auto-merge enable, direct-merge fallback, merge-train landing) and the "only the head Validate verified lands" rule, with no contradiction.

---

### User Story 3 - A newcomer reads the README or marketing page (Priority: P2)

Someone reads the README or the site and should see v0.0.88's headline themes and know whether upgrading needs action.

**Why this priority**: The README highlights and the site's "New in" line are the project's public changelog for users who do not read the full guide. This is a visibility requirement, below correctness in the guide.

**Independent Test**: Open the README and `docs/index.md`. Each contains a v0.0.88 entry in the same form as the earlier ones, with working links into the User Guide.

**Acceptance Scenarios**:

1. **Given** the README's version highlights, **When** read after this pass, **Then** a v0.0.88 entry follows v0.0.87 in the same format, covering the two themes in a few sentences with anchor links into the guide. It states what upgrade action Pruefer operators must take (the `review_tools` startup validation, repo plugins off by default) and that Fabrik engine operators need none.
2. **Given** `docs/index.md`, **When** read after this pass, **Then** its "New in" note reflects v0.0.88 and links to a section that exists.

---

### User Story 4 - Docs stay consistent and the bundle stays in sync (Priority: P1)

A maintainer merges the pass and CI must stay green, and the docs must not describe superseded behavior.

**Why this priority**: The docs-drift workflow fails a PR whose committed `docs/llms-full.txt` differs from the regenerated bundle, and stale statements (for example "the guide mentions a configuration that no longer exists") are the problem this pass exists to fix.

**Independent Test**: Run `bash scripts/generate-llms-full.sh` on the final tree; `git diff` shows no change to `docs/llms-full.txt`. Every configuration key, flag and environment variable named in the edited docs exists in the shipped binary.

**Acceptance Scenarios**:

1. **Given** any edit to `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md`, **When** the change is committed, **Then** `docs/llms-full.txt` is regenerated in the same commit.
2. **Given** each new or changed setting in the docs, **When** checked against the code, **Then** its name, default and accepted values match.
3. **Given** every in-repo anchor link added in the README or site, **When** followed, **Then** it lands on a heading that exists.

---

### Edge Cases

- A feature in the release notes is already documented well. Leave it unchanged rather than duplicating it.
- A statement already in the docs contradicts the release (for example, text saying repo-supplied plugins are never loaded, or that review sessions are kept in the Claude profile). Correct it everywhere it appears (User Guide, README, `docs/state-machine.md` if affected).
- A setting is operator-only (not settable from a repo's config). The docs must not imply a repo can set it.
- The `projects_v2_item` filtering (#2345) was not exercised live in this release. Do not document it as live-verified.
- Internal-only items (release metadata, plugin pins, e2e gate coverage, `internal/skillref` move, design notes) need no user-facing documentation.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The User Guide documents the Pruefer coverage resume loop: when it engages (first pass has a complete output but staged entries are unread), that it resumes the same session, the `max_coverage_passes` setting with its default of 2, that `0` disables it and that negative or non-integer values are refused, that it is operator-only, and that each extra pass spends Claude quota.
- **FR-002**: The User Guide states that the posted coverage block is the union of every accepted pass, and explains the batch limit for staged entries (one entry per read, or batches of at most 20 KiB) and that an entry seen only as a preview counts as unread.
- **FR-003**: The User Guide describes the notices that can appear in a posted Pruefer review (`Review skill fallback`, read-only review for outside authors) and what causes each, and that a review whose skill errors or is never invoked is re-run once with the default skill, not posted broken.
- **FR-004**: The User Guide and README state that `review_tools` is validated at startup and that a refused rule stops Pruefer from starting, with the remedy (remove the rule or narrow it to a read-only command).
- **FR-005**: The User Guide documents that repo-supplied review plugins are off by default, the three opt-ins needed to enable them, the extra requirements for plugins from another repository, and the trust model (plugin hooks and MCP servers run as the daemon's OS user). Existing text is verified against the release notes and corrected where it differs.
- **FR-006**: The User Guide documents that review sessions are no longer kept in the Claude profile, that transcripts are written to `.pruefer/transcripts/` and kept 7 days by default, and the related retention settings. Existing text is verified and corrected where it differs.
- **FR-007**: The User Guide mentions the Pruefer head-SHA fallback: when `refs/pull/<N>/head` is missing, the head commit is fetched by its SHA.
- **FR-008**: The User Guide states that `fabrik:paused` wins over every automatic advance and landing decision, on the stage path and in the merge train, that an item whose labels cannot be read is treated as paused, and that a paused train member keeps the whole landing Queued without counting against the train.
- **FR-009**: The User Guide's merge-pinning documentation covers the merge-queue enqueue, auto-merge enable, direct-merge fallback and merge-train landing, a head that moved deferring the landing silently, and the rule that only the head Validate verified lands.
- **FR-010**: The User Guide documents that the Validate completion SHA survives a restart (recovered from Fabrik's own Validate comment), that a later push still re-runs Validate, and that an unrecoverable SHA re-runs Validate rather than landing unverified. It replaces or clarifies the current "narrow restart window" wording where that wording is no longer accurate.
- **FR-011**: The User Guide records, where it describes how a comment-review update to the issue body reaches the next stage, that a poll-only daemon sees the edited body. It says nothing more about #2344, #2346 and #2347 than what a user can observe (newer state is no longer overwritten by older data).
- **FR-012**: The `projects_v2_item` webhook filtering (project and Status field) is described accurately in the existing webhook documentation, and where it is mentioned it notes that it is covered by unit tests and recorded payloads only, not yet by a live delivery.
- **FR-013**: The README gains a v0.0.88 highlights entry in the format of the v0.0.84–v0.0.87 entries, with links into the User Guide, and states the upgrade actions for Pruefer operators and that none are needed for Fabrik engine operators.
- **FR-014**: `docs/index.md` is updated so its "New in" note reflects v0.0.88 and links to an existing section; other marketing copy is changed only where it is now inaccurate.
- **FR-015**: Every setting name, flag, environment variable, default and accepted value written or changed in the docs is verified against the shipped code or the release notes.
- **FR-016**: `docs/state-machine.md`, `docs/stage-lifecycle.md` and `docs/positioning.md` are edited only where they contradict v0.0.88 behavior or where an as-built behavior of this release is missing from them. The pause guard, pinned merges and Validate SHA rebuild already have as-built entries and are verified, not duplicated.
- **FR-017**: `docs/llms-full.txt` is regenerated with `bash scripts/generate-llms-full.sh` and committed in the same commit as any canonical-doc edit.

### Key Entities *(if applicable)*

- **Release notes (v0.0.88)**: the source of truth for what changed. The Research stage compares them with the shipped docs to produce the confirmed gap list.
- **Canonical docs**: `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`. Editing any of them requires regenerating `docs/llms-full.txt`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A reader of the User Guide alone can find, for every user-facing item in the v0.0.88 release notes (Engine and Pruefer sections plus Upgrading), at least one correct statement of the behavior. The count of items with no coverage is zero.
- **SC-002**: No statement in the README, User Guide or site contradicts the v0.0.88 release notes (for example, none says repo plugins are never loaded, or that review sessions live in the Claude profile).
- **SC-003**: Running `bash scripts/generate-llms-full.sh` on the final tree produces no diff against the committed `docs/llms-full.txt`, and the `docs-drift` check passes.
- **SC-004**: The README and `docs/index.md` each carry a v0.0.88 entry, and every anchor link added resolves to an existing heading.
- **SC-005**: Every setting, flag or environment variable named in an added or changed passage exists in the code with the stated default.

## Assumptions

- Documentation only: no Go code, stage YAML, plugin skill or test changes. A defect found in the code while checking a claim is filed as a separate issue, not fixed here.
- Behavior already documented accurately in earlier PRs is left as is. This pass adds what is missing and corrects what is wrong.
- The Pruefer README (the reference the User Guide points to) is edited only if it contradicts the release. The User Guide, README and site are the primary targets.
- Internal-only changes (release metadata, plugin pins, `internal/skillref` move, e2e gate coverage, design notes, ADRs) need no user-facing documentation.
- The README highlight follows the existing format and length of the v0.0.84–v0.0.87 entries; it does not restate the full release notes.
- No behavior or configuration changes are needed for Fabrik engine operators in v0.0.88; the upgrade actions apply to Pruefer operators only.

## Out of Scope *(optional)*

- Changing any shipped behavior, defaults or settings.
- Rewriting existing documentation that is already accurate.
- Documenting ADR-level design rationale in the User Guide (ADRs already record it).
- A blog post or release announcement beyond the README and `docs/index.md` entries.
- Live verification of the `projects_v2_item` filter with a real webhook delivery (follows the release).

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.88
- Previous documentation pass: #2338 (v0.0.87), spec at `specs/2338-update-docs-for-v0-0-87/spec.md`
- Release PRs and ADRs: #2306, #2307, #2308, #2331, #2341–#2347, #2362, #2363, #2366, #2370, #2375
