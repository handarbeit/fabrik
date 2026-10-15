# Feature Specification: Update docs for v0.0.85

**Feature Branch**: `fabrik/issue-2289`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.85. See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.85 — Scope: USER_GUIDE.md — update sections affected by new features or changed behavior; README.md — update feature list if new user-facing capabilities were added; docs/index.md — update marketing page if warranted; Ensure code examples and configuration references are current; Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Fabrik's user-facing documentation must describe what the current release does. v0.0.85 changes a few behaviours operators can observe, and several of those changes are not yet in the docs. Without the update, an operator following the docs would be surprised in these ways: an item with a pending comment now gets a stage run before the comment pass, a stale `fabrik:awaiting-input` label is now cleared automatically, and the README's "highlights" paragraph still stops at v0.0.84.

The v0.0.85 release notes list these user-visible changes:

- **Stage runs before a pending comment (#2220).** A comment posted before an item reaches a stage used to go straight to the comment skill and could complete the stage on its own, so Specify, Research, Plan or Implement's draft PR could be skipped. The stage now runs first, then one comment pass applies the comment. The advance (including a `FABRIK_NO_WORK_NEEDED` move to Done) waits for that comment pass. This costs one extra invocation.
- **SIGHUP is a graceful restart (#2259).** The first SIGHUP or `ctrl+r` drains in-flight workers, and a second restarts now.
- **TUI waiting rows (#2238).** The In Progress panel lists items held on known gates, and the header reads `waiting on N item(s)` instead of "nothing to do".
- **Merge-train Queued order (#2246)** follows GitHub's own move time.
- **Stale `fabrik:awaiting-input` is cleared (#1970)** after every comment pass and reinvoke, and whenever a live read shows `fabrik:paused` is absent. A failed `fabrik:paused` removal on resume now leaves the comment unprocessed, so the next poll retries.
- **Usage-limit exit on a resumed session (#1949)** is classified as a usage limit, not a resume failure.
- **Already-landed origin issue branch is not adopted (#1917).** Fabrik forks fresh from base instead, with one log line, and logs `warn: base mismatch` when an open PR targets a different base.
- **`+dirty` dev builds rebuild (#2234)** when the checkout moves, and each upgrade check logs one outcome line. This applies to Pruefer dev builds too.
- **CI skips Go suites when no Go build input changes (#2254)**, and Linux `-race` runs the sim at higher parallelism (#2228).
- **Live gate on two beds (#1990).**

A survey of the current docs shows that #2238, #2246 and #2259 are already documented (TUI waiting rows, GitHub move-time ordering, the two-step SIGHUP drain, the `ctrl+r` row). #2220, #1970, #1949, #1917, #2234 and #2254 are not mentioned, and neither README.md nor docs/index.md carries a v0.0.85 summary. This is a documentation-only change with no code changes.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator understands comment-before-stage behaviour (Priority: P1)

An operator who comments on an item while it sits in an earlier column (for example Backlog) wants to know what Fabrik will do when the item reaches a stage.

**Why this priority**: It is the only v0.0.85 behaviour change that alters the invocation sequence an operator sees (one extra invocation), and it is entirely undocumented.

**Independent Test**: Read the User Guide section on comment processing and the stage lifecycle. Without any other source, state the order of events for an item that has a pending comment on entry to a stage, including the `FABRIK_NO_WORK_NEEDED` case.

**Acceptance Scenarios**:

1. **Given** an item with an unprocessed comment entering a stage for the first time, **When** the reader consults the guide, **Then** it states the stage runs first and one comment pass follows once the stage has produced its output.
2. **Given** a stage that reports `FABRIK_NO_WORK_NEEDED` with a comment pending, **When** the reader consults the guide, **Then** it states the item is marked complete but does not move to Done until the comment pass has run.
3. **Given** an incomplete stage run with a pending comment, **When** the reader consults the guide, **Then** it states the stage run is retried under its normal backoff.

---

### User Story 2 - Operator understands pause-label and resume behaviour changes (Priority: P1)

An operator who resumes a paused item by removing only `fabrik:paused`, or who sees `fabrik:awaiting-input` left behind, wants to know what Fabrik does now.

**Why this priority**: Pause-label guidance is operator-facing and the docs currently describe the pre-#1970 behaviour implicitly.

**Independent Test**: Read the label reference and pause/resume documentation, and confirm the clearing rule and the failed-removal behaviour are described.

**Acceptance Scenarios**:

1. **Given** an item with `fabrik:awaiting-input` but no `fabrik:paused`, **When** the reader consults the label reference, **Then** it states Fabrik removes the stale label after every comment pass and reinvoke, and when processing an item with a live read showing `fabrik:paused` absent.
2. **Given** a failed `fabrik:paused` removal on resume, **When** the reader consults the guide, **Then** it states the comment is left unprocessed and retried next poll.
3. **Given** a real pause, **When** the reader consults the guide, **Then** it states the pause is never lifted by this clearing.

---

### User Story 3 - Reader of the README and site sees what is new in v0.0.85 (Priority: P2)

A prospective or existing user reading the README or the marketing page wants a short summary of this release, consistent with how v0.0.84 was summarised.

**Why this priority**: Discoverability, but no behavioural dependency.

**Independent Test**: Open README.md and docs/index.md and find a v0.0.85 highlights note linking to the relevant User Guide sections.

**Acceptance Scenarios**:

1. **Given** the README highlights paragraph, **When** the reader reads it, **Then** it covers v0.0.85 (stage-before-comment, graceful SIGHUP, TUI waiting rows) with links to the User Guide.
2. **Given** the marketing page, **When** the reader reads its "new in" note, **Then** it is current for v0.0.85 or deliberately left unchanged because nothing in the release warrants marketing copy.

---

### User Story 4 - Operator understands smaller engine fixes (Priority: P3)

An operator or contributor wants the remaining engine and CI changes documented where a related topic already exists.

**Why this priority**: Lower operator impact; mostly log lines and CI behaviour.

**Independent Test**: Search the User Guide for each of the following and find a matching, accurate statement: resumed-session usage-limit classification, landed-origin-branch handling (including the `warn: base mismatch` line), `+dirty` dev-build rebuilds and per-check upgrade log line, and CI's skip of Go suites for non-Go-input PRs.

**Acceptance Scenarios**:

1. **Given** the usage-limit section, **When** read, **Then** it says a resumed-session limit exit is a usage limit and leaves the session and resume-failure counter alone.
2. **Given** the worktree or branch-adoption documentation, **When** read, **Then** it says an origin issue branch already on base, or equal to the head of a merged PR for the item, is not adopted.
3. **Given** the self-upgrade documentation, **When** read, **Then** it says a `+dirty` dev build rebuilds when its checkout moves (not when the build is at HEAD) and each check logs one outcome line.
4. **Given** the contributor-facing CI description, **When** read, **Then** it says PRs whose every changed path is on the skippable list skip the Go suites yet still report success.

---

### Edge Cases

- A topic already correctly documented (SIGHUP drain, TUI waiting rows, Queued ordering) must not be duplicated or contradicted; verify and leave as is.
- Where a topic appears in several places (README, User Guide, docs/index.md, docs/faq.md), they must not disagree.
- Release-notes details that are internal (ADR numbers, e2e gate figures, plugin pinning) are not operator documentation and need not be copied.
- The release was published on the day this spec was written; the docs refer to the version as v0.0.85 and do not state a publication date.
- Edits to any canonical page (`docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`) require regenerating `docs/llms-full.txt` in the same change.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The User Guide MUST describe that a stage runs before any comment that was pending when the item entered it, followed by one comment pass, including the extra invocation this implies.
- **FR-002**: The User Guide MUST state that the advance is held until the comment pass has run, including the `FABRIK_NO_WORK_NEEDED` case (marked complete, not moved to Done until then).
- **FR-003**: The User Guide MUST state that an incomplete stage run with a pending comment is retried as a stage run under its normal backoff.
- **FR-004**: The User Guide MUST describe automatic clearing of a stale `fabrik:awaiting-input` (after every comment pass and reinvoke, and when a live read shows `fabrik:paused` absent) and that a real pause is never lifted this way.
- **FR-005**: The User Guide MUST state that a failed `fabrik:paused` removal on resume leaves the comment unprocessed and is retried on the next poll.
- **FR-006**: The User Guide MUST state that a usage-limit exit on a resumed session is classified as a usage limit and does not touch the session or the resume-failure counter.
- **FR-007**: The User Guide MUST state that an origin issue branch already on base, or equal to the head of a merged PR for the item, is not adopted into a new worktree (a fresh fork from base is used, with one log line), and that adopting a branch whose open PR targets a different base logs `warn: base mismatch`.
- **FR-008**: The User Guide MUST state that a `+dirty` dev build is rebuilt when its checkout moves, is not rebuilt when it is at the same commit as HEAD, and that each upgrade check logs one outcome line.
- **FR-009**: The documentation MUST describe the CI skip-Go-suites-when-no-Go-build-input-changed behaviour in the place contributor-facing CI behaviour is described, if such a place exists; otherwise this is omitted rather than invented.
- **FR-010**: README.md MUST carry a v0.0.85 highlights note (replacing or extending the v0.0.84 one) that covers stage-before-comment ordering, the graceful SIGHUP restart and the TUI waiting rows, with links into the User Guide, and MUST mention that no configuration changes are needed to upgrade.
- **FR-011**: The README's SIGHUP description and docs/index.md's SIGHUP sentence MUST be verified as consistent with the first-SIGHUP-drains, second-SIGHUP-restarts-now behaviour and corrected if not.
- **FR-012**: docs/index.md MUST be updated only if the release warrants marketing copy; its "New in" note MUST NOT be left naming a version that no longer describes the latest release's headline without a deliberate decision recorded in the PR.
- **FR-013**: Already-documented v0.0.85 items (#2238, #2246, #2259) MUST be verified against the release notes and corrected only where they differ.
- **FR-014**: Other places that describe the same topics (docs/faq.md, docs/stage-lifecycle.md, docs/state-machine.md) MUST be checked for statements that contradict the new behaviour (for example, a comment pass running before the stage, or "remove only `fabrik:paused`" guidance that implies a leftover label) and corrected.
- **FR-015**: Code examples and configuration references touched by the update MUST be current for v0.0.85 (no new configuration keys were introduced; none may be invented).
- **FR-016**: `docs/llms-full.txt` MUST be regenerated with `scripts/generate-llms-full.sh` and committed in the same change as any canonical-doc edit.

### Key Entities *(if applicable)*

- **Canonical doc pages**: `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, `docs/positioning.md`. Editing any of these triggers the `docs/llms-full.txt` regeneration requirement.
- **Release notes**: the v0.0.85 GitHub release, the source of truth for what changed.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every user-visible change in the v0.0.85 release notes (the items listed in Background) is either found accurately documented in the docs or has been added, with no statement left that contradicts the release notes.
- **SC-002**: A search of the User Guide for the stage-before-comment behaviour returns a section a reader can follow to the correct sequence of events without reading the release notes.
- **SC-003**: README.md carries a v0.0.85 highlights note with working links to User Guide anchors.
- **SC-004**: The `docs-drift` check passes on the PR (committed `docs/llms-full.txt` equals the regenerated bundle).
- **SC-005**: No documentation page names a configuration key, flag or command that does not exist in v0.0.85.

## Assumptions

- No configuration, flag or environment-variable changes shipped in v0.0.85 (the release notes say none are needed), so no new reference tables are needed.
- ADR numbers and gate/e2e statistics from the release notes are internal and are not reproduced in operator docs, though existing ADR links may stay.
- Documentation already merged for #2238, #2246 and #2259 is presumed correct and is verified, not rewritten.
- If the marketing page has no natural place for these fixes, leaving `docs/index.md` unchanged (apart from fixing any contradiction per FR-011) is acceptable.
- The CI classifier behaviour is documented only where CI behaviour is already described for contributors; the spec does not require creating a new section for it.
- The version is referred to as v0.0.85 throughout, in the style of the existing v0.0.84 note.

## Out of Scope *(optional)*

- Code changes of any kind.
- Rewriting the docs structure or reformatting unrelated sections.
- Documenting the multi-bed live e2e gate (#1990) beyond what already exists in contributor documentation.
- Changes to ADRs, release notes or the v0.0.85 release itself.
- New tutorials, blog posts or diagrams.

## Source References *(optional)*

- v0.0.85 release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.85
- CLAUDE.md "Documentation bundle (docs/llms-full.txt)" requirement
