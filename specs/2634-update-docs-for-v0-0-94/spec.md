# Feature Specification: Update docs for v0.0.94

**Feature Branch**: `fabrik/issue-2634`
**Created**: 2026-11-27
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.94.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.94

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)

## Labels

documentation, fabrik:yolo"

## Background

Fabrik ships a release roughly every few days, and the user-facing documentation has to describe what a user can now see and do on the newest release. Every release is followed by a documentation-update issue of this kind (the last was #2598 for v0.0.93). It added a "highlights" note to `README.md`, a "New in" line on the marketing page `docs/index.md`, and edits to the affected `docs/USER_GUIDE.md` sections. It also regenerated the `docs/llms-full.txt` bundle.

v0.0.94 (published 2026-11-27) changed behavior in these areas, per its release notes:

- **Merge train**: the singleton catch-up's busy check now ignores Fabrik's own scratch files and clears stale `MERGE_HEAD`/`REBASE_HEAD`/`CHERRY_PICK_HEAD` refs. A worktree deferral that does not clear is bounded: after two consecutive deferrals with no worker, the item is rerouted off `Queued` and paused. The deferral is visible in the partition's `idle_reason`.
- **CI classification**: a check run cancelled by fail-fast whose Actions run contains a failed step is treated as a failure everywhere CI is judged (merge-train trial, landing, catch-up, the stage CI gate and the CI-fix comment). The train therefore re-runs or ejects instead of abandoning the batch as an infrastructure problem.
- **Merge gate**: `fabrik:rebase-needed` is cleared while CI is pending on a head whose mergeability is computed and conflict-free, and the wait reason says the rebase was pushed.
- **Spawn**: a child issue created by hand in a repo the instance does not serve, with the spawned-child marker already on the parent, resumes the parent's spawn instead of pausing it.
- **Board repair on GHES**: `repair-board --apply` and `init --create-board` replace Status options without option IDs on servers that do not accept them, only when no item has a Status. On a populated board they refuse, name the missing columns and give the UI remedy. This avoids clearing every item's Status.
- **Done cleanup**: a closed item whose linked PR merged gets its missing gate-checked completion labels filled before the worktree is removed, including when the item was paused.
- **Attention**: live worker turns count as progress, so a long healthy run is no longer reported as stalled. The stalled summary now names the time since the last turn.
- **Pruefer**: the review clone authenticates through the git environment (an origin-scoped `extraHeader`) and never through a token-bearing URL, and git log lines redact the credential.

The remaining release items (plugin pinning, e2e gate coverage, test flake fixes) are internal and not user-facing.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator learns what changed in v0.0.94 (Priority: P1)

An operator running or considering v0.0.94 opens the README and the marketing page and sees what is new in this release, how it affects them, and whether any action is needed on upgrade.

**Why this priority**: These two pages are the first place users look for release news. Without the update, the docs stop at v0.0.93.

**Independent Test**: Read the README highlights and the `docs/index.md` "New in" line without opening any other file, and confirm they cover the user-visible v0.0.94 changes and state the upgrade impact.

**Acceptance Scenarios**:

1. **Given** the README highlights list ends at v0.0.93, **When** the update is applied, **Then** a v0.0.94 highlights entry follows it in the same style, grouping the changes into themes and stating that no configuration change is needed to upgrade.
2. **Given** `docs/index.md` carries a "New in v0.0.93" line, **When** the update is applied, **Then** it carries a "New in v0.0.94" line summarizing the release, with links to the relevant User Guide anchors, and the v0.0.93 line is condensed into the parenthetical of earlier releases as in earlier updates.

---

### User Story 2 - Operator finds the changed behavior in the User Guide (Priority: P1)

An operator who hits one of the changed situations (a merge-train member that will not catch up, a cancelled CI check, a lingering `fabrik:rebase-needed`, a hand-made spawned child, a GHES `repair-board` refusal, a long-running worker, a paused closed item at Done) looks it up in the User Guide and finds an accurate description of the v0.0.94 behavior.

**Why this priority**: The User Guide is the reference. A description that contradicts the shipped behavior misleads operators.

**Independent Test**: For each v0.0.94 behavior change, locate the User Guide section that covers the affected feature and confirm its text matches the release notes. Confirm no passage still states the old behavior.

**Acceptance Scenarios**:

1. **Given** the Merge Train / Queued section describes catch-up deferral, **When** the update is applied, **Then** it states that Fabrik's own scratch files and stale operation refs no longer block the catch-up, that a deferral is bounded at two consecutive with no worker (reroute off `Queued` and pause), and that it appears in `idle_reason`.
2. **Given** the docs describe how CI results are judged, **When** the update is applied, **Then** they state that a fail-fast-cancelled check run with a failed step is treated as red, with the failing job and step named where the docs already show failing-check detail.
3. **Given** the docs describe `fabrik:rebase-needed`, **When** the update is applied, **Then** they state that the label is cleared while CI is pending on a conflict-free head and that the wait reason reads that the rebase was pushed.
4. **Given** the docs describe spawned children and the board repair commands, **When** the update is applied, **Then** they cover the hand-created cross-repo child behavior and the GHES option-ID behavior and refusal remedy.
5. **Given** the docs describe Done cleanup and the attention/stalled classification, **When** the update is applied, **Then** they cover the filled completion labels on a closed item with a merged PR (including a paused one) and live turns counting as progress.

---

### User Story 3 - Pruefer operators see the credential hardening (Priority: P2)

An operator who runs Pruefer learns that the review clone's token no longer appears on git's command line.

**Why this priority**: It is a security-relevant behavior that operators may want to cite, but it has no configuration surface.

**Independent Test**: Search the Pruefer docs for the review-clone authentication description and confirm it matches the release notes.

**Acceptance Scenarios**:

1. **Given** the Pruefer docs describe how the review clone is authenticated, **When** the update is applied, **Then** they state that authentication travels in the git environment as an origin-scoped header and that logged git output is redacted, and no passage still says the token is embedded in the clone URL.

---

### User Story 4 - Agents and bundles stay in sync (Priority: P2)

A consumer of the `docs/llms-full.txt` bundle, and CI's `docs-drift` check, see a bundle that matches the canonical pages.

**Why this priority**: CI fails the PR if the bundle drifts from its sources.

**Independent Test**: Run the bundle generator and confirm it produces no diff against the committed `docs/llms-full.txt`.

**Acceptance Scenarios**:

1. **Given** any canonical doc page (`docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`) was edited, **When** the change is committed, **Then** `docs/llms-full.txt` is regenerated in the same commit and the `docs-drift` check passes.

---

### Edge Cases

- A v0.0.94 change has no user-visible surface (internal plugin pinning, e2e gate coverage, test flake fixes): it is not documented.
- A v0.0.94 behavior is already described correctly in the docs, for example because an earlier change included its documentation: leave that text as is, and add nothing.
- A passage elsewhere in the docs describes pre-v0.0.94 behavior that v0.0.94 contradicts (for example a statement that the catch-up defers while any untracked file exists, or that a cancelled check is infrastructure): it is corrected, not only supplemented.
- Statements such as "known issue", "since v0.0.9x" or version-specific upgrade notes stay accurate. Nothing is marked fixed that the release notes do not say is fixed.
- Anchor links added to the README or marketing page point at headings that exist in the User Guide.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `README.md` MUST gain a v0.0.94 highlights entry after the v0.0.93 one, in the same format: themes, any known issue, and an "Upgrading" statement.
- **FR-002**: The v0.0.94 README entry MUST state that upgrading needs no configuration change. It MUST mention the GHES `repair-board`/`init --create-board` refusal, which is the one behavior an operator on such a server can newly meet.
- **FR-003**: `docs/index.md` MUST gain a "New in v0.0.94" line summarizing the release with links to the User Guide anchors for the described behaviors, and the prior "New in v0.0.93" content MUST be folded into the existing earlier-releases parenthetical rather than dropped.
- **FR-004**: `docs/USER_GUIDE.md` MUST describe the bounded, scratch-tolerant singleton catch-up deferral, including the two-deferral pause and its appearance in the partition `idle_reason`.
- **FR-005**: `docs/USER_GUIDE.md` MUST state that a fail-fast-cancelled check run with a failed step counts as a CI failure wherever Fabrik judges CI, and that other cancelled, stale or empty conclusions are still not treated as a pass.
- **FR-006**: `docs/USER_GUIDE.md` MUST state when `fabrik:rebase-needed` is cleared (CI pending on a conflict-free head) and what the wait reason says.
- **FR-007**: `docs/USER_GUIDE.md` MUST describe that a spawned-child marker is honored before the board-servability check, so a hand-created child in an unserved repo resumes the parent, and MUST name the manual path for a repo this instance does not serve.
- **FR-008**: `docs/USER_GUIDE.md` MUST describe the `repair-board --apply` and `init --create-board` behavior on servers whose option input has no `id`: name-only replacement only when no item has a Status, otherwise a refusal that names the missing columns and the UI remedy.
- **FR-009**: `docs/USER_GUIDE.md` MUST describe that Done cleanup of a closed item with a merged linked PR fills missing gate-checked completion labels first, including when the item is paused.
- **FR-010**: Documentation of the stalled/attention classification MUST state that live worker turns count as progress and that the stalled summary names the time since the last turn.
- **FR-011**: The Pruefer documentation MUST state that the review clone authenticates through the git environment with an origin-scoped header, never a token-bearing URL, with credential redaction in git log output.
- **FR-012**: Configuration references, flags and code examples touched by the above MUST be accurate for v0.0.94. No new flags or config keys are introduced by v0.0.94, so none are to be added.
- **FR-013**: Where an existing passage in any updated page contradicts v0.0.94 behavior, it MUST be corrected.
- **FR-014**: `docs/state-machine.md`, `docs/stage-lifecycle.md` and `docs/troubleshooting.md` MUST be checked for passages that contradict v0.0.94 behavior and corrected only where one exists. As-built engine behavior beyond that is already recorded there by the originating issues.
- **FR-015**: `docs/llms-full.txt` MUST be regenerated with `scripts/generate-llms-full.sh` and committed in the same commit as any canonical-doc edit.
- **FR-016**: Internal-only release items (plugin pinning, e2e gate coverage, test flake fixes) MUST NOT appear in user-facing docs.

### Key Entities *(if applicable)*

- **Release highlights entry**: the per-release note in `README.md`, with its counterpart "New in" line on `docs/index.md`.
- **Canonical doc pages**: `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`, whose edits require regenerating `docs/llms-full.txt`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every user-visible item in the v0.0.94 release notes (Fixes and Improvements) is findable in the docs by an operator, in the README entry and in at least one User Guide or Pruefer section.
- **SC-002**: No passage in the updated docs describes a pre-v0.0.94 behavior that v0.0.94 changed.
- **SC-003**: The `docs-drift` CI check passes on the first push, i.e. regenerating `docs/llms-full.txt` yields zero diff.
- **SC-004**: Every new internal link and anchor added in this update resolves to an existing heading.
- **SC-005**: The README and `docs/index.md` updates read in the same style and length as the v0.0.93 ones.

## Assumptions

- The release notes at `https://github.com/handarbeit/fabrik/releases/tag/v0.0.94` are the source of truth for what changed. Where a detail is needed, the originating issues (#2615, #2614, #2613, #2612, #2616, #2628, #2604, #2622) are the reference.
- The release introduces no new flags, environment variables, config keys or stage keys, so no configuration reference table needs new rows.
- "Marketing site" means `docs/index.md` (named in the Scope), plus `docs/pruefer.md` and `docs/companion-tools.md` only if they contain a passage v0.0.94 makes inaccurate.
- The update follows the pattern of the v0.0.93 documentation update (#2598).
- The "Documentation updated for v0.0.93 (#2598)" item in the release notes needs no new documentation.
- The README entry may list the known issues the release notes do not include as none, rather than inventing any.

## Out of Scope *(optional)*

- Documenting internal-only changes (plugin pinning, e2e gate coverage, test and sim flake fixes).
- Changing any code, stage config or behavior; this issue changes documentation only.
- Restructuring or rewriting the docs beyond what v0.0.94 requires.
- Documentation for releases after v0.0.94.
- Editing `adrs/` or the architecture list in `CLAUDE.md`.

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.94
- Previous documentation update: #2598 (v0.0.93), commit "docs: update README, marketing page and User Guide for v0.0.93"
- Originating issues named in the release notes: #2615, #2614, #2613, #2612, #2616, #2628, #2604, #2622 (reports: #2607, #2609, #2608, #2601, #2610)
