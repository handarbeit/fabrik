# Feature Specification: Update docs for v0.0.84

**Feature Branch**: `fabrik/issue-2256`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.84.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.84

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Every release is followed by a documentation pass so that what users read matches what the shipped binary does. v0.0.84 is a very large release. Its headline themes are:

- the MCP overseer: the local API, `fabrik mcp`, the read, action and subscribe tools, and push events;
- fully-qualified `plugin:skill` references, the `fabrik-stages` base plugin, `local-workflows` and `fabrik upgrade` migration;
- a reworked merge train: singleton catch-up, auto-repair, overlap-aware batching, CI-infrastructure handling, persisted runs, and a reserved slot;
- display-only board fields (`Fabrik`, `Last activity`, `Last run`) and the operator warnings panel;
- state and cache correctness fixes, GitHub-budget and admission fixes, and timeout fixes;
- safety changes: the per-board instance lock, the zero-ahead push guard, log rotation, the worker-environment scrub, and process reaping;
- Pruefer review hardening.

Most feature PRs in this release already updated `docs/USER_GUIDE.md`, `docs/state-machine.md` and `docs/stage-lifecycle.md` as they landed. A keyword audit of the release notes against the current docs shows the User Guide already covers the MCP tools and events, `plugin_dirs`, `local-workflows`, `persist_spec`, the merge-train keys, `project_fields`, `repair-board`, the worker-env passthrough, the instance lock, log rotation, the zero-ahead guard and the retry-admission change.

The audit also found gaps. The terms below have no hit in the User Guide, README or marketing page:

- `.fabrik-context/ci-status.md`;
- the Pruefer additions (`PRUEFER_COVERAGE`, `catch_up_marker_authors`, `PRUEFER_REVIEW_ENV_PASSTHROUGH`);
- the "Display fields missing" warnings-panel entry;
- the Specify carry-forward of human-added sections (`## Human Decisions`);
- the `entwurf@fabrik` marketplace removal;
- the `fabrik upgrade` "plugin already up to date" output.

The README and `docs/index.md` carry no v0.0.84 highlights. The README's "Big-board efficiency", "Anthropic auth namespace scrub" and GHES callouts stop at v0.0.78, and the marketing page mentions none of the overseer, merge-train or board-field work.

This issue is the documentation pass. It changes no engine behavior.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator upgrading to v0.0.84 can follow the docs (Priority: P1)

An operator running an existing project reads the User Guide before or after installing v0.0.84. They need accurate, ordered upgrade guidance, including the breaking or default-changing items.

**Why this priority**: Several changes can stop a daemon from starting or change default behavior (`actions: write` under App auth, worker-env scrub, merge-train defaults, `fabrik upgrade` migration, log files in `.gitignore`). Wrong or missing guidance causes outages.

**Independent Test**: Read the User Guide as an operator of a v0.0.83 install. Every item under the release notes' "Upgrading" section is findable, and its command, setting, flag or env name matches the shipped binary.

**Acceptance Scenarios**:

1. **Given** an App-auth project, **When** the operator reads the GitHub App section, **Then** `actions: write` is stated as required, with the two manual grant steps.
2. **Given** a project with a customised `.fabrik/plugin/`, **When** the operator reads the upgrade and plugin sections, **Then** they learn that `fabrik upgrade` moves edits to `.fabrik/plugins/local-workflows/`, that customisation belongs there from now on, and what `--no-migrate` and `--force` do.
3. **Given** a train-enabled project, **When** the operator reads the merge-train section, **Then** the changed defaults and their off-switches are stated: `singleton_catch_up: off`, `max_train_auto_repair_attempts: 0`, `merge_train_overlap_ignore`, and the reserved train slot.
4. **Given** a project whose skills or scripts read `FABRIK_*` variables, **When** the operator reads the worker-environment section, **Then** the six forwarded variables and `FABRIK_WORKER_ENV_PASSTHROUGH` are documented.

---

### User Story 2 - A reader finds documented v0.0.84 behavior that is currently missing (Priority: P1)

A user hits one of the behaviors the audit found undocumented and looks for it in the User Guide.

**Why this priority**: These are real shipped behaviors with no documentation, and closing the gap is the core deliverable.

**Independent Test**: For each gap in FR-004 through FR-009, a search of the User Guide finds a section that explains the behavior and its configuration or fix action.

**Acceptance Scenarios**:

1. **Given** a worker run on an item with an open PR, **When** the user looks up `.fabrik-context/ci-status.md`, **Then** the guide lists its fields, the five verdicts and how the shipped skills use it.
2. **Given** a Pruefer review body ending in `PRUEFER_COVERAGE: partial`, **When** the operator searches the guide, **Then** they learn that a partial review still clears Fabrik's review gate and should be checked.
3. **Given** the warnings panel shows "Display fields missing", **When** the operator searches the guide, **Then** it names `fabrik repair-board --apply` as the fix and `project_fields.* : off` as the opt-out.

---

### User Story 3 - A prospective user sees v0.0.84's headline capabilities in the README and marketing page (Priority: P2)

A newcomer reads the README feature list and the marketing page to decide whether Fabrik fits.

**Why this priority**: The README and marketing page are the public front door, but they only need the user-facing headlines, not exhaustive detail.

**Independent Test**: The README and `docs/index.md` mention the overseer MCP server, the qualified-skill and plugin model, the faster merge train, and the board display fields. Each mention links to the User Guide for detail, and each claim is true of the shipped release.

**Acceptance Scenarios**:

1. **Given** the README, **When** a reader scans the feature list and release callouts, **Then** the overseer (`fabrik mcp`), `plugin:skill` addressing and the display fields are described briefly and link to the User Guide.
2. **Given** `docs/index.md`, **When** a reader reads it, **Then** it reflects the headline capabilities only where the page's existing structure has a natural place for them, without bloating the page.

---

### User Story 4 - The documentation bundle stays consistent (Priority: P1)

A maintainer's PR must pass the `docs-drift` check.

**Why this priority**: CI fails the PR if `docs/llms-full.txt` differs from its regeneration.

**Independent Test**: Run `bash scripts/generate-llms-full.sh` after the final doc edit. `git diff --exit-code docs/llms-full.txt` is clean.

**Acceptance Scenarios**:

1. **Given** edits to any of `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md`, **When** the change is committed, **Then** the regenerated `docs/llms-full.txt` is in the same commit.

---

### Edge Cases

- A topic is already documented correctly. It is left alone. The pass fills gaps and fixes stale statements; it does not rewrite or duplicate existing sections.
- A documented name (flag, env var, config key, label, event name) differs from the shipped binary or release notes. The docs are corrected to the shipped name, using the code or the release notes as the source of truth.
- The release notes name a renamed term, such as "suspension" becoming "usage limited", `fabrik:fabrik-X` becoming `fabrik-stages:X`, or `entwurf@fabrik` removed. Any leftover docs references to the old term are updated or removed.
- The README or marketing page would need a claim the release notes only partly support, such as Pruefer review skip or review-cycle fields that did not ship. The claim is omitted or stated as the release notes state it.
- Nothing in a canonical doc changes. The bundle is then not regenerated, and no empty commit is made.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Audit `docs/USER_GUIDE.md`, `README.md`, `docs/index.md` and `docs/faq.md` against every section of the v0.0.84 release notes, and fix each place where a user-facing name, default, flag, env var, config key, label, event or procedure is missing or stale.
- **FR-002**: The User Guide's upgrade guidance matches the release notes' "Upgrading" section for v0.0.84. It covers the required `actions: write` grant with both manual steps, the ordered per-project procedure (stop the daemon, install, `fabrik upgrade`, review and commit, `repair-board` then `repair-board --apply`, restart, unpause), `--no-migrate` and `--force` semantics, and "customise only `local-workflows`".
- **FR-003**: The User Guide documents the "Other changes to check" items that are not already covered. These are the six forwarded worker variables, the merge-train default changes and their off-switches, spec persistence being opt-in for existing `specify.yaml` files (drift warning, `refresh-stages --apply`), the `.fabrik/fabrik.log.*` gitignore advice, the PM-plugin reinstall steps and double-server warning, and the removal of `entwurf@fabrik` from the marketplace.
- **FR-004**: The User Guide documents `.fabrik-context/ci-status.md`: when it is written, its fields (head SHA, verdict `green`/`red`/`pending`/`none`/`unknown`, `ci_gated`, `ci_gate_ahead`, the `## Failing checks` section for red), and how skills use it. They skip a local full suite on a green, clean head and reproduce only failing checks on red. On CI-gated stages they run targeted tests only.
- **FR-005**: The User Guide documents the Pruefer v0.0.84 additions. These are path-scoped read-only review tools and the scrubbed review environment with `PRUEFER_REVIEW_ENV_PASSTHROUGH`, the `PRUEFER_COVERAGE: complete | partial` line, with the note that a partial review still clears the gate, and `catch_up_marker_authors` (`--catch-up-marker-authors`, `PRUEFER_CATCH_UP_MARKER_AUTHORS`) for skipping reviews of pure merge-train catch-up pushes.
- **FR-006**: The User Guide documents the warnings-panel entries added this release that are not yet described. These are "Display fields missing" with its `repair-board --apply` fix action, and any other entry from #2135 and #2223 (for example `local_api:unavailable`) that the guide omits. It documents the dismissal and clearing behavior in the release notes.
- **FR-007**: The User Guide documents Specify's carry-forward of human-added, non-template sections (for example `## Human Decisions`) alongside its spec-persistence description, and the case-insensitive parsing of marker headers with errors that quote the offending line (#2091).
- **FR-008**: The User Guide's `fabrik upgrade` description includes its reporting behavior, in particular `fabrik upgrade: plugin already up to date` and that byte-identical files are skipped (#2219).
- **FR-009**: Any remaining User Guide statement that contradicts v0.0.84 is corrected. Examples are "suspension" wording where the release says "usage limited" (health fields, `claude-usage-limited` / `claude-usage-limit-lifted` events, TUI banner), pre-rename `fabrik:fabrik-X` skill names presented as current, and any statement that the `actions` permission is optional.
- **FR-010**: `README.md` gains concise, accurate v0.0.84 entries in the same style as its existing version callouts and feature list. They cover the overseer MCP server and PM plugin, qualified skill references with the `fabrik-stages` base plugin and `local-workflows` migration, the merge-train improvements, and the display-only board fields with the warnings panel. Each links to the relevant User Guide section, and each is brief.
- **FR-011**: `docs/index.md` is updated only where the page has a natural place for the headline v0.0.84 capabilities. If the existing page structure does not warrant a change, it is left as is, and the PR description says so.
- **FR-012**: Configuration examples and reference tables touched by the pass are current. They cover `plugin_dirs`, `project_fields.*`, `singleton_catch_up`, `max_train_auto_repair_attempts`, `merge_train_overlap_ignore`, `persist_spec` and the new threshold flags (`--stall-threshold`, `--retry-starved-threshold`, `--item-wedged-threshold`, `--cache-divergence-window`), each with its flag, env var and default as shipped.
- **FR-013**: If `docs/state-machine.md` or `docs/stage-lifecycle.md` is found to omit or misstate a v0.0.84 behavior, it is corrected in the same pass. These are as-built docs, so the code is the source of truth.
- **FR-014**: After the last edit to any canonical doc (`docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`), `docs/llms-full.txt` is regenerated with `bash scripts/generate-llms-full.sh` and committed in the same commit.
- **FR-015**: Every internal link and anchor added or changed resolves, and cross-references between the README, marketing page and User Guide point at existing headings.
- **FR-016**: The change is documentation only. No engine, plugin, stage-YAML or test code changes, apart from the regenerated bundle.

### Key Entities *(if applicable)*

- **Release notes (v0.0.84)**: The published notes at `https://github.com/handarbeit/fabrik/releases/tag/v0.0.84`. They are the checklist for what changed.
- **Canonical docs**: `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`. A change to any of them requires regenerating `docs/llms-full.txt`.
- **Public pages**: `README.md` and `docs/index.md`. They carry user-facing headlines only.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Each of the gaps named in FR-004 through FR-008 has a findable section in `docs/USER_GUIDE.md`, confirmed by a keyword search that previously returned zero hits.
- **SC-002**: Every flag, env var, config key, label and event name added to docs in this pass matches the shipped code or release notes. A spot check of all new names finds zero mismatches.
- **SC-003**: `README.md` and, where warranted, `docs/index.md` mention each of the four headline themes in FR-010, each linking to an existing User Guide anchor, and none of the claims is untrue of the release.
- **SC-004**: `bash scripts/generate-llms-full.sh` run on the final tree produces no diff against the committed `docs/llms-full.txt`, so the `docs-drift` workflow passes on first push.
- **SC-005**: No User Guide text remains that describes `actions: write` as optional, the pre-rename `fabrik:fabrik-X` skill names as current, or "suspension" in place of "usage limited".
- **SC-006**: The diff touches only documentation files plus the regenerated bundle.

## Assumptions

- The published v0.0.84 release notes are the authoritative scope list. The code on the issue branch (which is at the v0.0.84 release commit) is the source of truth when the notes and the docs disagree.
- Because the feature PRs already documented most of the release, this pass is an audit and gap-fill. It is not a rewrite, and existing correct text is left alone.
- The README and marketing page stay brief. Detail belongs in the User Guide.
- The README's existing version-callout style (a short blockquote with a link) is the format for new README entries.
- The blog, postmortems, competitive analysis, design and cache-refactor pages are historical or non-user-facing and are not touched.
- The `.claude` / `CLAUDE.md` project guidance is not a public doc and is not part of this pass. Only a factual error found there may be fixed in passing.

## Out of Scope *(optional)*

- Any engine, plugin, stage-YAML or test change.
- New ADRs.
- Rewriting existing, correct documentation, or restructuring the User Guide.
- Documentation for community-report threads (those are closed by hand after the release, per project policy).
- Blog posts or release-announcement copy.
- Features that did not ship in v0.0.84 (review-cycle or train-attempt board fields, member-branch conflict resolution inside multi-member trials, tracked in #2111).

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.84
- `CLAUDE.md` "Documentation bundle (docs/llms-full.txt)" section, and `scripts/generate-llms-full.sh`
- `.github/workflows/docs-drift.yml`
