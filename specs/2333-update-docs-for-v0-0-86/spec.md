# Feature Specification: Update docs for v0.0.86

**Feature Branch**: `fabrik/issue-2333`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.86.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.86

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

v0.0.86 is a bug-fix release with no new features. Its changes come from community reports, production incidents and the v0.0.85 gate. Together they change what operators see:

- The merge train no longer charges GitHub timing to members. A stale `dirty` trial is rebuilt, a transient `blocked` landing is waited out, a green trial is kept, and a pending-eject signal stays within its own run (#2298, #2297, #1557).
- A cancelled, stale, empty or unrecognised check-run conclusion is no longer a pass (#2296).
- Fabrik syncs an existing issue branch from origin before working on it, and every push uses a verified lease. A diverged branch pauses the item (#2295).
- `CLAUDE_CONFIG_DIR` is resolved from one ordered set of sources for Fabrik and Pruefer (#2293, #2294). A bare inherited value now ranks below `.env` and `config.yaml`.
- Two new pauses and one comment change: an unknown merge state pauses after 30 minutes (#2302), an ungrantable denied edit pauses on its first occurrence (#2301), and the CI-timeout pause names the gate that held the item (#2302).
- A Validate run that finishes after its PR merged takes the terminal path (#2299), and a comment-review run can block on input (#2280).
- Sessions: a time-sliced stage resumes its session again (#2292), and `resume_session` / `resume_threshold_tokens` are new stage options (#2300).
- Pruefer skips re-review of an unchanged patch (`patch_skip`, #2230) and keeps transcripts of partial-coverage reviews (`keep_transcripts`, #2303).
- A dev-build self-upgrade accepts only an exact `github.com/<owner>/<repo>` origin (#1209).

Most of these PRs updated the User Guide as part of their own work. This issue is the release-time pass that makes the documentation set consistent with v0.0.86. It follows the same pattern as the v0.0.85 pass (#2289). Every earlier release has a "highlights" note in the README and a "New in" blurb on the marketing page, and v0.0.86 has neither. A first audit found User Guide coverage for `resume_session`, `--claude-config-dir`, `--merge-state-unknown-timeout`, the singleton catch-up, cancelled checks, branch divergence and the base-branch rule for children. It found no User Guide mention of the ungrantable denied-edit pause, and nothing about the v0.0.86 behavior in the README or the marketing page. The audit is a starting point. The work includes verifying every release item rather than trusting it.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator upgrading to v0.0.86 learns what changed (Priority: P1)

An operator reads the README and User Guide before or after upgrading, and wants to know which behaviors changed, which new pauses they may see and what to do about each.

**Why this priority**: v0.0.86 changes behavior without changing configuration. An operator who is not told will meet a new pause or a re-ordered `CLAUDE_CONFIG_DIR` precedence with no explanation.

**Independent Test**: Take each item under the release notes' "Upgrading" heading. Find where the User Guide describes it, including the resume action for each pause. Confirm the README highlights note links to that place.

**Acceptance Scenarios**:

1. **Given** the README, **When** an operator looks for what is new, **Then** a v0.0.86 highlights note in the same style as the v0.0.85 one summarizes the upgrade-relevant changes and links to the User Guide sections.
2. **Given** the User Guide, **When** an operator whose run was stopped by a denied edit under `.claude/` looks up the pause, **Then** it explains that the first occurrence pauses the item and names the file, that it does not use `max_retries`, and the three options to resume.
3. **Given** an operator with a bare `CLAUDE_CONFIG_DIR` in their shell, **When** they read the profile section, **Then** the documented order matches the release notes (flag, shell `FABRIK_CLAUDE_CONFIG_DIR`, `.env`, `config.yaml`, bare inherited value).

---

### User Story 2 - A configuration reader finds current options and examples (Priority: P2)

A user configuring Fabrik or Pruefer wants flags, environment variables, `config.yaml` keys and stage options that are accurate and complete for v0.0.86.

**Why this priority**: A wrong or missing reference line costs a debugging session. Four of the options ship in this release.

**Independent Test**: For each v0.0.86 option, compare the documented name, default and meaning with the shipped source and release notes.

**Acceptance Scenarios**:

1. **Given** the options introduced or changed in v0.0.86 (`--merge-state-unknown-timeout`, `--claude-config-dir`, `claude_config_dir`, `resume_session`, `resume_threshold_tokens`), **When** the User Guide's tables, stage-option reference and examples are read, **Then** each has the right name, default and description.
2. **Given** the Pruefer options (`claude_config_dir`, `-claude-config-dir`, `PRUEFER_CLAUDE_CONFIG_DIR`, `patch_skip`, `keep_transcripts`), **When** the Pruefer reference is read, **Then** each is documented and, where the User Guide mentions Pruefer behavior, the two documents agree.

---

### User Story 3 - A prospective user sees a current marketing page (Priority: P3)

A visitor to the marketing page wants to see that the project is actively maintained and what recently changed.

**Why this priority**: v0.0.86 is a bug-fix release, so the page may reasonably need no new feature claim. A short note keeps the "New in" line from being stale.

**Independent Test**: Read the marketing page's release note against the latest release.

**Acceptance Scenarios**:

1. **Given** the marketing page, **When** it is read after this change, **Then** it either carries a brief v0.0.86 note linking to the relevant User Guide sections or is left unchanged on purpose, with no statement that contradicts v0.0.86 behavior.

---

### User Story 4 - The documentation bundle stays in sync (Priority: P1)

A maintainer or a downstream consumer of the combined documentation bundle expects it to match the canonical pages.

**Why this priority**: CI fails the PR if the bundle differs from its regeneration.

**Independent Test**: Regenerate the bundle and check there is no diff.

**Acceptance Scenarios**:

1. **Given** edits to any canonical documentation page, **When** the change is committed, **Then** the regenerated `docs/llms-full.txt` is in the same commit and the docs-drift check passes.

---

### Edge Cases

- **A release item already documented.** Leave it alone. Do not duplicate it or rewrite its wording just to touch it.
- **A release item that conflicts with the documentation.** For example, a statement written before the final behavior. Correct the documentation to the shipped behavior, and use the source and the ADR as tie-breakers.
- **Version labels in documentation.** Write "v0.0.86" only where a behavior is first available in it. Do not relabel older notes.
- **A bug-fix release with no new feature.** The README and marketing page must not describe a "new capability" that does not exist. They frame the changes as fixes and behavior changes.
- **The PR is opened from a branch that already contains the feature PRs' doc changes.** Edits are limited to genuine gaps and inconsistencies.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every item under "Upgrading" in the v0.0.86 release notes has a corresponding description in the User Guide, including how an operator resumes from each new pause.
- **FR-002**: The User Guide explains the ungrantable denied-edit pause (#2301): the trigger, that it happens on the first occurrence, that it names the file, that it does not count against `max_retries`, and the options for proceeding. It cross-references the existing tools-denied troubleshooting.
- **FR-003**: The User Guide's description of the `CLAUDE_CONFIG_DIR` precedence, the startup log line and the warning when sources disagree matches the release notes. Examples that set a bare `CLAUDE_CONFIG_DIR` and expect it to win are corrected.
- **FR-004**: Each option or flag added or changed in v0.0.86 appears with its correct name, default and meaning in the User Guide's reference tables and, for stage options, in the stage-config reference and example.
- **FR-005**: The User Guide describes the other operator-visible behavior from the release notes: the sync and verified-lease push and the diverged-branch pause; cancelled and non-passing check runs; the stale-trial rebuild and kept green trial; a Validate run that ends after its PR merged; comment-review runs that can ask for input; time-sliced session resume; and the dev self-upgrade remote requirement. Where a topic is already accurately covered, nothing is added.
- **FR-006**: The README gains a v0.0.86 highlights note in the style of the earlier ones. It lists the upgrade-relevant behavior changes, states that no configuration change is required, and links to the User Guide.
- **FR-007**: The Pruefer documentation covers the v0.0.86 Pruefer changes: shared Claude account resolution and its options, `patch_skip` and `keep_transcripts`. The User Guide's Pruefer notes do not contradict it.
- **FR-008**: The marketing page's "New in" content is reviewed against v0.0.86. A short note is added if it adds value. No claim on the page contradicts v0.0.86 behavior.
- **FR-009**: Code blocks, command examples and configuration snippets touched or affected by this release are valid for v0.0.86, and cross-references and anchors the edits add or change resolve.
- **FR-010**: If `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md` is edited, `docs/llms-full.txt` is regenerated with the repository's script and committed in the same change.
- **FR-011**: The change edits documentation only. No source code, stage configuration or behavior changes. State-machine and lifecycle documents are touched only to fix an inaccuracy found while auditing.

### Key Entities *(if applicable)*

- **Release notes (v0.0.86)**: the source list of changes and the "Upgrading" items to verify against.
- **Canonical documentation pages**: the User Guide, state machine, stage lifecycle and positioning pages, whose combined bundle must be regenerated on any edit.
- **README highlights note**: the per-release summary near the top of the README.
- **Marketing page**: the project's public landing page.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of the release notes' "Upgrading" items are traceable to a User Guide location that matches the shipped behavior, including every new pause's resume action.
- **SC-002**: The README contains a v0.0.86 highlights note and the marketing page has been checked against v0.0.86, with any resulting change recorded in the PR description.
- **SC-003**: Every option or flag introduced or changed in v0.0.86 appears in the relevant reference with the correct name and default.
- **SC-004**: The docs-drift check and the repository's existing documentation tests pass on the first push, with the bundle regenerated.
- **SC-005**: A reviewer finds no statement in the touched documents that contradicts the v0.0.86 release notes or shipped behavior.

## Assumptions

- The release notes at `v0.0.86` are the authoritative list of what changed, and the shipped source and ADRs break ties.
- Because v0.0.86 has no new features, the README note is framed as a behavior-changes and fixes summary, not a feature announcement.
- The marketing page may be left unchanged if a note would add nothing. The reviewer decides this from the content, and the choice is recorded in the PR description.
- Existing, accurate documentation from the feature PRs is kept as written.
- The previous documentation pass (v0.0.85, #2289) is the model for the amount and style of change.

## Out of Scope *(optional)*

- Any code, stage configuration, skill or behavior change.
- Documenting features that are not in v0.0.86.
- Reorganizing or rewriting the User Guide beyond what v0.0.86 requires.
- Back-filling documentation gaps from earlier releases, unless found while auditing a v0.0.86 topic. Those become follow-up issues.
- Release-note or changelog edits, and the blog or postmortem content.

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.86
- Previous pass: #2289 (PR #2290), documenting v0.0.85
- Release issues: #2298, #2297, #1557, #2296, #2295, #2293, #2294, #2302, #2301, #2299, #2280, #2292, #2300, #2230, #2303, #1209
- Repository documentation rule: regenerate `docs/llms-full.txt` after any canonical-page edit
