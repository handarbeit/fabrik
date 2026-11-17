# Feature Specification: Update docs for v0.0.93

**Feature Branch**: `fabrik/issue-2598`
**Created**: 2026-11-17
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.93.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.93

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Every release is followed by a documentation pass so a reader of the README, the User Guide and the marketing page sees the shipped behaviour rather than the previous release's. v0.0.93 (published 2026-11-17) is a user-facing release with four themes:

- **Review verdicts CI can wait on**: the opt-in `review_status` setting publishes a `fabrik/review` commit status (`pending`/`success`/`failure`) on each PR head. It needs the App's `statuses: write` permission only when enabled, and while it is pending the CI wait clocks are attributed to the review wait ("review wait timeout" backstop pause) (#2471).
- **Fewer needless re-checks with webhooks**: admission cooldowns are classified. While webhook ingestion is reliable for a repo, the periodic safety-net re-evaluation runs every 30 minutes instead of every ten poll intervals (`periodic_reeval_interval`, `0` = auto), and deadline timers wake the poll exactly when due (#2532). The release notes call this out as an upgrade note for webhook users.
- **Workspace on another disk**: `workspace_root` / `--workspace-root` / `FABRIK_WORKSPACE_ROOT` relocates worktrees, bare clones, sessions, logs and debug output. The root must already exist, and changing it while the previous root still holds worktrees is refused, not migrated (#2470).
- **Steadier review gate and merge train**: a dismissed review no longer hides the same author's later `COMMENTED` review (#2588); a zero-check trial or landing is held while member PRs show the repository runs CI (#2593); a conflict resolved while assembling a multi-member trial is written back to the member's branch (#2572).

Smaller items in the release: a webhook receipt time is no longer written back as GitHub's `updatedAt` (#2573), every engine log line ends with exactly one line ending (#2583), the PM plugin's version-check hook names the side that is behind (#2584), and a CI check guards against a PR that would auto-close a community report (#2576).

A survey of the current tree shows much of this is already in `docs/USER_GUIDE.md`, written by the feature PRs themselves (the `review_status` section and permission note, the Workspace Root section, the `--periodic-reeval-interval` and `--workspace-root` flag rows, the write-back bullet in the merge-train section). The remaining work is therefore a reconcile pass: add what is missing, correct what is now stale, and verify what is already there against the release notes. `docs/llms-full.txt` currently regenerates byte-identically, so it is in sync before any edit.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - README and marketing page announce v0.0.93 (Priority: P1)

A prospective or existing user reads the README and the marketing page and learns what v0.0.93 added and whether upgrading requires anything.

**Why this priority**: These are the two places a user reads about a release, and neither mentions v0.0.93. The README's highlights stop at v0.0.92 and `docs/index.md`'s "New in" line still says v0.0.92.

**Independent Test**: Search both files for "v0.0.93" and read the new text against the release notes; every highlight links to the User Guide section that documents it.

**Acceptance Scenarios**:

1. **Given** the README highlights block, **When** a reader looks for the latest release, **Then** a "v0.0.93 highlights" entry follows the v0.0.92 one, in the same style, covering the four themes and linking to the relevant User Guide sections.
2. **Given** that entry, **When** a webhook user reads its upgrade note, **Then** it says the periodic re-evaluation is now 30 minutes while delivery is healthy, names `periodic_reeval_interval` as the way to choose a cadence, and says poll-only instances are unchanged.
3. **Given** `docs/index.md`, **When** a reader reads the "New in" line, **Then** it names v0.0.93 and the v0.0.92 text is demoted to a parenthetical, as v0.0.91 was before it.

---

### User Story 2 - User Guide matches v0.0.93 behaviour (Priority: P1)

A user configuring Fabrik reads the User Guide and finds the v0.0.93 settings and behaviour correct and complete, with no statement that v0.0.93 made false.

**Why this priority**: The User Guide is the authoritative user-facing reference, and stale statements in it mislead operators.

**Independent Test**: For each v0.0.93 release-note item, locate its User Guide coverage and confirm it matches; confirm no remaining text contradicts the release.

**Acceptance Scenarios**:

1. **Given** the merge-train section's "Known issue: trial verdict timing on a quiet daemon" paragraph, which says a fix is "planned for v0.0.92 (#2542)", **When** the guide is reconciled, **Then** the stale known issue is removed or rewritten as resolved, because the fix shipped in v0.0.92.
2. **Given** the zero-check hold (#2593), **When** a reader looks in the merge-train/CI sections, **Then** the guide states that a trial or landing showing zero check runs is held while member PRs show the repository runs CI, that it is bounded by the CI backstop timeout, and that it costs nothing against the member (uncharged).
3. **Given** the dismissed-review fix (#2588), **When** a reader looks in the review gate section, **Then** the guide states that a dismissed review no longer hides the same author's later `COMMENTED` review from the gate.
4. **Given** the dependency-detection description that says blocked items are re-evaluated "every `10 × poll interval`", **When** it is read alongside the periodic re-evaluation text, **Then** it is consistent with it for webhook-healthy instances (event-driven, with the safety-net interval as backstop), and unchanged for poll-only instances.
5. **Given** the existing `review_status`, `workspace_root`, `periodic_reeval_interval` and write-back text, **When** checked against the release notes, **Then** names, defaults, permission requirements and refusal behaviour agree, with any discrepancy corrected.
6. **Given** the config examples and reference tables (annotated YAML example, flag table, environment-variable table), **When** checked, **Then** every v0.0.93 setting appears in all three places with correct defaults.

---

### User Story 3 - Generated bundle and indexes stay consistent (Priority: P2)

A maintainer or an LLM consumer of the docs gets a bundle that matches the canonical pages, and CI's docs-drift check passes first time.

**Why this priority**: CLAUDE.md requires regenerating `docs/llms-full.txt` in the same commit as any canonical-page edit; skipping it fails CI.

**Independent Test**: After all edits, `bash scripts/generate-llms-full.sh` produces no diff.

**Acceptance Scenarios**:

1. **Given** edits to `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md`, **When** the work is committed, **Then** `docs/llms-full.txt` is regenerated in the same commit and the regeneration is a no-op afterwards.
2. **Given** `docs/llms.txt` and the other docs pages that name releases or features, **When** reviewed, **Then** they are updated only where they would otherwise be wrong or misleading.

---

### Edge Cases

- A v0.0.93 item that is already fully and correctly documented is left alone; the task is not to rewrite working text.
- Historical statements about earlier releases (for example the v0.0.91 README highlight, or "removed in v0.0.92" wording) stay as they are unless they are factually wrong about the present. A forward-looking statement that has since come true or been resolved is corrected.
- Internal-only changes (gate-harness fixes, dependency bumps, plugin pinning, the closing-refs CI guard, the log line-ending fix, the `updatedAt` reconcile-drift fix) have no user-facing behaviour to document. They are not added to the README or marketing page, and the User Guide is touched only where it already describes the affected behaviour.
- Release-note claims are checked against the shipped behaviour in the User Guide and the as-built docs (`docs/state-machine.md`), not copied blindly; where they differ, the as-built behaviour wins and the difference is noted in the PR.
- The marketing page's examples and the config reference must not name settings or values that no longer exist.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The README MUST gain a "v0.0.93 highlights" entry after the v0.0.92 one, in the same format, covering the `fabrik/review` commit status (`review_status`), the 30-minute periodic re-evaluation under healthy webhooks (`periodic_reeval_interval`), the configurable workspace root (`workspace_root`), and the review-gate and merge-train fixes, each linked to its User Guide section.
- **FR-002**: The v0.0.93 README entry MUST include an "Upgrading" note stating the webhook cadence change and how to override it, that poll-only setups are unchanged, and that `review_status` and `workspace_root` are opt-in with no required configuration change.
- **FR-003**: `docs/index.md`'s "New in" line MUST name v0.0.93 and its headline features, with the v0.0.92 text kept as a parenthetical in the manner the v0.0.91 text was.
- **FR-004**: The User Guide's merge-train "Known issue: trial verdict timing on a quiet daemon" paragraph MUST be removed or rewritten as resolved, since #2542 shipped in v0.0.92.
- **FR-005**: The User Guide MUST document the zero-check hold: a trial or landing with zero check runs is held while member PRs show the repository runs CI, bounded by the CI backstop timeout, and not charged against the member.
- **FR-006**: The User Guide MUST state that a dismissed review no longer hides the same author's later `COMMENTED` review from the review gate.
- **FR-007**: The User Guide's description of blocked-item re-evaluation cadence MUST be consistent with the classified-cooldown behaviour (event-driven while ingestion is reliable, safety-net interval otherwise).
- **FR-008**: The User Guide's text for `review_status`, `workspace_root`, `periodic_reeval_interval` and the multi-member write-back MUST be verified against the v0.0.93 release notes, and any discrepancy in name, default, permission requirement or refusal behaviour MUST be corrected.
- **FR-009**: Every v0.0.93 configuration setting (`review_status` at stage and config level, `workspace_root`/`--workspace-root`/`FABRIK_WORKSPACE_ROOT`, `periodic_reeval_interval`/`--periodic-reeval-interval`/`FABRIK_PERIODIC_REEVAL_INTERVAL`) MUST appear with correct defaults in the annotated config example, the flag table and the environment-variable table of the User Guide, and in the README's flag table where the README lists comparable settings.
- **FR-010**: Code examples and configuration references in the edited pages MUST not name settings, flags or values that no longer exist.
- **FR-011**: `docs/llms-full.txt` MUST be regenerated with `bash scripts/generate-llms-full.sh` and committed in the same commit as any edit to `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md` or `docs/positioning.md`.
- **FR-012**: `docs/state-machine.md`, `docs/llms.txt` and other pages MUST be changed only where they would otherwise state something false about v0.0.93; no unrelated rewrites.

### Key Entities *(if applicable)*

- **Canonical doc pages**: `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md` — their edits trigger regeneration of the bundle.
- **Release notes (v0.0.93)**: the source of truth for what changed; checked against as-built docs.
- **`docs/llms-full.txt`**: generated bundle of the canonical pages, checked by the `docs-drift` workflow.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every user-facing v0.0.93 release-note item (features, fixes and improvements) is traceable to current text in the User Guide, the README or both, and none is missing.
- **SC-002**: No remaining text in the README, `docs/index.md` or the User Guide describes #2542 as planned or pending.
- **SC-003**: `bash scripts/generate-llms-full.sh` produces no diff after the final commit, so the `docs-drift` check passes on the first push.
- **SC-004**: The README and `docs/index.md` each name v0.0.93 as the latest release, and every link they add resolves to an existing User Guide anchor.

## Assumptions

- The User Guide's existing coverage of `review_status`, `workspace_root`, `periodic_reeval_interval` and the write-back is sufficient in outline, so the work there is verification and targeted fixes, not new sections.
- Internal-only items (CI closing-reference guard, log line endings, `updatedAt` drift fix, plugin pinning, gate/test fixes, dependency bumps) need no new user-facing documentation.
- The PM plugin version-check wording (#2584) is already reflected in the User Guide's plugin section ("naming the side that is behind"), and only a verification is needed.
- The README highlights follow the existing one-paragraph-per-release style rather than a restructure.

## Out of Scope *(optional)*

- Documenting internal-only changes (gate harness, dependency bumps, CI guard for community-report closing keywords, line-ending normalisation).
- Rewriting or restructuring existing documentation beyond what v0.0.93 makes stale or missing.
- Changing engine behaviour, defaults or any non-documentation code.
- Blog posts, postmortems and the competitive pages.

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.93
- Prior release docs pass for format and scope: commit "docs: update README, marketing page, Pruefer overview and User Guide for v0.0.92"
- CLAUDE.md "Documentation bundle (docs/llms-full.txt)"
